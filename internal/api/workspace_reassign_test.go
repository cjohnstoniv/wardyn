// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The two ADMIN-side halves of workspace ownership
// (docs/design/member-role-desktop.md §DECISIONS): the O6 offboarding reassign,
// and the O5 no-impersonation + cross-user marker contract every
// workspace-scoped write owes.

// UpdateWorkspace is a stub on authzStore (the ownership matrix never needs it);
// the no-impersonation sweep below does, because workspace.update is one of the
// events that has to carry the marker.
func (s *ownerStore) UpdateWorkspace(_ context.Context, id uuid.UUID, ws types.Workspace) (types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.workspaces[id]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	// Mirror the PG statement's column list: it does NOT carry owned_by, which
	// is what stops an ordinary edit from moving ownership.
	cur.Name, cur.Sources = ws.Name, ws.Sources
	s.workspaces[id] = cur
	return cur, nil
}

// reassignHarness seeds one member-owned workspace and returns the ids/sessions
// every case below needs.
func reassignHarness(t *testing.T) (srv *Server, st *ownerStore, h *harness, admin, member *http.Cookie, owned uuid.UUID) {
	t.Helper()
	srv, st, h = ownerHarness(t, runner.MemberMountPolicy{})
	admin = ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin)
	member = ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	owned = st.put(types.Workspace{OwnedBy: ownerMemberSub})
	return srv, st, h, admin, member, owned
}

// auditsFor returns every recorded event with the given action.
func auditsFor(h *harness, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.audit.events {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// auditData decodes an event's Data (nil Data decodes to a nil map).
func auditData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	if len(ev.Data) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("decode audit data %s: %v", ev.Data, err)
	}
	return m
}

// TestWorkspaceReassign_AdminReturnsRowToOperator is O6's happy path: the row
// comes back to the operator, the audit names who it came FROM, and the
// ex-owner's write access is gone the moment it lands (they keep the
// operator-owned READ every member has).
func TestWorkspaceReassign_AdminReturnsRowToOperator(t *testing.T) {
	srv, st, h, admin, member, owned := reassignHarness(t)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+owned.String()+"/reassign", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin reassign: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OwnedBy != "" {
		t.Errorf("response owned_by = %q, want \"\" (operator-owned)", got.OwnedBy)
	}
	stored, err := st.GetWorkspace(context.Background(), owned)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if stored.OwnedBy != "" {
		t.Errorf("stored owned_by = %q, want \"\" — the column must actually move, not just the response", stored.OwnedBy)
	}

	evs := auditsFor(h, "workspace.reassign")
	if len(evs) != 1 {
		t.Fatalf("workspace.reassign events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Actor != "sub-owner-admin" {
		t.Errorf("audit actor = %q, want the ADMIN's identity — never the member's", ev.Actor)
	}
	if ev.Target != owned.String() {
		t.Errorf("audit target = %q, want the workspace id", ev.Target)
	}
	data := auditData(t, ev)
	if data["from_owner"] != ownerMemberSub {
		t.Errorf("audit from_owner = %v, want %q", data["from_owner"], ownerMemberSub)
	}
	if data["workspace_owner"] != ownerMemberSub {
		t.Errorf("audit workspace_owner = %v, want %q (the O5 cross-user marker)", data["workspace_owner"], ownerMemberSub)
	}

	// The reassigned row's local_dir sources leave the member mount gate with
	// it — memberMountPosture resolves no roots for an operator-owned
	// workspace, so its binds are ordinary operator mounts from here on. Pinned
	// HERE, at the transition, because that is where a reader looks for it.
	if p := srv.memberMountPosture([]types.Workspace{stored}); p.Roots != nil || p.Sources != nil {
		t.Errorf("posture after reassign = %+v, want the zero value — an operator-owned row takes the operator mount path", p)
	}

	// The ex-owner is now an ordinary member on an operator-owned row: still
	// readable, no longer writable.
	if rw := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+owned.String(), member, ""); rw.Code != http.StatusOK {
		t.Errorf("ex-owner READ after reassign: code = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	if dw := doSSO(t, srv, http.MethodDelete, "/api/v1/workspaces/"+owned.String(), member, ""); dw.Code != http.StatusForbidden {
		t.Errorf("ex-owner DELETE after reassign: code = %d, want 403; body=%s", dw.Code, dw.Body.String())
	}
}

// TestWorkspaceReassign_MemberRefusalIsExistenceBlind: reassign is admin-only,
// and its refusal must teach a member NOTHING about the id they named. Every
// case — their own workspace, another member's, an operator-owned one, and an
// id that does not exist at all — answers the byte-identical 403, and none of
// them reaches the store.
func TestWorkspaceReassign_MemberRefusalIsExistenceBlind(t *testing.T) {
	srv, st, h, _, member, owned := reassignHarness(t)
	foreign := st.put(types.Workspace{OwnedBy: ownerOtherSub})
	opOwned := st.put(types.Workspace{})
	missing := uuid.New()

	var first string
	for _, c := range []struct {
		name string
		id   uuid.UUID
	}{
		{"own", owned}, {"foreign", foreign}, {"operator-owned", opOwned}, {"missing", missing},
	} {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+c.id.String()+"/reassign", member, "")
		if w.Code != http.StatusForbidden {
			t.Errorf("member reassign (%s): code = %d, want 403; body=%s", c.name, w.Code, w.Body.String())
			continue
		}
		got := w.Body.String()
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Errorf("member reassign (%s): body %q is DISTINGUISHABLE from the first case's %q — the refusal is an existence oracle", c.name, got, first)
		}
	}
	// A refused reassign never moved anything, and never wrote a success row.
	if ws, _ := st.GetWorkspace(context.Background(), owned); ws.OwnedBy != ownerMemberSub {
		t.Errorf("owner after a refused member reassign = %q, want %q — the refusal must not write", ws.OwnedBy, ownerMemberSub)
	}
	if n := len(auditsFor(h, "workspace.reassign")); n != 0 {
		t.Errorf("workspace.reassign events = %d, want 0 — a refused reassign is not a reassign", n)
	}
}

// TestWorkspaceReassign_Idempotent: offboarding walks a list of ids, so
// reassigning a row that is already operator-owned must succeed (from_owner "")
// rather than fail the sweep halfway through.
func TestWorkspaceReassign_Idempotent(t *testing.T) {
	srv, st, h, admin, _, _ := reassignHarness(t)
	opOwned := st.put(types.Workspace{})

	for i := 0; i < 2; i++ {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+opOwned.String()+"/reassign", admin, "")
		if w.Code != http.StatusOK {
			t.Fatalf("reassign #%d of an already-operator-owned row: code = %d, want 200; body=%s", i+1, w.Code, w.Body.String())
		}
	}
	for _, ev := range auditsFor(h, "workspace.reassign") {
		data := auditData(t, ev)
		if data["from_owner"] != "" {
			t.Errorf("from_owner = %v, want \"\" for an already-operator-owned row", data["from_owner"])
		}
		if _, ok := data["workspace_owner"]; ok {
			t.Errorf("workspace_owner was stamped on an OPERATOR-owned row (%v) — the marker means 'an admin touched a MEMBER's data'", data)
		}
	}
}

// TestWorkspaceReassign_UnknownWorkspaceIsNotFound: for an ADMIN (who is not
// being told anything they could not already learn from GET), a missing id is
// an honest 404 and writes nothing.
func TestWorkspaceReassign_UnknownWorkspaceIsNotFound(t *testing.T) {
	srv, _, h, admin, _, _ := reassignHarness(t)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+uuid.New().String()+"/reassign", admin, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("admin reassign of a missing id: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if n := len(auditsFor(h, "workspace.reassign")); n != 0 {
		t.Errorf("workspace.reassign events = %d, want 0", n)
	}
}

// TestWorkspaceOwner_NoImpersonation is decision O5's guard. When an ADMIN acts
// on a MEMBER-owned workspace:
//
//   - the audit actor is the ADMIN's own identity, NEVER the member's — there
//     is no impersonation anywhere on this path, so an admin can never produce
//     an audit trail that reads as the member having done it themselves;
//   - the event additionally carries workspace_owner naming the member, so
//     "which member data did this admin touch" is a QUERY (?actor=<admin> plus
//     workspace_owner != actor) and not an archaeology exercise.
//
// Swept across the workspace-scoped write surface rather than one route: the
// generic scopedWorkspaceWrite chokepoint, two handlers that audit directly,
// and the reassign added by O6. A new workspace write that forgets the marker
// is the regression this exists to catch.
func TestWorkspaceOwner_NoImpersonation(t *testing.T) {
	const adminSub = "sub-owner-admin"
	srv, st, h, admin, _, _ := reassignHarness(t)

	type act struct {
		action, method, path, body string
	}
	// One fresh member-owned workspace per act, so a destructive one (delete,
	// reassign) cannot decide what the next one sees.
	acts := []act{
		{"workspace.egress.approve", http.MethodPut, "/approved-egress", `{"domains":["example.com"]}`},
		{"workspace.update", http.MethodPut, "", `{"name":"renamed-by-admin"}`},
		{"workspace.delete", http.MethodDelete, "", ""},
		{"workspace.reassign", http.MethodPost, "/reassign", ""},
	}
	for _, a := range acts {
		id := st.put(types.Workspace{OwnedBy: ownerMemberSub})
		w := doSSO(t, srv, a.method, "/api/v1/workspaces/"+id.String()+a.path, admin, a.body)
		if w.Code >= 400 {
			t.Fatalf("%s: admin act failed with %d; body=%s", a.action, w.Code, w.Body.String())
		}
		var ev *types.AuditEvent
		for i := range h.audit.events {
			if h.audit.events[i].Action == a.action && h.audit.events[i].Target == id.String() {
				ev = &h.audit.events[i]
			}
		}
		if ev == nil {
			t.Errorf("%s: no audit event for the admin's act on a member-owned workspace", a.action)
			continue
		}
		if ev.Actor != adminSub {
			t.Errorf("%s: audit actor = %q, want %q — an admin acting on a member's workspace must never be recorded AS the member", a.action, ev.Actor, adminSub)
		}
		if got := auditData(t, *ev)["workspace_owner"]; got != ownerMemberSub {
			t.Errorf("%s: workspace_owner = %v, want %q — cross-user admin access must be queryable", a.action, got, ownerMemberSub)
		}
	}
}

// TestWorkspaceOwner_OwnerActingOnOwnIsUnmarked is the negative control for the
// marker: a member acting on their OWN workspace is not cross-user access, so
// no workspace_owner is stamped — otherwise the marker matches every event and
// distinguishes nothing.
func TestWorkspaceOwner_OwnerActingOnOwnIsUnmarked(t *testing.T) {
	srv, st, h, _, member, _ := reassignHarness(t)
	id := st.put(types.Workspace{OwnedBy: ownerMemberSub})

	if w := doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), member, `{"name":"mine"}`); w.Code != http.StatusOK {
		t.Fatalf("owner update: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	for _, ev := range auditsFor(h, "workspace.update") {
		if _, ok := auditData(t, ev)["workspace_owner"]; ok {
			t.Errorf("workspace_owner stamped on the OWNER's own write (%s) — the marker must mean cross-user access", ev.Data)
		}
	}
}
