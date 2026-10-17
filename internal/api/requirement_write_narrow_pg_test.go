// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// narrowPG is a Postgres-backed server with SSO on, a member mount policy
// whose read roots hold the test directory, and a capturing runner.
type narrowPG struct {
	srv    *Server
	fr     *fakeRunner
	member *http.Cookie
}

func newNarrowPG(t *testing.T, root string) narrowPG {
	t.Helper()
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.OIDC = &oidc.Authenticator{}
	srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
	srv.router = srv.routes()
	return narrowPG{srv: srv, fr: fr, member: ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)}
}

// put writes a workspace named name on dir through POST /workspaces, or PUT
// /workspaces/{id} when id is set, as the member or as the operator, then
// stores the scan result a local_dir scan seeds on the shared source: an
// optional write:<dir> row.
func (h narrowPG) put(t *testing.T, member bool, id uuid.UUID, name, dir string, overrides map[string]string) uuid.UUID {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "sources": []types.WorkspaceSource{
		{Type: types.WorkspaceSourceTypeLocalDir, Path: dir, Overrides: overrides}}})
	method, path, want := http.MethodPost, "/api/v1/workspaces", http.StatusCreated
	if id != uuid.Nil {
		method, path, want = http.MethodPut, "/api/v1/workspaces/"+id.String(), http.StatusOK
	}
	rec := h.send(t, member, method, path, string(body))
	if rec.Code != want {
		t.Fatalf("%s %s: code = %d, want %d; body=%s", method, path, rec.Code, want, rec.Body.String())
	}
	var got workspaceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	srcs, err := h.srv.cfg.Store.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range srcs {
		if src.Kind == types.SourceLocalDir && src.Locator == dir {
			if _, err := h.srv.cfg.Store.SetSourceScanResultUnfenced(ctx, src.ID, []byte("{}"), types.WorkspaceScanned,
				seedSourceRequirements(types.SourceLocalDir, dir, workspacescan.WorkspaceProfile{})); err != nil {
				t.Fatal(err)
			}
		}
	}
	return got.ID
}

func (h narrowPG) send(t *testing.T, member bool, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	if member {
		return doSSO(t, h.srv, method, path, h.member, body)
	}
	return do(t, h.srv, method, path, adminToken, body)
}

// bind launches body as the member through POST /runs and returns the
// workspace bind's read-only flag on the sandbox the runner was handed.
func (h narrowPG) bind(t *testing.T, body string) bool {
	t.Helper()
	before := h.fr.createCount()
	if w := h.send(t, true, http.MethodPost, "/api/v1/runs", body); w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.fr.createCount() == before && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// A run that never reached the runner leaves the previous run's spec in lastSpec, which a control could pass on.
	if h.fr.createCount() == before {
		t.Fatal("the run never reached CreateSandbox within 5s")
	}
	h.fr.mu.Lock()
	spec := h.fr.lastSpec
	h.fr.mu.Unlock()
	return mountFor(t, spec, composerWorkspaceTarget).ReadOnly
}

func (f *fakeRunner) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

// TestPG_WriteRow_StoringWorkspaceDoesNotNarrow: an operator workspace O
// requires its own write on directory P. A member's workspace M on the same P
// is older, so the source index pulls it into a run that attaches only O, and
// M's write row is the scan's optional one, not enabled. M only stores P: it
// neither grants the write nor removes O's, so the bind is writable. M is
// written through the member's own POST, and in the second case brought back
// to the scan's optional row through PUT after it had required the write.
//
// Controls, on the same workspaces: a run that selects O read_only is
// read-only, and with O's row brought back to optional and not enabled, the
// attached O's own row narrows, as an attached workspace's row always has.
func TestPG_WriteRow_StoringWorkspaceDoesNotNarrow(t *testing.T) {
	for _, viaUpdate := range []bool{false, true} {
		name := "member workspace written by POST"
		if viaUpdate {
			name = "member workspace changed by PUT"
		}
		t.Run(name, func(t *testing.T) {
			root, project := memberProjectRoot(t)
			h := newNarrowPG(t, root)
			required := map[string]string{"write:" + project: types.OverrideRequired}
			optional := map[string]string{"write:" + project: types.OverrideOptional}
			mName := "m-" + uuid.NewString()[:8]
			var m uuid.UUID
			if viaUpdate {
				m = h.put(t, true, uuid.Nil, mName, project, required)
				h.put(t, true, m, mName, project, optional)
			} else {
				m = h.put(t, true, uuid.Nil, mName, project, nil)
			}
			if got, err := h.srv.cfg.Store.GetWorkspace(context.Background(), m); err != nil || got.EffectiveRequirements["write:"+project].Level != "optional" {
				t.Fatalf("the member workspace's hydrated contract = %v (%v), want write:%s optional", got.EffectiveRequirements, err, project)
			}
			oName := "o-" + uuid.NewString()[:8]
			o := h.put(t, false, uuid.Nil, oName, project, required)

			sel := func(ids ...string) string {
				return `{"agent":"claude-code","task":"do the thing","workspaces":[` + strings.Join(ids, ",") + `]}`
			}
			ws := func(id uuid.UUID, extra string) string { return `{"workspace_id":"` + id.String() + `"` + extra + `}` }

			if ro := h.bind(t, sel(ws(o, ""))); ro {
				t.Error("O attached, M only storing: the bind is read-only; M's not-enabled row removed O's own write")
			}
			if ro := h.bind(t, sel(ws(o, `,"read_only":true`))); !ro {
				t.Error("O attached read_only: the bind is writable")
			}
			h.put(t, false, o, oName, project, optional)
			if ro := h.bind(t, sel(ws(o, ""))); !ro {
				t.Error("O's own row optional and not enabled: the bind is writable")
			}
		})
	}
}
