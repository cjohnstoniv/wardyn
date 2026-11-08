// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The chain pins of unit deciders, on the fixture of deciders_resolve_test.go: every rule for a kind from the
// run's unit up binds the hold. Its deciders are the entries all of them name, a second_human any of them sets
// holds, and approval.decide names the nearest unit with a rule.

// egressRule is an egress_domain rule naming who.
func egressRule(who ...string) types.DeciderRule {
	return dcRule(types.DeciderKindEgressDomain, who...)
}

var dcGroupEntry = types.DeciderGroupPrefix + dcGroup

// moveTeamUnder re-parents dc-team below p in the fake tree, adding p when the tree does not hold it.
func (f *dcFix) moveTeamUnder(p types.GovernanceUnit) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.team.ParentID = &p.ID
	units := []types.GovernanceUnit{f.team}
	for _, u := range f.st.units {
		if u.ID != f.team.ID && u.ID != p.ID {
			units = append(units, u)
		}
	}
	f.st.units = append(units, p)
}

// decidersEntry is unit's entry in GET /governance's deciders as the wire carries it.
func (f *dcFix) decidersEntry(unit uuid.UUID) map[string]json.RawMessage {
	f.t.Helper()
	var units []unitResolution
	for _, u := range f.st.units {
		units = append(units, unitResolution{Row: u})
	}
	views, err := f.srv.governanceDeciders(context.Background(), units)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, v := range views {
		if v.UnitID != unit {
			continue
		}
		b, _ := json.Marshal(v)
		var out map[string]json.RawMessage
		if err := json.Unmarshal(b, &out); err != nil {
			f.t.Fatal(err)
		}
		return out
	}
	f.t.Fatalf("no deciders entry for %s", unit)
	return nil
}

// TestDecidersChainMeet: under a parent rule [unit_admins, group] and a child rule [group], a group member
// decides, a unit admin gets the 404 anyone without rights gets, and the parent's second_human binds although
// the child's rule does not set it.
func TestDecidersChainMeet(t *testing.T) {
	parent := egressRule(types.DeciderUnitAdmins, dcGroupEntry)
	parent.SecondHuman = true
	setup := func(f *dcFix) {
		f.st.setRules(f.root.ID, parent)
		f.st.setRules(f.team.ID, egressRule(dcGroupEntry))
	}
	f := newDCFix(t)
	setup(f)
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("unit admin", f.decide(dcMember(t, dcAdmin), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	if f.mayDecideAs(dcCtx(dcAdmin, oidc.RoleUser, []string{}, false), ap) {
		t.Error("mayDecide = true for a unit admin the child's rule leaves out")
	}
	f.wantStatus("group member", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusOK, "", ap)

	own := newDCFix(t, createdBy(dcSec))
	setup(own)
	ap = own.hold(types.DeciderKindEgressDomain)
	if own.mayDecideAs(dcCtx(dcSec, oidc.RoleSecurityAdmin, nil, false), ap) {
		t.Error("mayDecide = true for a security admin on their own run under the parent's second_human")
	}
	own.wantStatus("security on their own run", own.decide(dcSecurity(t, dcSec), ap, "deny", ""), http.StatusForbidden, "second_human_required", ap)
	own.wantStatus("another security admin", own.decide(dcSecurity(t, dcSec2), ap, "deny", ""), http.StatusOK, "", ap)
}

// TestDecidersEmptyIntersectionSecurityOnly: a stored child rule that names nobody its parent names leaves the
// kind to the security tier, and GET /governance shows that as an effective who of [].
func TestDecidersEmptyIntersectionSecurityOnly(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.team.ID, egressRule(types.DeciderOwner))
	f.st.setRules(f.root.ID, egressRule(dcGroupEntry)) // the parent, narrowed after the child was stored
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("owner", f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusForbidden, "not_a_decider", ap)
	f.wantStatus("group member", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	f.wantStatus("unit admin", f.decide(dcMember(t, dcAdmin), ap, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, ap)
	f.wantStatus("security", f.decide(dcSecurity(t, dcSec), ap, "deny", ""), http.StatusOK, "", ap)

	entry := f.decidersEntry(f.team.ID)
	var effective map[string]map[string]json.RawMessage
	if err := json.Unmarshal(entry["effective"], &effective); err != nil {
		t.Fatalf("effective = %s: %v", entry["effective"], err)
	}
	if who := string(effective[types.DeciderKindEgressDomain]["who"]); who != "[]" {
		t.Errorf("effective egress_domain who = %s, want []", who)
	}
	var bounded map[string][]uuid.UUID
	if err := json.Unmarshal(entry["bounded_by"], &bounded); err != nil || !slices.Equal(bounded[types.DeciderKindEgressDomain], []uuid.UUID{f.team.ID, f.root.ID}) {
		t.Errorf("bounded_by = %s (%v), want egress_domain [team, root]", entry["bounded_by"], err)
	}
}

// TestDecidersParentNarrowedLaterBindsStoredChild: a parent rule narrowed after the child's was stored binds the
// child's holds at once, pending ones included; nothing refuses the stored child.
func TestDecidersParentNarrowedLaterBindsStoredChild(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, egressRule(types.DeciderOwner, dcGroupEntry))
	f.st.setRules(f.team.ID, egressRule(types.DeciderOwner, dcGroupEntry))
	first, second := f.hold(types.DeciderKindEgressDomain), f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("owner before the narrowing", f.decide(dcMember(t, dcOwner), first, "deny", ""), http.StatusOK, "", first)
	f.st.setRules(f.root.ID, egressRule(dcGroupEntry))
	f.wantStatus("owner after the narrowing", f.decide(dcMember(t, dcOwner), second, "deny", ""), http.StatusForbidden, "not_a_decider", second)
	f.wantStatus("group member after the narrowing", f.decide(dcMember(t, dcLead, dcGroup), second, "deny", ""), http.StatusOK, "", second)
}

// TestDecidersReparentUnderNarrowerParentMeets: a unit moved below a narrower parent keeps its rules, and its
// holds are met with the new parent's.
func TestDecidersReparentUnderNarrowerParentMeets(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, egressRule(types.DeciderOwner, dcGroupEntry))
	f.st.setRules(f.team.ID, egressRule(types.DeciderOwner, dcGroupEntry))
	narrow := types.GovernanceUnit{ID: uuid.New(), Name: "dc-narrow"}
	f.st.setRules(narrow.ID, egressRule(dcGroupEntry))
	ap := f.hold(types.DeciderKindEgressDomain)
	if !f.mayDecideAs(dcCtx(dcOwner, oidc.RoleUser, []string{}, false), ap) {
		t.Fatal("mayDecide = false for the owner before the move")
	}
	f.moveTeamUnder(narrow)
	f.wantStatus("owner after the move", f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusForbidden, "not_a_decider", ap)
	f.wantStatus("group member after the move", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusOK, "", ap)
	if got := f.st.rules[f.team.ID]; len(got) != 1 || !slices.Contains(got[0].Who, types.DeciderOwner) {
		t.Errorf("the moved unit's rules = %+v, want them kept", got)
	}
}

// TestDecidersGroupTokenNeedsStampTTL: a group decider matches an API token's group snapshot only when
// WARDYN_ROLE_STAMP_TTL bounds its age; a console session matches either way.
func TestDecidersGroupTokenNeedsStampTTL(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, egressRule(dcGroupEntry))
	ap := f.hold(types.DeciderKindEgressDomain)
	token := withAPITokenID(dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false), uuid.New())
	session := dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false)
	if f.mayDecideAs(token, ap) {
		t.Error("TTL off: a token in the group matches the group decider")
	}
	if !f.mayDecideAs(session, ap) {
		t.Error("TTL off: a session in the group does not match")
	}
	f.srv.cfg.RoleStampTTL = time.Hour
	if !f.mayDecideAs(token, ap) {
		t.Error("TTL on: a token in the group does not match")
	}
	if !f.mayDecideAs(session, ap) {
		t.Error("TTL on: a session in the group does not match")
	}
	f.wantStatus("session", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusOK, "", ap)
}

// TestDecidersRuleRefStaysNearest: under rules on the run's unit and above it, approval.decide's rule names the
// nearest unit with a rule for the kind.
func TestDecidersRuleRefStaysNearest(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, egressRule(types.DeciderUnitAdmins, dcGroupEntry))
	f.st.setRules(f.team.ID, egressRule(dcGroupEntry))
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("group member", f.decide(dcMember(t, dcLead, dcGroup), ap, "deny", ""), http.StatusOK, "", ap)
	rows := f.actionRows("approval.decide", ap.ID.String())
	if len(rows) != 1 {
		t.Fatalf("approval.decide rows = %d, want 1", len(rows))
	}
	data := auditData(t, rows[0])
	rule, _ := data["rule"].(map[string]any)
	if data["basis"] != dcGroupEntry || rule["unit"] != f.team.ID.String() || rule["kind"] != types.DeciderKindEgressDomain {
		t.Errorf("approval.decide data = %s, want basis %s and rule {%s, egress_domain}", rows[0].Data, dcGroupEntry, f.team.ID)
	}
}
