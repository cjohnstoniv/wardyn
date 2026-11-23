// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run that attaches a workspace binding a folder to a person's runner, and whose policy also names that
// folder as a mount, is checked by the folder's text at every door: what this server has at the same path is
// never read. The same run with the folder held on this server is refused at the seed, and a run that
// attaches no workspace keeps the host check on its inline and overlay mounts.
func TestRunCreate_PolicyMountOfARunnerBoundFolderReadsNoServerPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newPlacementEnv(t)
	e.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, pool.ID, laptop)
	advertiseRunnerRoots(t, e.srv, laptop, root)
	w := e.call(e.person, http.MethodPost, "/api/v1/workspaces", workspaceBody(t, "server-folder", types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: path}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create the unbound workspace = %d %s", w.Code, w.Body)
	}
	unbound := decodeJSON[types.Workspace](t, w)

	mount := `{"source":"` + path + `","target":"/home/agent/work","read_only":true}`
	stored := `{"name":"bound-folder","spec":{"min_confinement_class":"CC1","allowed_domains":["api.anthropic.com"],"workspace_mounts":[` + mount + `]}}`
	w = e.admin(http.MethodPost, "/api/v1/policies", stored)
	if w.Code != http.StatusCreated {
		t.Fatalf("store the policy = %d %s", w.Code, w.Body)
	}
	policy := decodeJSON[types.RunPolicy](t, w)

	// The path is a directory until now, so the save-time host check passed. From here this server has a
	// link into a denied folder at it, which the runner's own disk does not.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", path); err != nil {
		t.Fatal(err)
	}
	bound, _ := e.boundFolderWorkspace(e.person, "laptop-folder", path, laptop)

	attach := func(id string) string { return `"workspaces":[{"workspace_id":"` + id + `"}]` }
	const base = `"agent":"claude-code","task":"t"`
	inline := `"inline_policy":{"min_confinement_class":"CC1","allowed_domains":["api.anthropic.com"],"workspace_mounts":[` + mount + `]}`
	overlay := `"policy_overlay":{"workspace_mounts":[` + mount + `]}`
	onPool := `"runner_pool_id":"` + pool.ID.String() + `"`

	clean := func(name string, got map[string]*httptest.ResponseRecorder) {
		t.Helper()
		for door, w := range got {
			body := w.Body.String()
			for _, reason := range []string{"inline_policy_invalid", "policy_overlay_invalid", reasonWorkspaceSeedPolicyConflict} {
				if strings.Contains(body, `"`+reason+`"`) {
					t.Errorf("%s %s = %d %s, want the folder past stepPolicy and the seed", name, door, w.Code, body)
				}
			}
			if strings.Contains(body, "/etc") {
				t.Errorf("%s %s = %d %s, want no word of what this server has at %s", name, door, w.Code, body, path)
			}
		}
	}
	seedConflict := func(name string, got map[string]*httptest.ResponseRecorder) {
		t.Helper()
		if b := sameEverywhere(t, name, got, http.StatusUnprocessableEntity, reasonWorkspaceSeedPolicyConflict); !strings.Contains(b, "/etc") {
			t.Errorf("%s refusal %s, want it to name the denied path the server folder resolves to", name, b)
		}
	}
	hostRefusal := func(name string, got map[string]*httptest.ResponseRecorder, reason string) {
		t.Helper()
		if b := sameEverywhere(t, name, got, http.StatusBadRequest, reason); !strings.Contains(b, "/etc") {
			t.Errorf("%s refusal %s, want it to name the denied path the server folder resolves to", name, b)
		}
	}

	// stored policy: the member's own run, on the member's pool
	storedBody := func(ws string) string {
		return `{` + base + `,"policy_id":"` + policy.ID.String() + `",` + onPool + `,` + attach(ws) + `}`
	}
	storedBound := e.answers(e.person, storedBody(bound.ID.String()))
	for door, w := range storedBound {
		if w.Code >= http.StatusBadRequest {
			t.Errorf("stored, bound %s = %d %s, want the folder past seeding", door, w.Code, w.Body)
		}
	}
	clean("stored, bound", storedBound)
	seedConflict("stored, unbound", e.answers(e.person, storedBody(unbound.ID.String())))

	// inline and overlay policies, as an admin
	for _, arm := range []struct{ name, field, reason string }{
		{"inline", inline, "inline_policy_invalid"},
		{"overlay", overlay, "policy_overlay_invalid"},
	} {
		t.Logf("%s arm, admin doors:", arm.name)
		boundGot := e.answers(nil, `{`+base+`,`+arm.field+`,`+attach(bound.ID.String())+`}`)
		for door, w := range boundGot {
			t.Logf("  bound %s = %d %s", door, w.Code, w.Body)
		}
		clean(arm.name+", bound", boundGot)
		seedConflict(arm.name+", unbound", e.answers(nil, `{`+base+`,`+arm.field+`,`+attach(unbound.ID.String())+`}`))
		hostRefusal(arm.name+", no workspace", e.answers(nil, `{`+base+`,`+arm.field+`}`), arm.reason)
	}
}
