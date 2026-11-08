// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The second-human, approval-rule, scope-cap, projection and no-rule pins of unit deciders, on the fixture of
// deciders_resolve_test.go.

// actionRows is every row of action the harness recorded for target.
func (f *dcFix) actionRows(action, target string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == action && ev.Target == target {
			out = append(out, ev)
		}
	}
	return out
}

// TestDecidersSecondHumanRefusesAuthor: under a rule's second_human, the run's own people never decide: an
// owner who is in the decider group, the sponsor of the service identity that created the run, and its
// recorded human author. Another member of the group decides.
func TestDecidersSecondHumanRefusesAuthor(t *testing.T) {
	rule := types.DeciderRule{Kinds: []string{types.DeciderKindCredential}, Who: []string{types.DeciderUnitAdmins, types.DeciderGroupPrefix + dcGroup}, SecondHuman: true}
	for _, tc := range []struct {
		name string
		run  func(*dcFix)
	}{
		{"owner in the group", createdBy(dcLead)},
		{"sponsor", dcServiceRun(dcLead, "sub-dc-someone")},
		{"human author", dcServiceRun("sub-dc-someone", dcLead)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDCFix(t, tc.run)
			f.st.setRules(f.team.ID, rule)
			ap := f.hold(types.DeciderKindCredential)
			if f.mayDecideAs(dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false), ap) {
				t.Error("mayDecide = true for the run's own person")
			}
			if w := f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""); w.Code == http.StatusOK {
				t.Fatalf("the run's own person decided it: %d %s", w.Code, w.Body)
			}
			f.wantStatus("another group member", f.decide(dcMember(t, dcLead2, dcGroup), ap, "deny", ""), http.StatusOK, "", ap)
		})
	}
}

// TestDecidersSecondHumanBindsSecurityOwnRun: a rule's second_human binds a security admin on their own run, as
// the deployment switches do; the refusal names the unit's deciders. Another security admin decides.
func TestDecidersSecondHumanBindsSecurityOwnRun(t *testing.T) {
	f := newDCFix(t, createdBy(dcSec))
	f.st.setRules(f.team.ID, types.DeciderRule{Kinds: []string{types.DeciderKindEgressDomain}, Who: []string{types.DeciderGroupPrefix + dcGroup}, SecondHuman: true})
	ap := f.hold(types.DeciderKindEgressDomain)
	if f.mayDecideAs(dcCtx(dcSec, oidc.RoleSecurityAdmin, nil, false), ap) {
		t.Error("mayDecide = true for a security admin on their own run")
	}
	w := f.decide(dcSecurity(t, dcSec), ap, "deny", "")
	f.wantStatus("security on their own run", w, http.StatusForbidden, "second_human_required", ap)
	if !strings.Contains(w.Body.String(), "unit deciders") {
		t.Errorf("refusal %s does not name the unit's deciders", w.Body)
	}
	f.wantStatus("another security admin", f.decide(dcSecurity(t, dcSec2), ap, "deny", ""), http.StatusOK, "", ap)
}

// TestDecidersAdminTokenBypassAudited: the admin token passes a rule's second_human as the audited break-glass it
// is for the switches: approval.second_human.bypass names the unit's deciders, and approval.decide records the
// basis admin_token and the rule.
func TestDecidersAdminTokenBypassAudited(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, types.DeciderRule{Kinds: []string{types.DeciderKindMCPCall}, Who: []string{types.DeciderUnitAdmins}, SecondHuman: true})
	ap := f.hold(types.DeciderKindMCPCall)
	f.wantStatus("admin token", f.decide(nil, ap, "deny", ""), http.StatusOK, "", ap)
	bypass := f.actionRows("approval.second_human.bypass", ap.ID.String())
	if len(bypass) != 1 || bypass[0].Outcome != "success" {
		t.Fatalf("bypass rows = %+v, want one success row", bypass)
	}
	if sw, _ := auditData(t, bypass[0])["switch"].(string); !strings.Contains(sw, "unit deciders") || !strings.Contains(sw, f.root.ID.String()) {
		t.Errorf("bypass switch = %q, want the deciders of unit %s", sw, f.root.ID)
	}
	decided := f.actionRows("approval.decide", ap.ID.String())
	if len(decided) != 1 {
		t.Fatalf("approval.decide rows = %d, want 1", len(decided))
	}
	data := auditData(t, decided[0])
	rule, _ := data["rule"].(map[string]any)
	if data["basis"] != "admin_token" || rule["unit"] != f.root.ID.String() || rule["kind"] != types.DeciderKindMCPCall {
		t.Errorf("approval.decide data = %s, want basis admin_token and rule {%s, mcp_call}", decided[0].Data, f.root.ID)
	}
}

// TestDecidersAuditNamesBasis: approval.decide names how each decider decided under the rule (owner,
// unit_admin:<unit>, group:<name>, security) and the rule's unit and kind.
func TestDecidersAuditNamesBasis(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderOwner, types.DeciderUnitAdmins, types.DeciderGroupPrefix+dcGroup))
	for _, tc := range []struct {
		c     *http.Cookie
		basis string
	}{
		{dcMember(t, dcOwner), "owner"},
		{dcMember(t, dcAdmin), "unit_admin:" + f.team.ID.String()},
		{dcMember(t, dcLead, dcGroup), "group:" + dcGroup},
		{dcSecurity(t, dcSec), "security"},
	} {
		ap := f.hold(types.DeciderKindEgressDomain)
		f.wantStatus(tc.basis, f.decide(tc.c, ap, "deny", ""), http.StatusOK, "", ap)
		rows := f.actionRows("approval.decide", ap.ID.String())
		if len(rows) != 1 {
			t.Fatalf("%s: approval.decide rows = %d", tc.basis, len(rows))
		}
		data := auditData(t, rows[0])
		rule, _ := data["rule"].(map[string]any)
		if data["basis"] != tc.basis || rule["unit"] != f.root.ID.String() || rule["kind"] != types.DeciderKindEgressDomain {
			t.Errorf("%s: approval.decide data = %s", tc.basis, rows[0].Data)
		}
	}
}

// newPolicyDecidersFix is newRuleFix's service-identity run, launched in a unit whose deciders hold rule.
func newPolicyDecidersFix(t *testing.T, rule *types.DeciderRule) (*ruleFix, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	ast := newAuthzStore()
	unit := types.GovernanceUnit{ID: uuid.New(), Name: "dc-ci"}
	f := &ruleFix{t: t, h: h,
		st:  &ruleStore{authzStore: ast, svc: types.ServiceIdentity{Name: "ci", Sponsor: "sub-sponsor", UserType: "ci-bots"}, rules: []types.ApprovalRule{testApprovalRule()}},
		aps: &ruleApprovals{authzApprovals: newAuthzApprovals(ast), rec: h.audit},
		run: types.AgentRun{ID: uuid.New(), CreatedBy: "service:ci", HumanAuthor: "sub-sponsor", State: types.RunRunning, Repo: "org/repo", UnitID: &unit.ID},
	}
	ast.runs[f.run.ID] = f.run
	rules := map[uuid.UUID][]types.DeciderRule{}
	if rule != nil {
		rules[unit.ID] = []types.DeciderRule{*rule}
	}
	cfg := baseTestConfig(h, &ruleDecidersStore{ruleStore: f.st, units: []types.GovernanceUnit{unit}, rules: rules})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = f.aps
	f.srv = New(cfg)
	f.tok = h.mintRunToken(t, f.run.ID)
	return f, unit.ID
}

// ruleDecidersStore is ruleStore with a unit tree and its deciders.
type ruleDecidersStore struct {
	*ruleStore
	units []types.GovernanceUnit
	rules map[uuid.UUID][]types.DeciderRule
}

func (s *ruleDecidersStore) ListUnitDeciders(context.Context) ([]types.UnitDeciders, error) {
	var out []types.UnitDeciders
	for unit, rules := range s.rules {
		out = append(out, types.UnitDeciders{UnitID: unit, Rules: rules})
	}
	return out, nil
}

func (s *ruleDecidersStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	return s.units, nil
}

// TestDecidersPolicyNeverMatchesWho: an approval rule matches no who entry; it decides only as the security
// tier's automation, so it decides under a rule that names a group, and under one that names nobody, with
// the basis policy. With no deciders rule its approval.decide row carries neither basis nor rule.
func TestDecidersPolicyNeverMatchesWho(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	for name, rule := range map[string]*types.DeciderRule{
		"group":         {Kinds: []string{types.DeciderKindEgressDomain}, Who: []string{types.DeciderGroupPrefix + dcGroup}},
		"security only": {Kinds: []string{types.DeciderKindEgressDomain}, Who: []string{}},
		"no rule":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			f, unit := newPolicyDecidersFix(t, rule)
			_, created := f.raise(types.ApprovalEgressDomain, ruleEgressScope)
			if s := f.state(created.ID); s != types.ApprovalApproved {
				t.Fatalf("state = %s, want APPROVED by the approval rule", s)
			}
			rows := f.rows("approval.decide")
			if len(rows) != 1 {
				t.Fatalf("approval.decide rows = %d, want 1", len(rows))
			}
			data := auditData(t, rows[0])
			if rule == nil {
				if _, has := data["basis"]; has {
					t.Errorf("no rule: data %s carries a basis", rows[0].Data)
				}
				if _, has := data["rule"]; has {
					t.Errorf("no rule: data %s carries a rule", rows[0].Data)
				}
				return
			}
			got, _ := data["rule"].(map[string]any)
			if data["basis"] != "policy" || got["unit"] != unit.String() || got["kind"] != types.DeciderKindEgressDomain {
				t.Errorf("data = %s, want basis policy and rule {%s, egress_domain}", rows[0].Data, unit)
			}
		})
	}
}

// TestDecidersPolicyRefusedUnderRuleSecondHuman: an approval rule is never a second human, so under a deciders
// rule's second_human it leaves the request PENDING with a second_human_required skip row.
func TestDecidersPolicyRefusedUnderRuleSecondHuman(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f, _ := newPolicyDecidersFix(t, &types.DeciderRule{Kinds: []string{types.DeciderKindEgressDomain}, Who: []string{types.DeciderUnitAdmins}, SecondHuman: true})
	_, created := f.raise(types.ApprovalEgressDomain, ruleEgressScope)
	skips := f.pending(created.ID)
	if len(skips) != 1 || skipReason(t, skips[0]) != "second_human_required" {
		t.Fatalf("skip rows = %+v, want one second_human_required", skips)
	}
}

// untilBody is a decision_scope until body half an hour out.
func untilBody() string {
	return `{"reason":"t","decision_scope":"until","decision_expires_at":"` + time.Now().Add(30*time.Minute).UTC().Format(time.RFC3339) + `"}`
}

// TestDecidersMaxScopeRefusesWider: a scope above the rule's max_scope is refused before the decision, for
// every decider, security included; a scope within it is decided.
func TestDecidersMaxScopeRefusesWider(t *testing.T) {
	f := newDCFix(t)
	egress := dcRule(types.DeciderKindEgressDomain, types.DeciderOwner)
	egress.MaxScope = types.ScopeOnce
	ado := dcRule(types.DeciderKindADOEscalation, types.DeciderOwner)
	ado.MaxScope = types.ScopeOnce
	cred := dcRule(types.DeciderKindCredential, types.DeciderUnitAdmins)
	cred.MaxScope = types.ScopeOnce
	f.st.setRules(f.team.ID, egress, ado, cred)
	for _, tc := range []struct {
		name string
		c    *http.Cookie
		kind string
		body string
		code int
	}{
		{"owner, egress run", dcMember(t, dcOwner), types.DeciderKindEgressDomain, `{"reason":"t","decision_scope":"run"}`, http.StatusForbidden},
		{"owner, egress until", dcMember(t, dcOwner), types.DeciderKindEgressDomain, untilBody(), http.StatusForbidden},
		{"security, egress run", dcSecurity(t, dcSec), types.DeciderKindEgressDomain, `{"reason":"t","decision_scope":"run"}`, http.StatusForbidden},
		{"owner, Azure DevOps run", dcMember(t, dcOwner), types.DeciderKindADOEscalation, `{"reason":"t","decision_scope":"run"}`, http.StatusForbidden},
		{"unit admin, credential run", dcMember(t, dcAdmin), types.DeciderKindCredential, `{"reason":"t","decision_scope":"run"}`, http.StatusForbidden},
		{"owner, egress once", dcMember(t, dcOwner), types.DeciderKindEgressDomain, `{"reason":"t","decision_scope":"once"}`, http.StatusOK},
		{"owner, Azure DevOps once", dcMember(t, dcOwner), types.DeciderKindADOEscalation, `{"reason":"t","decision_scope":"once"}`, http.StatusOK},
	} {
		ap := f.hold(tc.kind)
		reason := ""
		if tc.code == http.StatusForbidden {
			reason = "decision_scope_above_rule"
		}
		f.wantStatus(tc.name, f.decide(tc.c, ap, "deny", tc.body), tc.code, reason, ap)
	}
}

// TestDecidersMaxScopeClampsDefault: a decision sent with no scope takes the rule's cap when the kind's default
// reaches further: an egress decision under max_scope until is stored as once. Under max_scope run, and
// with no max_scope, the default is stored as it is without a rule.
func TestDecidersMaxScopeClampsDefault(t *testing.T) {
	for _, tc := range []struct {
		max  types.ApprovalScope
		want types.ApprovalScope
	}{
		{types.ScopeUntil, types.ScopeOnce},
		{types.ScopeOnce, types.ScopeOnce},
		{types.ScopeRun, ""},
		{"", ""},
	} {
		f := newDCFix(t)
		r := dcRule(types.DeciderKindEgressDomain, types.DeciderOwner)
		r.MaxScope = tc.max
		f.st.setRules(f.team.ID, r)
		ap := f.hold(types.DeciderKindEgressDomain)
		f.wantStatus("max_scope "+string(tc.max), f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusOK, "", ap)
		if got := f.row(ap).DecisionScope; got != tc.want {
			t.Errorf("max_scope %q: stored decision_scope %q, want %q", tc.max, got, tc.want)
		}
	}
}

// TestDecidersAlwaysRefusedAtDecide: under any rule for the kind, always is refused at the decision, for a
// security admin too: it would persist the host onto the workspace beyond any run the rule governs.
func TestDecidersAlwaysRefusedAtDecide(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderOwner))
	for _, verb := range []string{"approve", "deny"} {
		ap := f.hold(types.DeciderKindEgressDomain)
		f.wantStatus("security "+verb+" always", f.decide(dcSecurity(t, dcSec), ap, verb, `{"reason":"t","decision_scope":"always"}`),
			http.StatusForbidden, "decision_scope_above_rule", ap)
	}
}

// TestGetApprovalsProjectsDeciders: a PENDING row a rule governs lists who decides it, security last, for the
// owner and for a security admin; a row no rule governs has no deciders field.
func TestGetApprovalsProjectsDeciders(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderGroupPrefix+dcGroup))
	governed, free := f.hold(types.DeciderKindEgressDomain), f.hold(types.DeciderKindMCPCall)
	for name, c := range map[string]*http.Cookie{"owner": dcMember(t, dcOwner), "security": dcSecurity(t, dcSec)} {
		w := doSSO(t, f.srv, http.MethodGet, "/api/v1/approvals?state=&run_id="+f.run.ID.String(), c, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: GET /approvals = %d %s", name, w.Code, w.Body)
		}
		var rows []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatalf("%s: decode %s: %v", name, w.Body, err)
		}
		for _, row := range rows {
			deciders, has := row["deciders"]
			switch row["id"] {
			case governed.ID.String():
				list, _ := deciders.([]any)
				if len(list) != 2 || list[0] != "group:"+dcGroup || list[1] != "security" {
					t.Errorf("%s: governed row deciders = %v, want [group:%s security]", name, deciders, dcGroup)
				}
			case free.ID.String():
				if has {
					t.Errorf("%s: a row no rule governs carries deciders %v", name, deciders)
				}
			}
		}
	}
}

// TestDecidersKindsWithoutARunHoldGovernNone: image and elevation name requests that are not run holds
// in this build, and file_download governs a reviewed download only (desktop_review_decide_test.go), so a rule
// for one of them gives nobody another kind's run hold: a unit admin and a group member it names still get the
// plain 404 on every run hold below.
func TestDecidersKindsWithoutARunHoldGovernNone(t *testing.T) {
	f := newDCFix(t)
	var rules []types.DeciderRule
	for _, k := range []string{types.DeciderKindImage, types.DeciderKindFileDownload, types.DeciderKindElevation} {
		rules = append(rules, types.DeciderRule{Kinds: []string{k}, Who: []string{types.DeciderUnitAdmins, types.DeciderGroupPrefix + dcGroup}})
	}
	f.st.setRules(f.root.ID, rules...)
	for _, kind := range append(dcRuleKinds, "plain tool_call") {
		ap := f.hold(kind)
		f.wantStatus(kind+": unit admin", f.decide(dcMember(t, dcAdmin), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
		f.wantStatus(kind+": group member", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	}
}

// TestDecidersAbsentGoldenVerdicts: with no deciders rule anywhere, on a store that keeps deciders, a unit tree,
// a unit admin and a group, every verdict is the one a deployment without deciders gives, for mayDecide and for a
// real POST: kind x caller x four-eyes switch.
func TestDecidersAbsentGoldenVerdicts(t *testing.T) {
	type caller struct {
		name   string
		cookie func(t *testing.T) *http.Cookie
		ctx    context.Context
		deny   bool // the owner, with their egress_host grant denied
		admin  bool // the admin token
	}
	callers := []caller{
		{name: "owner", cookie: func(t *testing.T) *http.Cookie { return dcMember(t, dcOwner) }, ctx: dcCtx(dcOwner, oidc.RoleUser, []string{}, false)},
		{name: "owner without grant", cookie: func(t *testing.T) *http.Cookie { return dcMember(t, dcOwner) }, ctx: dcCtx(dcOwner, oidc.RoleUser, []string{}, false), deny: true},
		{name: "other member", cookie: func(t *testing.T) *http.Cookie { return dcMember(t, dcOther) }, ctx: dcCtx(dcOther, oidc.RoleUser, []string{}, false)},
		{name: "unit admin", cookie: func(t *testing.T) *http.Cookie { return dcMember(t, dcAdmin) }, ctx: dcCtx(dcAdmin, oidc.RoleUser, []string{}, false)},
		{name: "group member", cookie: func(t *testing.T) *http.Cookie { return dcMember(t, dcLead, dcGroup) }, ctx: dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false)},
		{name: "security", cookie: func(t *testing.T) *http.Cookie { return dcSecurity(t, dcSec) }, ctx: dcCtx(dcSec, oidc.RoleSecurityAdmin, nil, false)},
		{name: "admin token", ctx: context.Background(), admin: true},
	}
	// want is the answer without deciders: security and the admin token decide every kind; the owner decides
	// egress (with the grant), an Azure DevOps escalation and an MCP call, each refused by its own switch;
	// everyone else gets the plain 404.
	want := func(c caller, kind, sw string) int {
		switch {
		case c.admin || c.name == "security":
			return http.StatusOK
		case !strings.HasPrefix(c.name, "owner"):
			return http.StatusNotFound
		}
		switch kind {
		case types.DeciderKindEgressDomain:
			if c.deny || sw == envEgressSecondHuman {
				return http.StatusForbidden
			}
			return http.StatusOK
		case types.DeciderKindADOEscalation, types.DeciderKindMCPCall:
			if sw == envCapabilitySecondHuman {
				return http.StatusForbidden
			}
			return http.StatusOK
		}
		return http.StatusNotFound
	}
	for _, sw := range []string{"", envEgressSecondHuman, envCapabilitySecondHuman} {
		for _, kind := range append(dcRuleKinds, "plain tool_call") {
			for _, c := range callers {
				t.Run(sw+"/"+kind+"/"+c.name, func(t *testing.T) {
					if sw != "" {
						t.Setenv(sw, "1")
					}
					f := newDCFix(t, func(f *dcFix) {
						if c.deny {
							f.st.enf = map[string]bool{capEgressHost: true}
							f.st.grants = []types.CapabilityGrant{{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: dcOwner,
								Capability: capEgressHost, Value: dcHost, Effect: types.CapabilityDeny}}
						}
					})
					ap := f.hold(kind)
					var cookie *http.Cookie
					if !c.admin {
						cookie = c.cookie(t)
					}
					got := f.mayDecideAs(c.ctx, ap)
					w := f.decide(cookie, ap, "deny", "")
					if code := want(c, kind, sw); w.Code != code {
						t.Errorf("POST deny = %d %s, want %d", w.Code, w.Body, code)
					}
					if got != (w.Code == http.StatusOK) {
						t.Errorf("mayDecide = %v, but POST deny answered %d", got, w.Code)
					}
				})
			}
		}
	}
}

// decidersMDCases are TestMayDecideAgreesWithDecide's rows under unit deciders: mayDecide and a real POST give
// the same verdict for every caller a rule names, leaves out, or cannot be read for.
func decidersMDCases() []mdCase {
	type row struct {
		name   string
		rule   types.DeciderRule
		kind   string
		setup  func(*dcFix)
		cookie func(t *testing.T) *http.Cookie
		ctx    context.Context
		admin  bool
	}
	egress := func(who ...string) types.DeciderRule { return dcRule(types.DeciderKindEgressDomain, who...) }
	shEgress := egress(types.DeciderGroupPrefix + dcGroup)
	shEgress.SecondHuman = true
	owner := func(t *testing.T) *http.Cookie { return dcMember(t, dcOwner) }
	ownerCtx := dcCtx(dcOwner, oidc.RoleUser, []string{}, false)
	lead := func(t *testing.T) *http.Cookie { return dcMember(t, dcLead, dcGroup) }
	leadCtx := dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false)
	sec := func(t *testing.T) *http.Cookie { return dcSecurity(t, dcSec) }
	secCtx := dcCtx(dcSec, oidc.RoleSecurityAdmin, nil, false)
	deniedHost := func(f *dcFix) {
		f.st.enf = map[string]bool{capEgressHost: true}
		f.st.grants = []types.CapabilityGrant{{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: dcOwner,
			Capability: capEgressHost, Value: dcHost, Effect: types.CapabilityDeny}}
	}
	rows := []row{
		{name: "owner listed decides egress", rule: egress(types.DeciderOwner), kind: types.DeciderKindEgressDomain, cookie: owner, ctx: ownerCtx},
		{name: "owner listed, host denied by grant", rule: egress(types.DeciderOwner), kind: types.DeciderKindEgressDomain, setup: deniedHost, cookie: owner, ctx: ownerCtx},
		{name: "owner left out", rule: egress(types.DeciderUnitAdmins), kind: types.DeciderKindEgressDomain, cookie: owner, ctx: ownerCtx},
		{name: "unit admin decides a foreign run's egress", rule: egress(types.DeciderUnitAdmins), kind: types.DeciderKindEgressDomain,
			cookie: func(t *testing.T) *http.Cookie { return dcMember(t, dcAdmin) }, ctx: dcCtx(dcAdmin, oidc.RoleUser, []string{}, false)},
		{name: "unit admin who created the run", rule: dcRule(types.DeciderKindMCPCall, types.DeciderUnitAdmins), kind: types.DeciderKindMCPCall, setup: createdBy(dcAdmin),
			cookie: func(t *testing.T) *http.Cookie { return dcMember(t, dcAdmin) }, ctx: dcCtx(dcAdmin, oidc.RoleUser, []string{}, false)},
		{name: "group member decides a credential", rule: dcRule(types.DeciderKindCredential, types.DeciderGroupPrefix+dcGroup), kind: types.DeciderKindCredential, cookie: lead, ctx: leadCtx},
		{name: "group member with a truncated snapshot", rule: egress(types.DeciderGroupPrefix + dcGroup), kind: types.DeciderKindEgressDomain,
			cookie: func(t *testing.T) *http.Cookie { return uaSession(t, dcLead, []string{dcGroup}, true) }, ctx: dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, true)},
		{name: "member the rule does not name", rule: egress(types.DeciderGroupPrefix + dcGroup), kind: types.DeciderKindEgressDomain,
			cookie: func(t *testing.T) *http.Cookie { return dcMember(t, dcOther) }, ctx: dcCtx(dcOther, oidc.RoleUser, []string{}, false)},
		{name: "unknown stored decider type", rule: egress("manager"), kind: types.DeciderKindEgressDomain, cookie: owner, ctx: ownerCtx},
		{name: "security on a foreign run under a rule", rule: shEgress, kind: types.DeciderKindEgressDomain, cookie: sec, ctx: secCtx},
		{name: "security on their own run under second_human", rule: shEgress, kind: types.DeciderKindEgressDomain, setup: createdBy(dcSec), cookie: sec, ctx: secCtx},
		{name: "admin token under second_human", rule: shEgress, kind: types.DeciderKindEgressDomain, ctx: context.Background(), admin: true},
		{name: "rules unreadable: owner", rule: egress(types.DeciderOwner), kind: types.DeciderKindEgressDomain,
			setup: func(f *dcFix) { f.st.rulesErr = errDecidersTest }, cookie: owner, ctx: ownerCtx},
		{name: "rules unreadable: security", rule: egress(types.DeciderOwner), kind: types.DeciderKindEgressDomain,
			setup: func(f *dcFix) { f.st.rulesErr = errDecidersTest }, cookie: sec, ctx: secCtx},
	}
	var out []mdCase
	for _, r := range rows {
		out = append(out, mdCase{
			name:  "deciders: " + r.name,
			verbs: []string{"deny"},
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newDCFix(t, func(f *dcFix) {
					if r.setup != nil {
						r.setup(f)
					}
				})
				f.st.rules[f.team.ID] = []types.DeciderRule{r.rule}
				ap := f.hold(r.kind)
				return &mdFixture{srv: f.srv, ap: ap, run: f.run}, r.ctx, func(verb string) *httptest.ResponseRecorder {
					var c *http.Cookie
					if !r.admin {
						c = r.cookie(t)
					}
					return f.decide(c, ap, verb, "")
				}
			},
		})
	}
	return out
}

// errDecidersTest is a deciders read that fails.
var errDecidersTest = errors.New("deciders read failed")
