// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The unit deciders write pins (PUT /governance/units/{id}/deciders): who may write them, that a
// widening is always held, that a parent's lock binds, and that a rule the model refuses is a 400.
// They run on the unit-admin tree of governance_unit_admin_pg_test.go: ua-a with ua-a1 and ua-a2 below
// it and ua-b beside it, adminA on ua-a, adminA1 on ua-a1, alice and bob the security admins.

func decidersPath(u types.GovernanceUnit) string {
	return "/api/v1/governance/units/" + u.ID.String() + "/deciders"
}

func decidersBody(rules ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"rules": rules})
	return string(b)
}

// egressOwnerRule lets the owner and a group decide egress.
func egressOwnerRule() map[string]any {
	return map[string]any{"kinds": []string{"egress_domain"}, "who": []string{"owner", "group:eng-leads"}, "max_scope": "run"}
}

// mcpPeersRule lets unit admins decide MCP calls, with a second human.
func mcpPeersRule() map[string]any {
	return map[string]any{"kinds": []string{"mcp_call"}, "who": []string{"unit_admins"}, "second_human": true}
}

// storedDeciders is unit u's stored rules, nil when it has none.
func (e *unitAdminEnv) storedDeciders(u types.GovernanceUnit) []types.DeciderRule {
	e.t.Helper()
	got, err := store.GetUnitDecidersQ(context.Background(), e.pool, u.ID, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		e.t.Fatalf("read deciders of %s: %v", u.Name, err)
	}
	return got.Rules
}

func kindsOf(rules []types.DeciderRule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Kinds...)
	}
	slices.Sort(out)
	return out
}

// TestDecidersUnitAdminWriteAlwaysHeld: with the switch off, a unit admin's write for a unit below
// theirs is held, and nothing is stored until a distinct, authorised person approves it. Its proposer
// and an admin of another subtree cannot; a peer admin above the unit and a security admin each can.
func TestDecidersUnitAdminWriteAlwaysHeld(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for _, tc := range []struct {
		who      string
		unit     types.GovernanceUnit
		approver func() *http.Cookie
	}{
		{"a peer admin of ua-a", e.a1, func() *http.Cookie { return e.peerA }},
		{"a security admin", e.a2, func() *http.Cookie { return e.alice }},
	} {
		ch := e.pending(e.call(e.adminA, http.MethodPut, decidersPath(tc.unit), decidersBody(egressOwnerRule())))
		if ch.TargetKind != types.GovernanceTargetUnitDeciders {
			t.Fatalf("held kind = %q, want %q", ch.TargetKind, types.GovernanceTargetUnitDeciders)
		}
		if got := e.storedDeciders(tc.unit); got != nil {
			t.Fatalf("%s: a held write stored %v before approval", tc.unit.Name, got)
		}
		if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
			t.Errorf("the proposer approving = %d %s, want 403 second_human_required", w.Code, w.Body)
		}
		if w := e.call(e.adminB, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(t, w) {
			t.Errorf("ua-b's admin approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
		}
		if w := e.call(tc.approver(), http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
			t.Fatalf("%s approving = %d %s, want 200", tc.who, w.Code, w.Body)
		}
		if got := e.storedDeciders(tc.unit); !slices.Equal(kindsOf(got), []string{"egress_domain"}) {
			t.Errorf("%s: after approval stored %v, want the egress rule", tc.unit.Name, got)
		}
	}
	// The unit admin whose grant is on ua-a1 cannot approve a change for ua-a1 itself: nobody
	// decides the rules of a unit they govern directly.
	ch := e.pending(e.call(e.adminA, http.MethodPut, decidersPath(e.a1), decidersBody(mcpPeersRule())))
	if w := e.call(e.adminA1, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(t, w) {
		t.Errorf("ua-a1's own admin approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
}

// TestDecidersUnitAdminOwnUnitRefused: nobody sets the deciders of a unit they govern directly, so a
// unit admin cannot name themselves or a peer as the one who approves their own unit's holds. A unit
// outside their subtree is refused alike, and nothing is held or stored.
func TestDecidersUnitAdminOwnUnitRefused(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for _, tc := range []struct {
		what string
		c    *http.Cookie
		unit types.GovernanceUnit
	}{
		{"ua-a's admin on ua-a", e.adminA, e.a},
		{"ua-a1's admin on ua-a1", e.adminA1, e.a1},
		{"ua-a's admin on ua-b", e.adminA, e.b},
		{"a member with no grant on ua-a", e.member, e.a},
	} {
		if w := e.call(tc.c, http.MethodPut, decidersPath(tc.unit), decidersBody(egressOwnerRule())); !scopeRefused(t, w) {
			t.Errorf("%s = %d %s, want 403 unit_scope_denied", tc.what, w.Code, w.Body)
		}
	}
	if n := e.pendingCount(); n != 0 {
		t.Errorf("%d changes held after the refusals, want none", n)
	}
	for _, u := range []types.GovernanceUnit{e.a, e.a1, e.b} {
		if got := e.storedDeciders(u); got != nil {
			t.Errorf("%s stored %v, want nothing", u.Name, got)
		}
	}
}

// TestDecidersSecurityWriteFollowsSwitch: a security admin's write applies directly with the switch
// off, is held with it on, and the admin token is the audited break-glass.
func TestDecidersSecurityWriteFollowsSwitch(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(egressOwnerRule()))
	if w.Code != http.StatusOK {
		t.Fatalf("switch off: security write = %d %s, want 200", w.Code, w.Body)
	}
	var saved types.UnitDeciders
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.UnitID != e.a.ID || len(saved.Rules) != 1 {
		t.Errorf("200 body = %s (%v), want the unit's stored deciders", w.Body, err)
	}
	if got := e.storedDeciders(e.a); !slices.Equal(kindsOf(got), []string{"egress_domain"}) || e.pendingCount() != 0 {
		t.Errorf("switch off: stored %v with %d pending, want the rule and none held", got, e.pendingCount())
	}
	if rows := e.audits("governance.deciders.set"); len(rows) != 1 || rows[0].Target != e.a.ID.String() {
		t.Errorf("governance.deciders.set rows = %d, want one targeting ua-a", len(rows))
	}
	// Clearing is the same write with no rules.
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody()); w.Code != http.StatusOK {
		t.Fatalf("clearing = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.storedDeciders(e.a); got != nil {
		t.Errorf("after clearing stored %v, want none", got)
	}

	on := newUnitAdminEnv(t, true)
	ch := on.pending(on.call(on.alice, http.MethodPut, decidersPath(on.a), decidersBody(mcpPeersRule())))
	if got := on.storedDeciders(on.a); got != nil {
		t.Fatalf("switch on: stored %v before approval", got)
	}
	if w := on.call(on.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Errorf("switch on: the proposer approving = %d %s, want 403", w.Code, w.Body)
	}
	on.approve(ch.ID)
	if got := on.storedDeciders(on.a); !slices.Equal(kindsOf(got), []string{"mcp_call"}) {
		t.Errorf("switch on: after approval stored %v, want the mcp_call rule", got)
	}
	if w := on.admin(http.MethodPut, decidersPath(on.a1), decidersBody(egressOwnerRule())); w.Code != http.StatusOK {
		t.Fatalf("switch on: admin token write = %d %s, want 200", w.Code, w.Body)
	}
	// The env's own unit writes, made by the admin token with the switch on, leave bypass rows of the
	// unit kind; the deciders write leaves exactly one of its kind, naming ua-a1.
	var deciders []types.AuditEvent
	for _, row := range on.audits("governance.change.bypass") {
		var data map[string]any
		if json.Unmarshal(row.Data, &data) == nil && data["target_kind"] == govKindUnitDeciders {
			deciders = append(deciders, row)
		}
	}
	if len(deciders) != 1 || deciders[0].Target != on.a1.ID.String() {
		t.Errorf("governance.change.bypass rows for deciders = %d, want one for the admin token's write on ua-a1", len(deciders))
	}
}

// TestDecidersParentLock409: the knob "deciders" locks the rules of every unit below the locker, and a
// unit cannot lock it while a unit below states rules.
func TestDecidersParentLock409(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody(egressOwnerRule())); w.Code != http.StatusOK {
		t.Fatalf("seed ua-a1's rules = %d %s", w.Code, w.Body)
	}
	lock := e.restate(e.a, map[string]any{"locked_knobs": []string{"deciders"}})
	w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a.ID.String(), lock)
	if w.Code != http.StatusConflict || wireReason(t, w) != "unit_knob_locked" || !strings.Contains(w.Body.String(), "ua-a1") {
		t.Fatalf("locking deciders over a unit with rules = %d %s, want 409 unit_knob_locked naming ua-a1", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody()); w.Code != http.StatusOK {
		t.Fatalf("clear ua-a1's rules = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a.ID.String(), lock); w.Code != http.StatusOK {
		t.Fatalf("locking deciders with no rules below = %d %s, want 200", w.Code, w.Body)
	}
	for what, c := range map[string]*http.Cookie{"security admin": e.alice, "unit admin above": e.adminA} {
		// ua-a1 is strictly below ua-a, so the scope passes and the lock is what refuses.
		w := e.call(c, http.MethodPut, decidersPath(e.a1), decidersBody(egressOwnerRule()))
		if w.Code != http.StatusConflict || wireReason(t, w) != "unit_knob_locked" {
			t.Errorf("%s writing under the lock = %d %s, want 409 unit_knob_locked", what, w.Code, w.Body)
		}
	}
	if w := e.admin(http.MethodPut, decidersPath(e.a1), decidersBody(egressOwnerRule())); w.Code != http.StatusConflict {
		t.Errorf("admin token under the lock = %d %s, want 409: a lock binds every writer", w.Code, w.Body)
	}
	// The locking unit's own rules are not under its own lock.
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(egressOwnerRule())); w.Code != http.StatusOK {
		t.Errorf("the locker's own rules = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.storedDeciders(e.a1); got != nil {
		t.Errorf("ua-a1 stored %v under the lock, want none", got)
	}
}

// TestDecidersHeldChangeStaleOnDecide: a held write is stale, and applies nothing, when the stored
// rules changed after it was proposed; and a lock added meanwhile refuses it.
func TestDecidersHeldChangeStaleOnDecide(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch := e.pending(e.call(e.adminA, http.MethodPut, decidersPath(e.a1), decidersBody(egressOwnerRule())))
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody(mcpPeersRule())); w.Code != http.StatusOK {
		t.Fatalf("direct write while one is held = %d %s, want 200", w.Code, w.Body)
	}
	w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != "governance_change_stale" {
		t.Fatalf("approving over a changed target = %d %s, want 409 governance_change_stale", w.Code, w.Body)
	}
	if got := e.storedDeciders(e.a1); !slices.Equal(kindsOf(got), []string{"mcp_call"}) {
		t.Errorf("stale approval changed the rules to %v, want the direct write's mcp_call", got)
	}
	if e.changeState(ch.ID) != types.GovernanceChangeStale {
		t.Errorf("change is %s, want stale", e.changeState(ch.ID))
	}

	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody()); w.Code != http.StatusOK { // a lock waits for the rules below it to clear
		t.Fatalf("clear ua-a1's rules = %d %s", w.Code, w.Body)
	}
	ch = e.pending(e.call(e.adminA, http.MethodPut, decidersPath(e.a2), decidersBody(egressOwnerRule())))
	lock := e.restate(e.a, map[string]any{"locked_knobs": []string{"deciders"}})
	if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a.ID.String(), lock); w.Code != http.StatusOK {
		t.Fatalf("lock = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != "unit_knob_locked" {
		t.Errorf("approving under a new lock = %d %s, want 409 unit_knob_locked", w.Code, w.Body)
	}
	if got := e.storedDeciders(e.a2); got != nil {
		t.Errorf("approval under a lock stored %v", got)
	}
}

// TestDecidersWriteRefusesSecurityAndCutTypes: security is always implied and the cut types are not
// deciders, so a rule naming one is a 400 invalid_deciders that names the rule.
func TestDecidersWriteRefusesSecurityAndCutTypes(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for name, who := range map[string]string{"security": "security", "manager": "manager", "delegate": "delegate", "unknown": "everyone"} {
		w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(
			egressOwnerRule(), map[string]any{"kinds": []string{"mcp_call"}, "who": []string{who}}))
		if w.Code != http.StatusBadRequest || wireReason(t, w) != "invalid_deciders" || !strings.Contains(w.Body.String(), "rules[1]") {
			t.Errorf("%s: %d %s, want 400 invalid_deciders naming rules[1]", name, w.Code, w.Body)
		}
	}
	for name, kind := range map[string]string{"tool_call": "tool_call", "reauth": "credential_reauth", "takeover": "human_takeover"} {
		w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(map[string]any{"kinds": []string{kind}}))
		if w.Code != http.StatusBadRequest || wireReason(t, w) != "invalid_deciders" || !strings.Contains(w.Body.String(), "rules[0]") {
			t.Errorf("%s: %d %s, want 400 invalid_deciders naming rules[0]", name, w.Code, w.Body)
		}
	}
	w := e.call(e.alice, http.MethodPut, decidersPath(e.a), `{"rules":[{"kinds":["mcp_call"],"match":{"hosts":["x"]}}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an unknown field (match is cut) = %d %s, want 400", w.Code, w.Body)
	}
	if got := e.storedDeciders(e.a); got != nil {
		t.Errorf("refused writes stored %v", got)
	}
}

// TestDecidersEmptyWhoMeansSecurityOnly: a rule with no who is accepted and stored as an empty list,
// so the kind is decided by the security tier alone.
func TestDecidersEmptyWhoMeansSecurityOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	w := e.call(e.alice, http.MethodPut, decidersPath(e.a), `{"rules":[{"kinds":["egress_domain"]},{"kinds":["mcp_call"],"who":[]}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("empty who = %d %s, want 200", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"who":[]`) || strings.Contains(w.Body.String(), `"who":null`) {
		t.Errorf("body %s, want who as an empty list", w.Body)
	}
	for _, r := range e.storedDeciders(e.a) {
		if len(r.Who) != 0 {
			t.Errorf("stored who %v, want none", r.Who)
		}
	}
}

// TestDecidersOwnerWithSecondHumanRefusedAtWrite and the credential, push and elevation rules the
// model sets: refused at the door, so no stored rule can contradict the decide path.
func TestDecidersOwnerWithSecondHumanRefusedAtWrite(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for name, rule := range map[string]map[string]any{
		"owner with second_human": {"kinds": []string{"mcp_call"}, "who": []string{"owner"}, "second_human": true},
		"owner on credential":     {"kinds": []string{"credential"}, "who": []string{"owner"}, "second_human": true},
		"owner on push_content":   {"kinds": []string{"push_content"}, "who": []string{"owner"}, "second_human": true},
		"owner on elevation":      {"kinds": []string{"elevation"}, "who": []string{"owner"}, "max_elevation": "1h"},
	} {
		w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(rule))
		if w.Code != http.StatusBadRequest || wireReason(t, w) != "invalid_deciders" {
			t.Errorf("%s: %d %s, want 400 invalid_deciders", name, w.Code, w.Body)
		}
	}
}

// TestDecidersCredentialPushRequireSecondHuman: credential and push_content rules must state
// second_human, and an elevation rule states max_elevation (15m to 24h) and no second_human.
func TestDecidersCredentialPushRequireSecondHuman(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for name, rule := range map[string]map[string]any{
		"credential without":      {"kinds": []string{"credential"}, "who": []string{"unit_admins"}},
		"push_content without":    {"kinds": []string{"push_content"}, "who": []string{"group:ops"}},
		"elevation second_human":  {"kinds": []string{"elevation"}, "second_human": true, "max_elevation": "1h"},
		"elevation no cap":        {"kinds": []string{"elevation"}, "who": []string{"unit_admins"}},
		"elevation cap too long":  {"kinds": []string{"elevation"}, "max_elevation": "25h"},
		"elevation cap too short": {"kinds": []string{"elevation"}, "max_elevation": "5m"},
	} {
		w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(rule))
		if w.Code != http.StatusBadRequest || wireReason(t, w) != "invalid_deciders" {
			t.Errorf("%s: %d %s, want 400 invalid_deciders", name, w.Code, w.Body)
		}
	}
	noEligible := decidersBody(map[string]any{"kinds": []string{"elevation"}, "who": []string{"unit_admins"}, "max_elevation": "4h"})
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), noEligible); w.Code != http.StatusBadRequest ||
		wireReason(t, w) != "invalid_deciders" || !strings.Contains(w.Body.String(), "eligible_requesters") {
		t.Errorf("elevation without eligible_requesters: %d %s, want 400 invalid_deciders naming eligible_requesters", w.Code, w.Body)
	}
	ok := decidersBody(
		map[string]any{"kinds": []string{"credential", "push_content"}, "who": []string{"unit_admins"}, "second_human": true, "max_scope": "once"},
		map[string]any{"kinds": []string{"elevation"}, "who": []string{"unit_admins", "group:support-leads"}, "max_elevation": "4h", "eligible_requesters": []string{"group:support-leads"}})
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), ok); w.Code != http.StatusOK {
		t.Errorf("credential, push and elevation rules = %d %s, want 200", w.Code, w.Body)
	}
}

// TestDecidersAlwaysNeverUnderRule: max_scope always is refused at the write, because an always
// decision persists a host onto the workspace beyond any run the rule governs.
func TestDecidersAlwaysNeverUnderRule(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	rule := map[string]any{"kinds": []string{"egress_domain"}, "who": []string{"owner"}, "max_scope": "always"}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(rule)); w.Code != http.StatusBadRequest || wireReason(t, w) != "invalid_deciders" {
		t.Errorf("max_scope always = %d %s, want 400 invalid_deciders", w.Code, w.Body)
	}
}

// TestDecidersWriteUnknownUnit: an unknown unit is a 404 to a security caller; to a unit admin it is
// the scope refusal, so the route does not list units.
func TestDecidersWriteUnknownUnit(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ghost := types.GovernanceUnit{ID: uuid.New(), Name: "ghost"}
	if w := e.call(e.alice, http.MethodPut, decidersPath(ghost), decidersBody(egressOwnerRule())); w.Code != http.StatusNotFound || wireReason(t, w) != "governance_unit_not_found" {
		t.Errorf("security on an unknown unit = %d %s, want 404 governance_unit_not_found", w.Code, w.Body)
	}
	if w := e.call(e.adminA, http.MethodPut, decidersPath(ghost), decidersBody(egressOwnerRule())); !scopeRefused(t, w) {
		t.Errorf("unit admin on an unknown unit = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
}

// TestDecidersGovernanceRead: GET /governance carries each unit's rules and, per kind, the unit whose
// rule governs it, every unit whose rule bounds it and the rule they leave once met, scoped to the
// caller's subtrees; with no rules anywhere the field is absent.
func TestDecidersGovernanceRead(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	read := func(c *http.Cookie) map[string]struct {
		Rules     []types.DeciderRule          `json:"rules"`
		From      map[string]string            `json:"from"`
		BoundedBy map[string][]string          `json:"bounded_by"`
		Effective map[string]types.DeciderRule `json:"effective"`
	} {
		w := e.call(c, http.MethodGet, "/api/v1/governance", "")
		var body struct {
			Deciders []struct {
				UnitID    string                       `json:"unit_id"`
				Rules     []types.DeciderRule          `json:"rules"`
				From      map[string]string            `json:"from"`
				BoundedBy map[string][]string          `json:"bounded_by"`
				Effective map[string]types.DeciderRule `json:"effective"`
			} `json:"deciders"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("GET /governance = %d %s", w.Code, w.Body)
		}
		out := map[string]struct {
			Rules     []types.DeciderRule          `json:"rules"`
			From      map[string]string            `json:"from"`
			BoundedBy map[string][]string          `json:"bounded_by"`
			Effective map[string]types.DeciderRule `json:"effective"`
		}{}
		for _, d := range body.Deciders {
			out[d.UnitID] = struct {
				Rules     []types.DeciderRule          `json:"rules"`
				From      map[string]string            `json:"from"`
				BoundedBy map[string][]string          `json:"bounded_by"`
				Effective map[string]types.DeciderRule `json:"effective"`
			}{d.Rules, d.From, d.BoundedBy, d.Effective}
		}
		return out
	}
	if got := read(e.alice); len(got) != 0 {
		t.Fatalf("no rules anywhere: GET /governance deciders = %v, want none", got)
	}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a), decidersBody(egressOwnerRule())); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody(mcpPeersRule())); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	got := read(e.alice)
	a, a1, a2 := e.a.ID.String(), e.a1.ID.String(), e.a2.ID.String()
	if g := got[a]; len(g.Rules) != 1 || g.From["egress_domain"] != a || len(g.From) != 1 {
		t.Errorf("ua-a = %+v, want its egress rule governed by itself", g)
	}
	if g := got[a1]; len(g.Rules) != 1 || g.From["egress_domain"] != a || g.From["mcp_call"] != a1 {
		t.Errorf("ua-a1 = %+v, want mcp_call from itself and egress_domain from ua-a", g)
	}
	if g := got[a2]; len(g.Rules) != 0 || g.From["egress_domain"] != a {
		t.Errorf("ua-a2 = %+v, want no rules of its own and egress_domain from ua-a", g)
	}
	if _, in := got[e.b.ID.String()]; in {
		t.Errorf("ua-b is listed with no rule above or on it")
	}
	if seen := read(e.adminB); len(seen) != 0 {
		t.Errorf("ua-b's admin sees %v, want nothing outside their subtree", seen)
	}
	if seen := read(e.adminA1); len(seen) != 1 || seen[a1].From["mcp_call"] != a1 {
		t.Errorf("ua-a1's admin sees %v, want ua-a1's entry only", seen)
	}
	// bounded_by lists every unit with a rule for the kind, nearest first; effective is their meet.
	if g := got[a1]; !slices.Equal(g.BoundedBy["egress_domain"], []string{a}) || !slices.Equal(g.BoundedBy["mcp_call"], []string{a1}) ||
		!slices.Equal(g.Effective["egress_domain"].Who, []string{"owner", "group:eng-leads"}) || !g.Effective["mcp_call"].SecondHuman {
		t.Errorf("ua-a1 = %+v, want egress_domain bounded by ua-a and mcp_call by itself, each its one rule", g)
	}
	narrower := map[string]any{"kinds": []string{"egress_domain"}, "who": []string{"group:eng-leads"}, "max_scope": "once"}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody(mcpPeersRule(), narrower)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	got = read(e.alice)
	if g := got[a1]; g.From["egress_domain"] != a1 || !slices.Equal(g.BoundedBy["egress_domain"], []string{a1, a}) ||
		!slices.Equal(g.Effective["egress_domain"].Who, []string{"group:eng-leads"}) || g.Effective["egress_domain"].MaxScope != types.ScopeOnce ||
		!slices.Equal(g.Effective["egress_domain"].Kinds, []string{"egress_domain"}) {
		t.Errorf("ua-a1 = %+v, want egress_domain from itself, bounded by itself then ua-a, met to group:eng-leads once", g)
	}
	if g := got[a2]; !slices.Equal(g.BoundedBy["egress_domain"], []string{a}) || g.Effective["egress_domain"].MaxScope != types.ScopeRun {
		t.Errorf("ua-a2 = %+v, want egress_domain bounded by ua-a alone", g)
	}
}

// TestDecidersReparentUnderLockRefused: a unit write that moves a unit, or one with units below it, whose
// rules are set under a unit that locks the deciders knob is a 409 naming them: the rules would otherwise
// freeze under the lock. Once they are cleared the move applies.
func TestDecidersReparentUnderLockRefused(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	lock := e.restate(e.a, map[string]any{"locked_knobs": []string{"deciders"}})
	if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a.ID.String(), lock); w.Code != http.StatusOK {
		t.Fatalf("lock = %d %s", w.Code, w.Body)
	}
	bc := e.child(e.b)
	for _, u := range []types.GovernanceUnit{e.b, bc} {
		if w := e.call(e.alice, http.MethodPut, decidersPath(u), decidersBody(egressOwnerRule())); w.Code != http.StatusOK {
			t.Fatalf("rules for %s = %d %s", u.Name, w.Code, w.Body)
		}
	}
	move := uaUnitBody("ua-b", e.a.ID, uaDomains("pypi.org"), nil)
	path := "/api/v1/governance/units/" + e.b.ID.String()
	w := e.admin(http.MethodPut, path, move)
	if w.Code != http.StatusConflict || wireReason(t, w) != "unit_knob_locked" || !strings.Contains(w.Body.String(), "ua-b") || !strings.Contains(w.Body.String(), bc.Name) {
		t.Fatalf("moving units with rules under the lock = %d %s, want 409 unit_knob_locked naming ua-b and %s", w.Code, w.Body, bc.Name)
	}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.b), decidersBody()); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	w = e.admin(http.MethodPut, path, move)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), bc.Name) || strings.Contains(w.Body.String(), "ua-b,") {
		t.Fatalf("moving a unit with rules below it = %d %s, want 409 naming only %s", w.Code, w.Body, bc.Name)
	}
	if w := e.call(e.alice, http.MethodPut, decidersPath(bc), decidersBody()); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := e.admin(http.MethodPut, path, move); w.Code != http.StatusOK {
		t.Errorf("moving with every rule cleared = %d %s, want 200", w.Code, w.Body)
	}
}

// TestDecidersBoundaryLockOverRulesRefused: a boundary write that newly locks the deciders knob while a
// unit states rules is a 409 naming the unit, as a unit's own lock is; with the rules cleared it applies.
// The held form is TestDecidersBoundaryLockHeldRefusedAtApproval.
func TestDecidersBoundaryLockOverRulesRefused(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody(egressOwnerRule())); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	body := `{"mode":"hard_limits","spec":{},"limits":{},"locked_knobs":["deciders"]}`
	w := e.call(e.alice, http.MethodPut, "/api/v1/governance/boundary", body)
	if w.Code != http.StatusConflict || wireReason(t, w) != "unit_knob_locked" || !strings.Contains(w.Body.String(), "ua-a1") {
		t.Fatalf("boundary lock over rules = %d %s, want 409 unit_knob_locked naming ua-a1", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody()); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/boundary", body); w.Code != http.StatusOK {
		t.Errorf("boundary lock with no rules = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, decidersPath(e.a1), decidersBody(egressOwnerRule())); w.Code != http.StatusConflict {
		t.Errorf("rules under the boundary's lock = %d %s, want 409", w.Code, w.Body)
	}
}

// TestDecidersBoundaryLockHeldRefusedAtApproval: with the second-human switch on, a boundary lock held
// while no unit has rules is refused at approval once a unit has gained rules, and nothing is applied.
func TestDecidersBoundaryLockHeldRefusedAtApproval(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	body := `{"mode":"hard_limits","spec":{},"limits":{},"locked_knobs":["deciders"]}`
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/boundary", body))
	if w := e.admin(http.MethodPut, decidersPath(e.a1), decidersBody(egressOwnerRule())); w.Code != http.StatusOK {
		t.Fatalf("rules by the admin token = %d %s", w.Code, w.Body)
	}
	w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != "unit_knob_locked" || !strings.Contains(w.Body.String(), "ua-a1") {
		t.Fatalf("approving the lock over rules = %d %s, want 409 unit_knob_locked naming ua-a1", w.Code, w.Body)
	}
	if e.changeState(ch.ID) != types.GovernanceChangePending {
		t.Errorf("change is %s after the refusal, want still pending", e.changeState(ch.ID))
	}
}
