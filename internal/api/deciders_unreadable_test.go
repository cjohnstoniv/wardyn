// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pins of a decision made while the unit deciders cannot be read, on the fixture of deciders_resolve_test.go:
// a security admin and the admin token still decide, under the narrowest rule.

// dcUnreadable is each way the deciders read fails.
var dcUnreadable = map[string]func(*decidersStore){
	"rules":     func(s *decidersStore) { s.rulesErr = errors.New("deciders read failed") },
	"unit tree": func(s *decidersStore) { s.unitsErr = errors.New("units read failed") },
}

// TestDecidersUnreadableCapsAtOnce: while the rules or the unit tree cannot be read, a security admin and the
// admin token decide once at most. A wider scope is refused before the decision, a bodyless egress or Azure
// DevOps decision is stored as once, and a credential keeps its single mint.
func TestDecidersUnreadableCapsAtOnce(t *testing.T) {
	runBody := `{"reason":"t","decision_scope":"run"}`
	for name, broken := range dcUnreadable {
		t.Run(name, func(t *testing.T) {
			f := newDCFix(t)
			egress := dcRule(types.DeciderKindEgressDomain, types.DeciderOwner)
			egress.MaxScope = types.ScopeRun // were the rules read, run would pass
			f.st.setRules(f.team.ID, egress)
			broken(f.st)
			for _, tc := range []struct {
				name  string
				admin bool
				kind  string
				body  string
				code  int
				want  types.ApprovalScope // the stored decision_scope on a 200
			}{
				{name: "egress run", kind: types.DeciderKindEgressDomain, body: runBody, code: http.StatusForbidden},
				{name: "egress until", kind: types.DeciderKindEgressDomain, body: untilBody(), code: http.StatusForbidden},
				{name: "egress always", kind: types.DeciderKindEgressDomain, body: `{"reason":"t","decision_scope":"always"}`, code: http.StatusForbidden},
				{name: "admin token, egress run", admin: true, kind: types.DeciderKindEgressDomain, body: runBody, code: http.StatusForbidden},
				{name: "Azure DevOps run", kind: types.DeciderKindADOEscalation, body: runBody, code: http.StatusForbidden},
				{name: "credential run", kind: types.DeciderKindCredential, body: runBody, code: http.StatusForbidden},
				{name: "egress once", kind: types.DeciderKindEgressDomain, body: `{"reason":"t","decision_scope":"once"}`, code: http.StatusOK, want: types.ScopeOnce},
				{name: "egress bodyless", kind: types.DeciderKindEgressDomain, code: http.StatusOK, want: types.ScopeOnce},
				{name: "admin token, egress bodyless", admin: true, kind: types.DeciderKindEgressDomain, code: http.StatusOK, want: types.ScopeOnce},
				{name: "Azure DevOps bodyless", kind: types.DeciderKindADOEscalation, code: http.StatusOK, want: types.ScopeOnce},
				{name: "credential bodyless", kind: types.DeciderKindCredential, code: http.StatusOK, want: ""},
			} {
				ap := f.hold(tc.kind)
				c := dcSecurity(t, dcSec)
				if tc.admin {
					c = nil
				}
				reason := ""
				if tc.code == http.StatusForbidden {
					reason = "decision_scope_above_rule"
				}
				w := f.decide(c, ap, "deny", tc.body)
				f.wantStatus(tc.name, w, tc.code, reason, ap)
				if tc.code == http.StatusForbidden && !strings.Contains(w.Body.String(), "could not be read") {
					t.Errorf("%s: refusal %s does not say the deciders could not be read", tc.name, w.Body)
				}
				if got := f.row(ap).DecisionScope; tc.code == http.StatusOK && got != tc.want {
					t.Errorf("%s: stored decision_scope %q, want %q", tc.name, got, tc.want)
				}
			}
		})
	}
}

// TestDecidersUnreadableAuditSaysSo: a decision made while the rules cannot be read writes deciders_unreadable on
// its approval.decide row, with no basis and no rule. One made under readable rules writes no such key.
func TestDecidersUnreadableAuditSaysSo(t *testing.T) {
	for _, unreadable := range []bool{true, false} {
		f := newDCFix(t)
		f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderOwner))
		if unreadable {
			f.st.rulesErr = errDecidersTest
		}
		ap := f.hold(types.DeciderKindEgressDomain)
		f.wantStatus("security", f.decide(dcSecurity(t, dcSec), ap, "deny", ""), http.StatusOK, "", ap)
		rows := f.actionRows("approval.decide", ap.ID.String())
		if len(rows) != 1 {
			t.Fatalf("unreadable=%v: approval.decide rows = %d, want 1", unreadable, len(rows))
		}
		data := auditData(t, rows[0])
		got, has := data["deciders_unreadable"]
		_, hasBasis := data["basis"]
		_, hasRule := data["rule"]
		switch {
		case unreadable && (got != true || hasBasis || hasRule):
			t.Errorf("rules unreadable: data %s, want deciders_unreadable true and no basis or rule", rows[0].Data)
		case !unreadable && has:
			t.Errorf("rules readable: data %s carries deciders_unreadable", rows[0].Data)
		}
	}
}

// TestDecidersUnreadableNeedsASecondPerson: while the rules cannot be read, a security admin never decides a hold
// on their own run, and mayDecide says so. Another security admin decides, and the admin token passes as the
// audited break-glass, its bypass row naming the unreadable deciders.
func TestDecidersUnreadableNeedsASecondPerson(t *testing.T) {
	for name, broken := range dcUnreadable {
		t.Run(name, func(t *testing.T) {
			f := newDCFix(t, createdBy(dcSec))
			f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderOwner))
			broken(f.st)
			ap := f.hold(types.DeciderKindEgressDomain)
			if f.mayDecideAs(dcCtx(dcSec, oidc.RoleSecurityAdmin, nil, false), ap) {
				t.Error("mayDecide = true for a security admin on their own run with the rules unreadable")
			}
			w := f.decide(dcSecurity(t, dcSec), ap, "deny", "")
			f.wantStatus("security on their own run", w, http.StatusForbidden, "second_human_required", ap)
			if !strings.Contains(w.Body.String(), "unit deciders (unreadable)") {
				t.Errorf("refusal %s does not name the unreadable deciders", w.Body)
			}
			f.wantStatus("another security admin", f.decide(dcSecurity(t, dcSec2), ap, "deny", ""), http.StatusOK, "", ap)

			tok := f.hold(types.DeciderKindEgressDomain)
			f.wantStatus("admin token", f.decide(nil, tok, "deny", ""), http.StatusOK, "", tok)
			bypass := f.actionRows("approval.second_human.bypass", tok.ID.String())
			if len(bypass) != 1 || bypass[0].Outcome != "success" {
				t.Fatalf("bypass rows = %+v, want one success row", bypass)
			}
			if sw, _ := auditData(t, bypass[0])["switch"].(string); sw != "unit deciders (unreadable)" {
				t.Errorf("bypass switch = %q, want %q", sw, "unit deciders (unreadable)")
			}
		})
	}
}
