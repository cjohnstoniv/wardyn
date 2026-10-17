// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const charsetRefusal = "must not contain control characters or whitespace"

// A repo's locator, ref and explicit target ride a tab-framed record and a git
// argument, so every door that authors one holds them to repoFieldSafe, whether
// or not the entry names a target. A space-bearing mount path is not a repo
// field and stays valid.
func TestRepoTransportCharset_PolicyWriteDoor(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec types.RunPolicySpec
		want string // "" = accepted
	}{
		{"a locator with whitespace and no target", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "acme/my app"}}}, "workspace_repos[0]: repo " + charsetRefusal},
		{"a ref with whitespace and no target", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "acme/app", Ref: "main branch"}}}, "workspace_repos[0]: ref " + charsetRefusal},
		{"a target with whitespace", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "acme/app", Target: "/home/agent/work/my app"}}}, "workspace_repos[0]: target " + charsetRefusal},
		{"the second of several repos", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{
			{Repo: "acme/one", Target: "/home/agent/work/one"}, {Repo: "acme/two", Target: "/home/agent/work/t\two"}}}, "workspace_repos[1]: target " + charsetRefusal},
		{"clean repos", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{
			{Repo: "acme/one", Ref: "main"}, {Repo: "acme/two", Target: "/home/agent/work/two"}}}, ""},
		{"a mount path with a space", types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/dev/my work", Target: "/home/agent/my work"}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicyWorkspaces(tc.spec, true)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestRepoTransportCharset_WorkspaceSourceDoor(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  types.WorkspaceSource
		want string
	}{
		{"a repo target with whitespace", types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: "acme/app", Target: "/home/agent/work/my app"}, "target " + charsetRefusal},
		{"a repo with a clean target", types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: "acme/app", Target: "/home/agent/work/app"}, ""},
		{"an ephemeral target with a space", types.WorkspaceSource{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/my scratch"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := validateWorkspaceSource(tc.src, nil)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("validateWorkspaceSource = %q, want %q", got, tc.want)
			}
		})
	}
}

// A stored source written before the door checked is refused when a run seeds it,
// as its other stored-target failures are, and not dropped at dispatch.
func TestRepoTransportCharset_StoredSourceSeeding(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	ws := types.Workspace{ID: id, Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "acme/app", Target: "/home/agent/work/my app"}}}
	srv := New(baseTestConfig(h, &workspaceStoreFake{ws: ws}))
	spec, req := types.RunPolicySpec{}, createRunRequest{Agent: "claude-code", WorkspaceID: &id}
	if _, _, code, reason, err := seedLegacy(srv, context.Background(), &spec, &req); err == nil || code != http.StatusUnprocessableEntity ||
		!strings.Contains(err.Error(), charsetRefusal) || reason != reasonWorkspaceSeedSourceTargetInvalid {
		t.Fatalf("seed = %d, %q, %v; want a 422 naming the charset rule", code, reason, err)
	}
}

// What dispatch still receives from a stored policy: a repo whose ref or target
// cannot ride the record is dropped (a 201 sentence for the target, the log for
// the rest), and when every structured record is dropped the run's own repo is
// not cloned at a default target and ref in its place.
func TestRepoTransportCharset_DispatchDrops(t *testing.T) {
	const url = "https://github.com/acme/app.git"
	repoWithSpace := types.WorkspaceRepo{Repo: "acme/app", Target: "/home/agent/work/my app"}
	for _, tc := range []struct {
		name       string
		legacy     string
		repos      []types.WorkspaceRepo
		wantRepos  []string // dest of each record kept
		wantLegacy bool
		wantWarn   string
	}{
		{"every structured record dropped, run repo named", "acme/app", []types.WorkspaceRepo{repoWithSpace}, nil, false, "was NOT cloned"},
		{"a dropped ref, run repo named", "acme/app", []types.WorkspaceRepo{{Repo: "acme/app", Ref: "a b"}}, nil, false, ""},
		{"one of several dropped", "acme/app", []types.WorkspaceRepo{repoWithSpace, {Repo: "acme/two", Target: "/home/agent/work/two"}}, []string{"/home/agent/work/two"}, true, "was NOT cloned"},
		{"a healthy workspace launch", "acme/app", []types.WorkspaceRepo{{Repo: "acme/app", Target: "/home/agent/work/app", Ref: "main"}}, []string{"/home/agent/work/app"}, true, ""},
		{"the run's own repo on a reserved target", "acme/app", []types.WorkspaceRepo{{Repo: "acme/app", Target: "/home/agent/drive"}}, nil, false, "was NOT cloned"},
		{"a legacy repo alone", "acme/app", nil, []string{"/home/agent/work/app"}, true, ""},
		{"a legacy repo beside a different structured one", "acme/app", []types.WorkspaceRepo{{Repo: "acme/two", Target: "/home/agent/work/two"}}, []string{"/home/agent/work/app", "/home/agent/work/two"}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			applyRepoCloneEnv(env, types.AgentRun{Repo: tc.legacy}, types.RunPolicySpec{WorkspaceRepos: tc.repos})
			var dests []string
			for _, rec := range strings.Split(env["WARDYN_REPOS"], "\n") {
				if f := strings.Split(rec, "\t"); len(f) > 1 {
					dests = append(dests, f[1])
				}
			}
			if strings.Join(dests, ",") != strings.Join(tc.wantRepos, ",") {
				t.Errorf("WARDYN_REPOS dests = %v, want %v", dests, tc.wantRepos)
			}
			if got := env["WARDYN_REPO_URL"] == url && env["WARDYN_REPO_SLUG"] == tc.legacy; got != tc.wantLegacy {
				t.Errorf("legacy WARDYN_REPO_URL and _SLUG set = %v, want %v (env %v)", got, tc.wantLegacy, env)
			}
			if _, warns := buildRepoRecords(tc.legacy, tc.repos); tc.wantWarn != "" && !strings.Contains(strings.Join(warns, "\n"), tc.wantWarn) {
				t.Errorf("warnings = %v, want one saying %q", warns, tc.wantWarn)
			}
		})
	}
}

// A run repo that names no clone URL has no record to withhold the legacy pair
// for: it keeps its slug (the withhold fires only when a dropped record named it).
func TestRepoTransportCharset_UnclonableRunRepoKeepsItsSlug(t *testing.T) {
	env := map[string]string{}
	applyRepoCloneEnv(env, types.AgentRun{Repo: "acme"}, types.RunPolicySpec{})
	if env["WARDYN_REPO_SLUG"] != "acme" || env["WARDYN_REPO_URL"] != "" || env["WARDYN_REPOS"] != "" {
		t.Errorf("env = %v, want only WARDYN_REPO_SLUG=acme", env)
	}
}

// The attachments Review shows list a clone only if dispatch will make it.
func TestRepoTransportCharset_CloneTargetIsHonest(t *testing.T) {
	for _, r := range []types.WorkspaceRepo{
		{Repo: "acme/app", Target: "/home/agent/work/my app"},
		{Repo: "acme/app", Ref: "main branch"},
		{Repo: "acme/my app"},
	} {
		if dest, ok := repoCloneTarget(r); ok {
			t.Errorf("repoCloneTarget(%+v) = %q, true; a clone dispatch drops is not shown", r, dest)
		}
	}
	if dest, ok := repoCloneTarget(types.WorkspaceRepo{Repo: "acme/app", Ref: "main"}); !ok || dest != "/home/agent/work/app" {
		t.Errorf("a clean repo = %q, %v", dest, ok)
	}
}

// The record flow wires a stored source without the policy write door, so it
// holds the repo's target to the same rule.
func TestRepoTransportCharset_RecordWiring(t *testing.T) {
	ws := types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "acme/app", Target: "/home/agent/work/my app"}}}
	if _, _, err := wireWorkspaceSource(&types.AgentRun{}, &types.RunPolicySpec{}, ws); !errors.Is(err, errWorkspaceSourceTarget) {
		t.Errorf("wireWorkspaceSource err = %v, want errWorkspaceSourceTarget", err)
	}
	ws.Sources = []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/my scratch"}}
	if _, _, err := wireWorkspaceSource(&types.AgentRun{}, &types.RunPolicySpec{}, ws); err != nil {
		t.Errorf("a scratch directory with a space: %v", err)
	}
}
