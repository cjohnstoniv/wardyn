// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSameHumanAgreesWithSameGovernanceHuman: the one distinct-human rule gives the answer the proposer check
// gives, for every case the proposer check has: a shared principal, a shared email in another case, an empty
// email on either side, and two people who share nothing.
func TestSameHumanAgreesWithSameGovernanceHuman(t *testing.T) {
	cases := []struct {
		name                       string
		propPrincipal, propEmail   string
		callerPrincipal, callerEml string
	}{
		{"same principal", "p1", "a@x.example", "p1", "other@x.example"},
		{"same email, other case", "p1", "a@x.example", "p2", "A@X.example"},
		{"empty caller email", "p1", "a@x.example", "p2", ""},
		{"empty proposer email", "p1", "", "p2", "a@x.example"},
		{"both emails empty", "p1", "", "p2", ""},
		{"nothing shared", "p1", "a@x.example", "p2", "b@x.example"},
	}
	for _, c := range cases {
		ch := types.GovernanceChange{ProposedBy: c.propPrincipal, ProposedByEmail: c.propEmail}
		want := sameHuman(c.propPrincipal, c.propEmail, c.callerPrincipal, c.callerEml)
		if got := sameGovernanceHuman(ch, c.callerPrincipal, c.callerEml); got != want {
			t.Errorf("%s: sameGovernanceHuman = %v, sameHuman = %v", c.name, got, want)
		}
	}
}

// TestMultiAndEffective: the value table's first columns. A kind with one decider is never multi; for any
// other kind the minimum, the held count and the security flag each make a change multi, and the count it
// needs is the larger of what it was held with and the minimum, never below one.
func TestMultiAndEffective(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		minimum  int
		held     int
		security bool
		multi    bool
		eff      int
	}{
		{"request, minimum 2", types.GovernanceTargetElevation, 2, 1, false, false, 2},
		{"image request, minimum 1", types.GovernanceTargetImageRequest, 1, 1, false, false, 1},
		{"node record, minimum 2", govKindRunNodeRecord, 2, 1, false, false, 2},
		{"other, unset, held 1", govKindUnit, 0, 1, false, false, 1},
		{"other, unset, held 3", govKindUnit, 0, 3, false, true, 3},
		{"other, 1, held 1", govKindUnit, 1, 1, false, true, 1},
		{"other, 2, held 1", govKindUnit, 2, 1, false, true, 2},
		{"other, 2, held 3", govKindUnit, 2, 3, false, true, 3},
		{"other, unset, security flag", govKindUnit, 0, 1, true, true, 1},
		{"other, unset, held 0 (a row from before)", govKindUnit, 0, 0, false, false, 1},
	}
	for _, c := range cases {
		s := &Server{cfg: Config{GovernanceMinApprovals: c.minimum}}
		ch := types.GovernanceChange{TargetKind: c.kind, RequiredApprovals: c.held, RequireSecurity: c.security}
		if got := s.multi(ch); got != c.multi {
			t.Errorf("%s: multi = %v, want %v", c.name, got, c.multi)
		}
		if got := s.effective(ch); got != c.eff {
			t.Errorf("%s: effective = %d, want %d", c.name, got, c.eff)
		}
	}
}

// TestRequiredApprovalsSnapshot: a kind with one decider, or an unset minimum, is held needing one approval
// and no security admin; with the minimum set, a store that keeps no counts table gives the minimum.
func TestRequiredApprovalsSnapshot(t *testing.T) {
	for _, c := range []struct {
		kind    string
		minimum int
		want    int
	}{
		{types.GovernanceTargetElevation, 2, 1},
		{govKindUnit, 0, 1},
		{govKindUnit, 3, 3},
	} {
		s := &Server{cfg: Config{GovernanceMinApprovals: c.minimum}}
		got, sec, err := s.requiredApprovals(t.Context(), c.kind, "k")
		if err != nil || got != c.want || sec {
			t.Errorf("requiredApprovals(%s, min %d) = %d, %v, %v; want %d, false, nil", c.kind, c.minimum, got, sec, err, c.want)
		}
	}
}

// TestReachOf: the page says cant_reach when too few people remain, naming count or security, and cant_confirm
// when the same shortfall holds with people the pool could not count.
func TestReachOf(t *testing.T) {
	two := []poolPerson{{Principal: "a", Tier: tierUnitAdmin}, {Principal: "b", Tier: tierSecurityAdmin}}
	for _, c := range []struct {
		name         string
		blk          approvalsRead
		kept         []keptApproval
		pool         poolResult
		reach, whyIs string
	}{
		{"enough", approvalsRead{StillNeeded: 1, RemainingEligible: 2}, nil, poolResult{Eligible: two}, reachReachable, ""},
		{"too few", approvalsRead{StillNeeded: 2, RemainingEligible: 1}, nil, poolResult{Eligible: two[:1]}, reachCantReach, cantReachByCount},
		{"too few, some uncountable", approvalsRead{StillNeeded: 2, RemainingEligible: 1}, nil,
			poolResult{Eligible: two[:1], Uncountable: []poolSource{{Why: uncountableUnconfirmed, Count: 1}}}, reachCantConfirm, ""},
		{"security wanted, none left", approvalsRead{RequireSecurity: true, StillNeeded: 1, RemainingEligible: 1}, nil,
			poolResult{Eligible: two[:1]}, reachCantReach, cantReachBySecurity},
		{"security wanted, one counted", approvalsRead{RequireSecurity: true, StillNeeded: 1, RemainingEligible: 1},
			[]keptApproval{{tier: tierSecurityAdmin}}, poolResult{Eligible: two[:1]}, reachReachable, ""},
	} {
		reach, why := reachOf(&c.blk, c.kept, c.pool)
		if reach != c.reach || why != c.whyIs {
			t.Errorf("%s: reach = %q, %q; want %q, %q", c.name, reach, why, c.reach, c.whyIs)
		}
	}
}

// TestChangeViewRedactsForTheReader: writeChangeView, the GET, take-back and recorded-approval answer of a
// held change, serves a pending change under the approval rules with its diff as the reader may see it. A unit
// admin reads no host path and no belongs_by, a security admin no host path, and a super admin the stored diff
// whole.
func TestChangeViewRedactsForTheReader(t *testing.T) {
	const planted = "/srv/planted-host-path"
	s, _ := govCovServer(t, &govCovStore{})
	ch := types.GovernanceChange{ID: uuid.New(), State: types.GovernanceChangePending, RequiredApprovals: 2,
		Diff: json.RawMessage(`{"belongs_by":["unit-a"],"after":{"ceiling":{"workspace_mounts":[{"source":"` + planted + `","target":"/home/agent/data"}]}}}`)}
	if !s.multi(ch) {
		t.Fatal("the fixture is not under the approval rules, so the approvals path is not read")
	}
	for _, c := range []struct {
		role                string
		hostPath, belongsBy bool
	}{
		{oidc.RoleUser, false, false},
		{oidc.RoleSecurityAdmin, false, true},
		{oidc.RoleAdmin, true, true},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), "sub-reader"), c.role))
		w := httptest.NewRecorder()
		s.writeChangeView(w, r, ch)
		var got types.GovernanceChange
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("%s: answer = %d %s, want 200 and a change", c.role, w.Code, w.Body)
		}
		diff := string(got.Diff)
		if diff == "null" || diff == "" {
			t.Fatalf("%s: the diff is not served", c.role)
		}
		if strings.Contains(diff, planted) != c.hostPath {
			t.Errorf("%s: the host path is served = %v, want %v: %s", c.role, !c.hostPath, c.hostPath, diff)
		}
		if strings.Contains(diff, "belongs_by") != c.belongsBy {
			t.Errorf("%s: belongs_by is served = %v, want %v: %s", c.role, !c.belongsBy, c.belongsBy, diff)
		}
	}
}
