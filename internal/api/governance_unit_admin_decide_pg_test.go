// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newUnitAdminDecideEnv is the unit-admin tree on a store that runs *hook once, between the approver's
// pre-check and the decision transaction (beforeDecideStore). The hook is cleared before it runs, so a
// write it makes through the API cannot fire it again.
func newUnitAdminDecideEnv(t *testing.T, hook *func()) *unitAdminEnv {
	t.Helper()
	return newUnitAdminEnv(t, false, func(c *Config) {
		c.Store = beforeDecideStore{PG: store.NewPG(c.Store.(store.PG).Pool), hook: func(context.Context) {
			if f := *hook; f != nil {
				*hook = nil
				f()
			}
		}}
	})
}

func (e *unitAdminEnv) stateOf(id uuid.UUID) string {
	e.t.Helper()
	ch, err := e.pg.GetGovernanceChange(context.Background(), id)
	if err != nil {
		e.t.Fatalf("read change %s: %v", id, err)
	}
	return ch.State
}

// TestUnitAdminApproverScopeRereadInTx: a unit admin's scope over a held change is read again inside
// the decision's transaction, for a session and for an API token. A grant revoked, or the unit
// re-parented out of the approver's subtree, after the pre-check passed refuses the approval and
// leaves the change pending; an approver still in scope applies it.
func TestUnitAdminApproverScopeRereadInTx(t *testing.T) {
	var hook func()
	e := newUnitAdminDecideEnv(t, &hook)
	approver := func() (string, types.GovernanceUnitAdmin) {
		sub := e.fresh("sub-ua-approver")
		return sub, e.grant(e.a, types.CapabilitySubjectUser, sub)
	}
	revoke := func(g types.GovernanceUnitAdmin) func() {
		return func() {
			if _, err := store.RevokeUnitAdminQ(context.Background(), e.pool, g.UnitID, g.ID); err != nil {
				t.Errorf("revoke grant %s: %v", g.ID, err)
			}
		}
	}
	refusedPending := func(t *testing.T, what string, ch types.GovernanceChange, w *httptest.ResponseRecorder) {
		t.Helper()
		if !scopeRefused(t, w) {
			t.Fatalf("%s: approve = %d %s, want 403 unit_scope_denied", what, w.Code, w.Body)
		}
		if got := e.stateOf(ch.ID); got != types.GovernanceChangePending {
			t.Fatalf("%s: change state = %q, want pending", what, got)
		}
	}

	t.Run("session grant revoked", func(t *testing.T) {
		sub, g := approver()
		ch := e.heldOn(e.a2)
		hook = revoke(g)
		refusedPending(t, "session grant revoked", ch, e.call(uaSession(t, sub, []string{}, false), http.MethodPost, approvePath(ch.ID), ""))
	})

	t.Run("session unit re-parented out", func(t *testing.T) {
		sub, _ := approver()
		target := e.child(e.a)
		ch := e.heldOn(target)
		hook = func() {
			if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+target.ID.String(), e.restate(target, map[string]any{"parent_id": e.b.ID})); w.Code != http.StatusOK {
				t.Errorf("re-parent %s under ua-b = %d %s, want 200", target.Name, w.Code, w.Body)
			}
		}
		refusedPending(t, "session unit re-parented out", ch, e.call(uaSession(t, sub, []string{}, false), http.MethodPost, approvePath(ch.ID), ""))
	})

	t.Run("token grant revoked", func(t *testing.T) {
		sub, g := approver()
		_, raw := e.mintToken(sub, sub+"@corp.example", oidc.RoleUser)
		ch := e.heldOn(e.a2)
		hook = revoke(g)
		refusedPending(t, "token grant revoked", ch, do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw, ""))
	})

	t.Run("in scope applies", func(t *testing.T) {
		sub, _ := approver()
		_, raw := e.mintToken(sub, sub+"@corp.example", oidc.RoleUser)
		byToken := e.heldOn(e.a2)
		if w := do(t, e.srv, http.MethodPost, approvePath(byToken.ID), raw, ""); w.Code != http.StatusOK {
			t.Fatalf("token approve in scope = %d %s, want 200", w.Code, w.Body)
		}
		bySession := e.heldOn(e.a2)
		if w := e.call(uaSession(t, sub, []string{}, false), http.MethodPost, approvePath(bySession.ID), ""); w.Code != http.StatusOK {
			t.Fatalf("session approve in scope = %d %s, want 200", w.Code, w.Body)
		}
		for _, id := range []uuid.UUID{byToken.ID, bySession.ID} {
			if got := e.stateOf(id); got == types.GovernanceChangePending {
				t.Fatalf("change %s still pending after an in-scope approval", id)
			}
		}
	})
}
