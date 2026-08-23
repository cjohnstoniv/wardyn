// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Workspace OWNERSHIP (migration 0048, docs/design/member-role-desktop.md §b)
// and the member-safe local_dir gate (§c) at the HTTP boundary. TestAuthzMatrix
// proves the coarse owner/foreign/admin shape for the reclassified routes;
// these tests prove the two things it deliberately does not: 404 PARITY (a
// foreign owned row is byte-identical to a missing one) and the operator-owned
// back-compat case (still admin-only, still a 403).

// ownerStore is authzStore plus a real ListWorkspaces/CreateWorkspace, which
// the ownership list-scoping and owner-stamp assertions need.
type ownerStore struct{ *authzStore }

func newOwnerStore() *ownerStore { return &ownerStore{authzStore: newAuthzStore()} }

func (s *ownerStore) CreateWorkspace(_ context.Context, ws types.Workspace) (types.Workspace, error) {
	if ws.ID == uuid.Nil {
		ws.ID = uuid.New()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaces[ws.ID] = ws
	return ws, nil
}

func (s *ownerStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.Workspace, 0, len(s.workspaces))
	for _, ws := range s.workspaces {
		out = append(out, ws)
	}
	return out, nil
}

func (s *ownerStore) put(ws types.Workspace) uuid.UUID {
	if ws.ID == uuid.Nil {
		ws.ID = uuid.New()
	}
	if ws.Name == "" {
		ws.Name = "ws"
	}
	if len(ws.Sources) == 0 {
		ws.Sources = []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaces[ws.ID] = ws
	return ws.ID
}

// ownerHarness wires a server with OIDC on (so roles are real) over ownerStore.
func ownerHarness(t *testing.T, mounts runner.MemberMountPolicy) (*Server, *ownerStore, *harness) {
	t.Helper()
	st := newOwnerStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.MemberMounts = mounts
	return New(cfg), st, h
}

const (
	ownerMemberSub = "sub-owner-member"
	ownerOtherSub  = "sub-owner-other"
)

// TestWorkspaceOwnership_ForeignOwned404Parity is adversarial-matrix row 4: a
// member acting on ANOTHER member's owned workspace gets the byte-identical
// status AND body a truly-missing id gets — so an id probe is not an existence
// oracle. Asserted by EQUALITY against the missing-id response, never by
// eyeballing "it's a 404 too".
func TestWorkspaceOwnership_ForeignOwned404Parity(t *testing.T) {
	srv, st, h := ownerHarness(t, runner.MemberMountPolicy{})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	foreign := st.put(types.Workspace{OwnedBy: ownerOtherSub})
	missing := uuid.New()

	for _, c := range []struct{ method, suffix, body string }{
		{http.MethodGet, "", ""},
		{http.MethodPut, "", `{"name":"renamed"}`},
		// A MALFORMED body is the sharper probe: it is what catches a handler
		// that parses before it authorizes. Such a handler answers 400 here and
		// 404 for the well-formed case above, and the difference between the two
		// is a working existence oracle.
		{http.MethodPut, "", `{"name":""}`},
		{http.MethodPut, "", `{not json`},
		{http.MethodDelete, "", ""},
		{http.MethodPost, "/scan", ""},
		{http.MethodPost, "/build", ""},
		{http.MethodGet, "/build", ""},
		{http.MethodGet, "/env-as-code", ""},
		{http.MethodGet, "/observed-egress", ""},
	} {
		name := c.method + " " + c.suffix
		gotForeign := doSSO(t, srv, c.method, "/api/v1/workspaces/"+foreign.String()+c.suffix, member, c.body)
		gotMissing := doSSO(t, srv, c.method, "/api/v1/workspaces/"+missing.String()+c.suffix, member, c.body)
		if gotForeign.Code != http.StatusNotFound {
			t.Errorf("%s foreign owned: code = %d, want 404 (no existence oracle); body=%s", name, gotForeign.Code, gotForeign.Body.String())
			continue
		}
		if gotForeign.Code != gotMissing.Code || gotForeign.Body.String() != gotMissing.Body.String() {
			t.Errorf("%s: foreign (%d %s) is DISTINGUISHABLE from missing (%d %s)",
				name, gotForeign.Code, gotForeign.Body.String(), gotMissing.Code, gotMissing.Body.String())
		}
	}

	// The authz.denied audit fires ONLY for the positively-identified foreign
	// row, never for the truly-missing one — otherwise the audit trail becomes
	// the oracle the response body is not.
	var foreignDenials, missingDenials int
	for _, ev := range h.audit.events {
		if ev.Action != "authz.denied" {
			continue
		}
		switch ev.Target {
		case foreign.String():
			foreignDenials++
		case missing.String():
			missingDenials++
		}
	}
	if foreignDenials == 0 {
		t.Error("no authz.denied audit for the foreign workspace; a cross-member access attempt must be recorded")
	}
	if missingDenials != 0 {
		t.Errorf("authz.denied fired %d times for a MISSING workspace id — the audit trail must not distinguish missing from foreign", missingDenials)
	}
}

// TestWorkspaceOwnership_OwnerReachesOwn is the positive control for the same
// routes: the owning member is not refused.
func TestWorkspaceOwnership_OwnerReachesOwn(t *testing.T) {
	srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	own := st.put(types.Workspace{OwnedBy: ownerMemberSub})

	for _, c := range []struct{ method, suffix string }{
		{http.MethodGet, ""},
		{http.MethodGet, "/build"},
		{http.MethodGet, "/observed-egress"},
	} {
		w := doSSO(t, srv, c.method, "/api/v1/workspaces/"+own.String()+c.suffix, member, "")
		if w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
			t.Errorf("%s %s on the member's OWN workspace: code = %d, want reachable; body=%s", c.method, c.suffix, w.Code, w.Body.String())
		}
	}
}

// TestWorkspaceOwnership_OperatorOwnedStaysAdminOnly is the back-compat pin the
// matrix cannot express: an OPERATOR-owned workspace (owned_by = ” — every row
// that exists before this migration) keeps its 0.5 behavior for a member.
// READABLE, and its mutations answer the SAME 403 requireOperator wrote before
// the routes moved off the operatorOnly group — not a 404, which would be a lie
// about a row the member can see in their own list.
func TestWorkspaceOwnership_OperatorOwnedStaysAdminOnly(t *testing.T) {
	srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin)
	opOwned := st.put(types.Workspace{}) // owned_by == "" — operator-owned

	if w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+opOwned.String(), member, ""); w.Code != http.StatusOK {
		t.Errorf("member READING an operator-owned workspace: code = %d, want 200 (unchanged from 0.5); body=%s", w.Code, w.Body.String())
	}
	for _, c := range []struct{ method, suffix, body string }{
		{http.MethodPut, "", `{"name":"renamed"}`},
		{http.MethodDelete, "", ""},
		{http.MethodPost, "/scan", ""},
		{http.MethodPost, "/build", ""},
	} {
		w := doSSO(t, srv, c.method, "/api/v1/workspaces/"+opOwned.String()+c.suffix, member, c.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("member %s %s on an OPERATOR-owned workspace: code = %d, want 403 (byte-identical to the pre-0048 requireOperator refusal); body=%s",
				c.method, c.suffix, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "requires admin role") {
			t.Errorf("member %s %s: body = %s, want requireOperator's own message", c.method, c.suffix, w.Body.String())
		}
	}
	// The admin still reaches every one of them.
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/workspaces/"+opOwned.String(), admin, ""); w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
		t.Errorf("admin DELETE on an operator-owned workspace: code = %d, want allowed; body=%s", w.Code, w.Body.String())
	}
}

// TestWorkspaceOwnership_CreateStampsOwner: a member's create is owner-stamped
// from the SESSION, an operator's stays operator-owned. The stamp cannot come
// from the body — strict decoding refuses an owned_by field outright, which is
// what keeps the column un-forgeable.
func TestWorkspaceOwnership_CreateStampsOwner(t *testing.T) {
	srv, _, _ := ownerHarness(t, runner.MemberMountPolicy{})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin)

	created := func(t *testing.T, sess *http.Cookie, body string) types.Workspace {
		t.Helper()
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", sess, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("create: code = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		var ws types.Workspace
		if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return ws
	}

	if got := created(t, member, `{"name":"mine"}`).OwnedBy; got != ownerMemberSub {
		t.Errorf("member-created owned_by = %q, want %q", got, ownerMemberSub)
	}
	if got := created(t, admin, `{"name":"theirs"}`).OwnedBy; got != "" {
		t.Errorf("admin-created owned_by = %q, want \"\" (operator-owned)", got)
	}
	// A forged owned_by in the body is refused by strict decoding, so it can
	// never reach the stamp at all.
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, `{"name":"forged","owned_by":"`+ownerOtherSub+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("create with a body owned_by: code = %d, want 400 (unknown field); body=%s", w.Code, w.Body.String())
	}
}

// TestWorkspaceOwnership_ListScoping: a member's list is their own owned rows
// plus every operator-owned one, never another member's.
func TestWorkspaceOwnership_ListScoping(t *testing.T) {
	srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin)

	own := st.put(types.Workspace{OwnedBy: ownerMemberSub})
	foreign := st.put(types.Workspace{OwnedBy: ownerOtherSub})
	opOwned := st.put(types.Workspace{})

	list := func(t *testing.T, sess *http.Cookie) map[uuid.UUID]bool {
		t.Helper()
		w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces", sess, "")
		if w.Code != http.StatusOK {
			t.Fatalf("list: code = %d; body=%s", w.Code, w.Body.String())
		}
		var got []types.Workspace
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := map[uuid.UUID]bool{}
		for _, ws := range got {
			ids[ws.ID] = true
		}
		return ids
	}

	got := list(t, member)
	if !got[own] {
		t.Error("member's list omits their OWN workspace")
	}
	if !got[opOwned] {
		t.Error("member's list omits the OPERATOR-owned workspace (unchanged from 0.5)")
	}
	if got[foreign] {
		t.Error("member's list includes ANOTHER MEMBER's owned workspace")
	}

	adminGot := list(t, admin)
	for _, id := range []uuid.UUID{own, foreign, opOwned} {
		if !adminGot[id] {
			t.Errorf("admin's list omits %s; an admin sees every workspace", id)
		}
	}
}

// memberProjectRoot makes a real, resolved root with a project dir under it —
// the member-mount gate turns on EvalSymlinks, so the paths must exist.
func memberProjectRoot(t *testing.T) (root, project string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	root = filepath.Join(base, "projects")
	project = filepath.Join(root, "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root, project
}

// TestWorkspaceOwnership_MemberLocalDirGate wires the §c gate to the actual
// onboarding route: a MEMBER's local_dir source clears the root allowlist, the
// dotfile deny-list and the writable allowlist, while an OPERATOR onboarding
// the exact same paths is unaffected.
func TestWorkspaceOwnership_MemberLocalDirGate(t *testing.T) {
	root, project := memberProjectRoot(t)
	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	dotfile := filepath.Join(root, ".ssh")
	for _, d := range []string{outside, dotfile} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	srv, _, _ := ownerHarness(t, runner.MemberMountPolicy{Roots: []string{root}})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin)

	body := func(path string, writable bool) string {
		src := types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: path, Writable: writable}
		b, err := json.Marshal(map[string]any{"name": "w", "sources": []types.WorkspaceSource{src}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}

	if w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, body(project, false)); w.Code != http.StatusCreated {
		t.Errorf("member onboarding a dir INSIDE the root: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	for _, c := range []struct {
		name     string
		path     string
		writable bool
	}{
		{"outside every root", outside, false},
		{"credential dotfile inside a root", dotfile, false},
		{"writable with no writable roots configured", project, true},
	} {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, body(c.path, c.writable))
		if w.Code != http.StatusBadRequest {
			t.Errorf("member onboarding %s: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		}
	}

	// The OPERATOR is untouched by every one of those rules — the gate is
	// additive, and an operator's mounts keep the reach they have today.
	for _, c := range []struct {
		name     string
		path     string
		writable bool
	}{
		{"outside every member root", outside, false},
		{"writable anywhere", project, true},
	} {
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", admin, body(c.path, c.writable)); w.Code != http.StatusCreated {
			t.Errorf("ADMIN onboarding %s: code = %d, want 201 — the member gate must never narrow an operator mount; body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

// TestWorkspaceOwnership_MemberWritableAllowlist pins O3's two halves at the
// route: writable inside WARDYN_MEMBER_WRITABLE_ROOTS is accepted, and the
// deny carve-out wins over it.
func TestWorkspaceOwnership_MemberWritableAllowlist(t *testing.T) {
	root, project := memberProjectRoot(t)
	vendored := filepath.Join(project, "vendor")
	if err := os.MkdirAll(vendored, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srv, _, _ := ownerHarness(t, runner.MemberMountPolicy{
		Roots: []string{root}, WritableRoots: []string{root}, WritableDeny: []string{vendored},
	})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)

	body := func(path string) string {
		src := types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: path, Writable: true}
		b, _ := json.Marshal(map[string]any{"name": "w", "sources": []types.WorkspaceSource{src}})
		return string(b)
	}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, body(project)); w.Code != http.StatusCreated {
		t.Errorf("writable inside the writable root: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, body(vendored)); w.Code != http.StatusBadRequest {
		t.Errorf("writable inside the DENY carve-out: code = %d, want 400 (deny wins); body=%s", w.Code, w.Body.String())
	}
}

// TestMemberMountRoots_ThreadedToDispatch pins the seam that carries the roots
// to the driver: a run against a MEMBER-OWNED workspace resolves that member's
// roots (so the driver re-checks every bind), an operator-owned one resolves
// nil (so the driver takes exactly today's path), and a member-owned workspace
// on a deployment with NO roots resolves EMPTY-but-non-nil, which fails the
// bind closed rather than silently degrading to the operator path.
func TestMemberMountRoots_ThreadedToDispatch(t *testing.T) {
	root, _ := memberProjectRoot(t)
	srv, _, _ := ownerHarness(t, runner.MemberMountPolicy{Roots: []string{root}})

	opOwned := types.Workspace{ID: uuid.New()}
	memberOwned := types.Workspace{ID: uuid.New(), OwnedBy: ownerMemberSub}

	if got := srv.memberMountRoots(nil); got != nil {
		t.Errorf("no workspaces: roots = %v, want nil (operator path)", got)
	}
	if got := srv.memberMountRoots([]types.Workspace{opOwned}); got != nil {
		t.Errorf("operator-owned workspace: roots = %v, want nil (operator path)", got)
	}
	got := srv.memberMountRoots([]types.Workspace{opOwned, memberOwned})
	if len(got) != 1 || got[0] != root {
		t.Errorf("member-owned workspace: roots = %v, want %v", got, []string{root})
	}

	unconfigured, _, _ := ownerHarness(t, runner.MemberMountPolicy{})
	got = unconfigured.memberMountRoots([]types.Workspace{memberOwned})
	if got == nil {
		t.Fatal("member-owned workspace with NO configured roots: roots = nil, which the driver reads as an OPERATOR run — must be empty-but-non-nil so every bind fails closed")
	}
	if len(got) != 0 {
		t.Errorf("roots = %v, want empty", got)
	}
}
