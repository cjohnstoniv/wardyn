// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The write bound of unit deciders: a unit's rule may only narrow the rules above it for the same kind. They
// run on the unit-admin tree of governance_unit_admin_pg_test.go (ua-a with ua-a1 below it).

// egressWho is an egress_domain rule naming who, with any extra fields.
func egressWho(extra map[string]any, who ...string) map[string]any {
	m := map[string]any{"kinds": []string{"egress_domain"}, "who": who}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// wantWiderThanParent fails unless w is the 422 naming field and the unit above.
func wantWiderThanParent(t *testing.T, what string, w *httptest.ResponseRecorder, field, above string) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != "decider_rule_wider_than_parent" ||
		!strings.Contains(w.Body.String(), "rules[0]: "+field+" for egress_domain") || !strings.Contains(w.Body.String(), above) {
		t.Errorf("%s = %d %s, want 422 decider_rule_wider_than_parent naming rules[0], %s and %s", what, w.Code, w.Body, field, above)
	}
}

// TestDecidersChildCannotWiden: under a parent rule naming unit_admins, a child rule that adds owner is refused,
// for a security admin and for the admin token alike, and nothing is stored. Clearing is never refused.
func TestDecidersChildCannotWiden(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(egressWho(nil, "unit_admins"))); w.Code != http.StatusOK {
		t.Fatalf("parent rule = %d %s", w.Code, w.Body)
	}
	wide := decidersBody(egressWho(nil, "unit_admins", "owner"))
	wantWiderThanParent(t, "security admin", e.call(e.alice, http.MethodPut, decidersPath(e.a1), wide), "who", "ua-a")
	wantWiderThanParent(t, "admin token", e.admin(http.MethodPut, decidersPath(e.a1), wide), "who", "ua-a")
	if got := e.storedDeciders(e.a1); got != nil {
		t.Errorf("ua-a1 stored %v after the refusals, want none", got)
	}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody()); w.Code != http.StatusOK {
		t.Errorf("clearing under a parent rule = %d %s, want 200", w.Code, w.Body)
	}
}

// TestDecidersChildWidenRefusedAtProposeAndApply: a unit admin's held write wider than the parent is refused when
// proposed. One proposed while the parent allowed it is refused at approval once the parent is narrowed, and the
// change stays pending with nothing stored.
func TestDecidersChildWidenRefusedAtProposeAndApply(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(egressWho(nil, "unit_admins"))); w.Code != http.StatusOK {
		t.Fatalf("parent rule = %d %s", w.Code, w.Body)
	}
	wide := decidersBody(egressWho(nil, "unit_admins", "owner"))
	wantWiderThanParent(t, "proposing", e.call(e.adminA, http.MethodPut, decidersPath(e.a1), wide), "who", "ua-a")
	if n := e.pendingCount(); n != 0 {
		t.Fatalf("%d changes held after the refused proposal, want none", n)
	}

	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(egressWho(nil, "unit_admins", "owner"))); w.Code != http.StatusOK {
		t.Fatalf("widen the parent = %d %s", w.Code, w.Body)
	}
	ch := e.pending(e.call(e.adminA, http.MethodPut, decidersPath(e.a1), wide))
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(egressWho(nil, "unit_admins"))); w.Code != http.StatusOK {
		t.Fatalf("narrow the parent = %d %s", w.Code, w.Body)
	}
	wantWiderThanParent(t, "approving", e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""), "who", "ua-a")
	if e.changeState(ch.ID) != types.GovernanceChangePending {
		t.Errorf("change is %s after the refusal, want still pending", e.changeState(ch.ID))
	}
	if got := e.storedDeciders(e.a1); got != nil {
		t.Errorf("ua-a1 stored %v after the refused approval, want none", got)
	}
}

// TestDecidersChildNarrowingAccepted: a child rule that names fewer deciders, adds a second_human or lowers
// max_scope is within the parent's and applies.
func TestDecidersChildNarrowingAccepted(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	parent := egressWho(map[string]any{"max_scope": "run"}, "unit_admins", "group:eng-leads")
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(parent)); w.Code != http.StatusOK {
		t.Fatalf("parent rule = %d %s", w.Code, w.Body)
	}
	for name, rule := range map[string]map[string]any{
		"fewer who":          egressWho(map[string]any{"max_scope": "run"}, "unit_admins"),
		"added second_human": egressWho(map[string]any{"max_scope": "run", "second_human": true}, "unit_admins", "group:eng-leads"),
		"lower max_scope":    egressWho(map[string]any{"max_scope": "once"}, "unit_admins", "group:eng-leads"),
	} {
		if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody(rule)); w.Code != http.StatusOK {
			t.Errorf("%s = %d %s, want 200", name, w.Code, w.Body)
		}
	}
	// An empty max_scope reaches as far as run, so it is within a parent's run.
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody(egressWho(nil, "unit_admins"))); w.Code != http.StatusOK {
		t.Errorf("an empty max_scope under a parent's run = %d %s, want 200", w.Code, w.Body)
	}
}

// TestDecidersPendingParentWideningRefusesChild: a child's widening that relies on a parent widening still held
// is refused when proposed; once the parent's change is approved, the child's is proposed again and applies.
func TestDecidersPendingParentWideningRefusesChild(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	if w := e.admin(http.MethodPut, decidersPath(e.a), decidersBody(egressWho(nil, "unit_admins"))); w.Code != http.StatusOK {
		t.Fatalf("parent rule = %d %s", w.Code, w.Body)
	}
	wide := decidersBody(egressWho(nil, "unit_admins", "owner"))
	parent := e.pending(e.call(e.alice, http.MethodPut, decidersPath(e.a), wide))
	wantWiderThanParent(t, "the child while the parent's widening is held", e.call(e.alice, http.MethodPut, decidersPath(e.a1), wide), "who", "ua-a")
	e.approve(parent.ID)
	child := e.pending(e.call(e.alice, http.MethodPut, decidersPath(e.a1), wide))
	e.approve(child.ID)
	if got := e.storedDeciders(e.a1); len(got) != 1 || len(got[0].Who) != 2 {
		t.Errorf("ua-a1 stored %v after the parent's change applied, want the widened rule", got)
	}
}
