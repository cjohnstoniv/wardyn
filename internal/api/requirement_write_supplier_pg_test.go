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

// supplierPG is a Postgres-backed server with SSO on, a member mount policy
// with read roots and no writable roots, and a capturing runner.
type supplierPG struct {
	srv    *Server
	fr     *fakeRunner
	member *http.Cookie
}

func newSupplierPG(t *testing.T, root string) supplierPG {
	t.Helper()
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.OIDC = &oidc.Authenticator{}
	srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
	srv.router = srv.routes()
	return supplierPG{srv: srv, fr: fr, member: ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)}
}

// workspace creates a workspace on dir through POST /workspaces, as the member
// when member is set and as the operator otherwise, with overrides on its one
// source, then records the scan's own write:<dir> row on the shared source.
func (h supplierPG) workspace(t *testing.T, member bool, name, dir string, overrides map[string]string) uuid.UUID {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "sources": []types.WorkspaceSource{
		{Type: types.WorkspaceSourceTypeLocalDir, Path: dir, Overrides: overrides}}})
	w := h.send(t, member, http.MethodPost, "/api/v1/workspaces", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create workspace: code = %d; body=%s", w.Code, w.Body.String())
	}
	var got workspaceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	h.scanned(t, dir)
	return got.ID
}

// scanned stores the scan result a local_dir scan seeds: write:<dir> optional.
func (h supplierPG) scanned(t *testing.T, dir string) {
	t.Helper()
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
			return
		}
	}
	t.Fatalf("no source row for %s", dir)
}

func (h supplierPG) send(t *testing.T, member bool, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	if member {
		return doSSO(t, h.srv, method, path, h.member, body)
	}
	return do(t, h.srv, method, path, adminToken, body)
}

// launch is a run request attaching ws, read_only when readOnly is set.
func launch(ws uuid.UUID, readOnly bool) string {
	sel := `{"workspace_id":"` + ws.String() + `"`
	if readOnly {
		sel += `,"read_only":true`
	}
	return `{"agent":"claude-code","task":"do the thing","workspaces":[` + sel + `}]}`
}

var doorSuccess = map[string]int{"/api/v1/runs": http.StatusCreated, "/api/v1/runs/preflight": http.StatusOK, "/api/v1/runs/policy-preview": http.StatusOK}

// TestPG_RequirementWrite_MemberOverrideSupplierIsHeld: a member promotes the
// scan's optional write:<P> row to required on their own workspace M, by an
// override set through the member's own POST, or through PUT on a workspace
// created without one. An operator workspace O on the same P is created after
// M. The member attaches O only; the source index pulls M into the run (it
// keeps the oldest holder), and M's row is what would make P writable. P is
// outside the member's writable roots, so every door refuses, as it does when
// the member attaches M itself. A run that selects O read_only is not turned
// writable by M's row, and the record door refuses M as create does.
func TestPG_RequirementWrite_MemberOverrideSupplierIsHeld(t *testing.T) {
	for _, viaUpdate := range []bool{false, true} {
		name := "override set at create"
		if viaUpdate {
			name = "override set by update"
		}
		t.Run(name, func(t *testing.T) {
			root, project := memberProjectRoot(t)
			h := newSupplierPG(t, root)
			required := map[string]string{"write:" + project: types.OverrideRequired}
			mName := "m-" + uuid.NewString()[:8]
			var m uuid.UUID
			if viaUpdate {
				m = h.workspace(t, true, mName, project, nil)
				body, _ := json.Marshal(map[string]any{"name": mName, "sources": []types.WorkspaceSource{
					{Type: types.WorkspaceSourceTypeLocalDir, Path: project, Overrides: required}}})
				if w := h.send(t, true, http.MethodPut, "/api/v1/workspaces/"+m.String(), string(body)); w.Code != http.StatusOK {
					t.Fatalf("update workspace: code = %d; body=%s", w.Code, w.Body.String())
				}
				h.scanned(t, project) // an update resets the scan state
			} else {
				m = h.workspace(t, true, mName, project, required)
			}
			hydrated, err := h.srv.cfg.Store.GetWorkspace(context.Background(), m)
			if err != nil || hydrated.EffectiveRequirements["write:"+project].Level != "required" {
				t.Fatalf("the member's hydrated contract = %v (%v), want write:%s required", hydrated.EffectiveRequirements, err, project)
			}
			o := h.workspace(t, false, "o-"+uuid.NewString()[:8], project, nil)

			for door := range doorSuccess {
				for label, ws := range map[string]uuid.UUID{"operator workspace attached": o, "member workspace attached": m} {
					w := h.send(t, true, http.MethodPost, door, launch(ws, false))
					if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonWorkspaceSourceMountNotAllowed {
						t.Errorf("%s, %s: code = %d, want 422 %s; body=%s", door, label, w.Code, reasonWorkspaceSourceMountNotAllowed, w.Body.String())
					}
				}
			}
			if h.fr.createCalls != 0 {
				t.Fatalf("CreateSandbox calls = %d, want 0", h.fr.createCalls)
			}

			if w := h.send(t, true, http.MethodPost, "/api/v1/runs", launch(o, true)); w.Code != http.StatusCreated {
				t.Fatalf("operator workspace selected read_only: code = %d, want 201; body=%s", w.Code, w.Body.String())
			}
			h.fr.waitForSandbox(t)
			if mnt := mountFor(t, h.fr.lastSpec, composerWorkspaceTarget); !mnt.ReadOnly {
				t.Errorf("the run selected the operator workspace read_only, and the member workspace's row bound %q writable", mnt.Source)
			}

			w := h.send(t, false, http.MethodPost, "/api/v1/workspaces/"+m.String()+"/record", `{"name":"capture"}`)
			if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonWorkspaceSourceMountNotAllowed {
				t.Errorf("record on the member workspace: code = %d, want 422 %s; body=%s", w.Code, reasonWorkspaceSourceMountNotAllowed, w.Body.String())
			}
		})
	}
}

// TestPG_RequirementWrite_OperatorOwnWriteAdmitted is the control: an
// operator workspace whose own override requires the write, with a member's
// workspace on the same directory merely stored (created later, carrying no
// override), is admitted at every door, and create binds it writable.
func TestPG_RequirementWrite_OperatorOwnWriteAdmitted(t *testing.T) {
	root, project := memberProjectRoot(t)
	h := newSupplierPG(t, root)
	o := h.workspace(t, false, "o-"+uuid.NewString()[:8], project, map[string]string{"write:" + project: types.OverrideRequired})
	h.workspace(t, true, "m-"+uuid.NewString()[:8], project, nil)

	for door, want := range doorSuccess {
		if w := h.send(t, true, http.MethodPost, door, launch(o, false)); w.Code != want {
			t.Errorf("%s: code = %d, want %d; body=%s", door, w.Code, want, w.Body.String())
		}
	}
	h.fr.waitForSandbox(t)
	if mnt := mountFor(t, h.fr.lastSpec, composerWorkspaceTarget); mnt.ReadOnly || !strings.HasSuffix(mnt.Source, "/app") {
		t.Errorf("bind %q read-only=%v; the operator workspace's own write must reach the driver", mnt.Source, mnt.ReadOnly)
	}
}

// TestPG_RequirementWrite_OperatorSupplierGrantsBesideAMemberSupplier: an
// operator workspace O requires its own write on P, and an older member
// workspace M on P requires one too, outside the member's writable roots. The
// index pulls M in and both rows grant the mount; O's row alone grants it, so
// the run is admitted writable at every door, launched by the member or by an
// operator. M's refusal is the answer only when M is the sole supplier.
func TestPG_RequirementWrite_OperatorSupplierGrantsBesideAMemberSupplier(t *testing.T) {
	root, project := memberProjectRoot(t)
	h := newSupplierPG(t, root)
	required := map[string]string{"write:" + project: types.OverrideRequired}
	h.workspace(t, true, "m-"+uuid.NewString()[:8], project, required)
	o := h.workspace(t, false, "o-"+uuid.NewString()[:8], project, required)

	for n, member := range []bool{true, false} {
		for door, want := range doorSuccess {
			if w := h.send(t, member, http.MethodPost, door, launch(o, false)); w.Code != want {
				t.Fatalf("member=%v %s: code = %d, want %d; body=%s", member, door, w.Code, want, w.Body.String())
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for h.fr.creates() <= n && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if mnt := mountFor(t, h.fr.spec(), composerWorkspaceTarget); mnt.ReadOnly {
			t.Errorf("member=%v: bind %q is read-only; the operator workspace's own write must reach the driver", member, mnt.Source)
		}
	}
}

// creates and spec read the runner's counters under its lock.
func (f *fakeRunner) creates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

func (f *fakeRunner) spec() runner.SandboxSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSpec
}
