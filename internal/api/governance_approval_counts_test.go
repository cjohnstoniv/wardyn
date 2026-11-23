// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The approval counts without a database: the kinds the page lists, who approves each, the changes that only
// a super admin decides, the pool status at each boundary, and the PUT's refusals that come before any read.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

const approvalCountsPath = "/api/v1/governance/approval-counts"

// countsServer is a server over a store with no database, with the minimum set to minimum.
func countsServer(t *testing.T, minimum int, mutate ...func(*Config)) *Server {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), newAuthzStore())
	cfg.OIDC = &oidc.Authenticator{}
	cfg.GovernanceMinApprovals = minimum
	for _, m := range mutate {
		m(&cfg)
	}
	return New(cfg)
}

// TestApprovalCountKindsAreDerived: the page lists every kind of the table except the single-decider kinds and
// the counts' own kind, computed here the same way, so a new held kind needs no edit.
func TestApprovalCountKindsAreDerived(t *testing.T) {
	var want []string
	for kind := range governanceChangeKinds {
		if !singleDecider(kind) && kind != govKindApprovalCounts {
			want = append(want, kind)
		}
	}
	slices.Sort(want)
	got := approvalCountKinds()
	if !slices.Equal(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	if !slices.Contains(got, govKindAssistantSettings) || approversOf(govKindAssistantSettings) != approversSuperAdmin {
		t.Errorf("assistant_settings is missing or not a super admin's: %v %q", got, approversOf(govKindAssistantSettings))
	}
	for _, gone := range []string{"governance_elevation", "governance_image_request", "run_node_record", "governance_approval_counts"} {
		if slices.Contains(got, gone) {
			t.Errorf("%s is listed", gone)
		}
	}
}

// TestApproversOfAKind: the four unit kinds by name, a super-admin kind by its reason, and the rest.
func TestApproversOfAKind(t *testing.T) {
	for kind, want := range map[string]string{
		govKindUnit: approversSecurityOrU, govKindMembership: approversSecurityOrU, govKindUnitAdmin: approversSecurityOrU,
		govKindUnitDeciders: approversSecurityOrU, govKindRoleMapping: approversSuperAdmin, govKindDelegate: approversSuperAdmin,
		govKindEgressBaseline: approversSecurity, govKindRunnerPoolUsePolicy: approversSecurity,
	} {
		if got := approversOf(kind); got != want {
			t.Errorf("approversOf(%s) = %q, want %q", kind, got, want)
		}
	}
}

// TestRaisingProbesNeedASuperAdmin: each probe is a change forChange gives the super-admin tier, where the kind's
// table tier is the security tier; it names no proposer; the unit probe sets a host path. No other kind has one.
func TestRaisingProbesNeedASuperAdmin(t *testing.T) {
	for _, kind := range []string{govKindRunnerPoolUsePolicy, govKindUnit} {
		probe, ok := raisingProbe(kind)
		if !ok {
			t.Fatalf("%s has no probe", kind)
		}
		if got := governanceChangeKinds[kind].forChange(probe).approver.reason; got != authz.ReasonAdminSurface {
			t.Errorf("%s probe: approver reason = %q, want %q", kind, got, authz.ReasonAdminSurface)
		}
		if governanceChangeKinds[kind].approver.reason == authz.ReasonAdminSurface {
			t.Errorf("%s: the table tier is already a super admin's", kind)
		}
		if probe.ProposedBy != "" || probe.ProposedByEmail != "" {
			t.Errorf("%s probe names a proposer: %q %q", kind, probe.ProposedBy, probe.ProposedByEmail)
		}
	}
	unit, _ := raisingProbe(govKindUnit)
	if !profilePayloadSetsHostPath(unit.Payload) {
		t.Errorf("the unit probe sets no host path: %s", unit.Payload)
	}
	for _, kind := range approvalCountKinds() {
		if _, ok := raisingProbe(kind); ok && kind != govKindRunnerPoolUsePolicy && kind != govKindUnit {
			t.Errorf("%s has a probe", kind)
		}
	}
}

func peopleN(n int) []poolPerson {
	out := make([]poolPerson, n)
	for i := range out {
		out[i] = poolPerson{Principal: "p" + strings.Repeat("x", i)}
	}
	return out
}

// TestPoolStatusAtEachBoundary: the proposer never decides their own change, so a pool of the count plus two is
// ok, the count plus one is exact, the count is short, and the count with someone uncountable is cant_confirm with
// the reason; the count shown is the pool less one, never below 0.
func TestPoolStatusAtEachBoundary(t *testing.T) {
	const required = 3
	for _, c := range []struct {
		pool        int
		uncountable []poolSource
		want, why   string
	}{
		{required + 2, nil, poolStatusOK, ""},
		{required + 1, nil, poolStatusExact, ""},
		{required, nil, poolStatusShort, ""},
		{0, nil, poolStatusShort, ""},
		{required + 2, []poolSource{{Why: uncountableUnconfirmed, Count: 1}}, poolStatusOK, ""},
		{required + 1, []poolSource{{Why: uncountableUnconfirmed, Count: 1}}, poolStatusExact, ""},
		{required, []poolSource{{Why: uncountableUnconfirmed, Count: 1}}, poolStatusCantConfirm, uncountableUnconfirmed},
		{required, []poolSource{{Why: uncountableTooMany}}, poolStatusCantConfirm, uncountableTooMany},
	} {
		got := poolStatus(poolResult{Eligible: peopleN(c.pool), Uncountable: c.uncountable}, required)
		if got.Status != c.want || got.Why != c.why || got.Count != max(c.pool-1, 0) {
			t.Errorf("pool %d, uncountable %v: %+v, want %s %q", c.pool, c.uncountable, got, c.want, c.why)
		}
	}
}

// TestWorstPoolIsShown: a kind is shown and judged on its worst pool: short beats cant_confirm beats exact beats
// ok, the fewer people win a tie, and the raised pool (first) wins a full tie, with no unit name.
func TestWorstPoolIsShown(t *testing.T) {
	unconfirmed := []poolSource{{Why: uncountableUnconfirmed, Count: 1}}
	pool := func(n int, unit string, raised bool, unc []poolSource) kindPool {
		return kindPool{res: poolResult{Eligible: peopleN(n), Uncountable: unc}, unitName: unit, raised: raised}
	}
	const required = 2
	for _, c := range []struct {
		name       string
		pools      []kindPool
		wantStatus string
		wantUnit   string
		wantRaised bool
	}{
		{"short beats cant_confirm with fewer people", []kindPool{pool(2, "A", false, nil), pool(1, "B", false, unconfirmed)}, poolStatusShort, "A", false},
		{"short in a larger unit beats cant_confirm", []kindPool{pool(1, "A", false, unconfirmed), pool(2, "B", false, nil)}, poolStatusShort, "B", false},
		{"cant_confirm beats exact", []kindPool{pool(3, "A", false, nil), pool(2, "B", false, unconfirmed)}, poolStatusCantConfirm, "B", false},
		{"exact beats ok", []kindPool{pool(9, "A", false, nil), pool(3, "B", false, nil)}, poolStatusExact, "B", false},
		{"fewer people on a tie", []kindPool{pool(9, "A", false, nil), pool(5, "B", false, nil)}, poolStatusOK, "B", false},
		{"the raised pool on a full tie", []kindPool{pool(4, "", true, nil), pool(4, "", false, nil)}, poolStatusOK, "", true},
		{"a smaller ordinary pool", []kindPool{pool(4, "", true, nil), pool(3, "", false, nil)}, poolStatusExact, "", false},
	} {
		got, raised := worstPool(c.pools, required)
		if got.Status != c.wantStatus || got.UnitName != c.wantUnit || raised != c.wantRaised {
			t.Errorf("%s: %+v raised=%v, want %s %q raised=%v", c.name, got, raised, c.wantStatus, c.wantUnit, c.wantRaised)
		}
	}
}

// TestApprovalCountsWithNoUnits: with no units, each unit kind is one pool with no unit name, and every kind is
// listed once; nothing is stored, so the default is the minimum.
func TestApprovalCountsWithNoUnits(t *testing.T) {
	srv := countsServer(t, 2)
	view, err := srv.approvalCountsViewOf(approvalCountsReads{in: &poolInputs{}})
	if err != nil {
		t.Fatal(err)
	}
	if view.Minimum != 2 || view.Highest != 2 || view.Default != (approvalCountsRule{Required: 2}) {
		t.Errorf("minimum %d highest %d default %+v, want 2, 2 and {2 false}", view.Minimum, view.Highest, view.Default)
	}
	if len(view.Kinds) != len(approvalCountKinds()) {
		t.Fatalf("%d kinds, want %d", len(view.Kinds), len(approvalCountKinds()))
	}
	for _, k := range view.Kinds {
		if k.Own || k.Required != 2 || k.Pool.Count != 0 || k.Pool.UnitName != "" {
			t.Errorf("%s = %+v, want the default and an empty pool with no unit", k.Kind, k)
		}
	}
}

// TestApprovalCountsEditNeedsTheHighest: the held edit's count is the highest in force: the minimum, every
// kind's own count and the default; and it needs no security rule of its own.
func TestApprovalCountsEditNeedsTheHighest(t *testing.T) {
	rows := []store.GovernanceApprovalCount{{Kind: store.GovernanceApprovalDefaultKind, Required: 3}, {Kind: govKindRoleMapping, Required: 5, RequireSecurity: true}}
	if got := highestOf(approvalCountsState(rows, 2), 2); got != 5 {
		t.Errorf("highest = %d, want 5", got)
	}
	if got := highestOf(approvalCountsState(nil, 4), 4); got != 4 {
		t.Errorf("highest with nothing stored = %d, want the minimum 4", got)
	}
	srv := countsServer(t, 2)
	n, sec, err := srv.requiredApprovals(t.Context(), govKindApprovalCounts, approvalCountsKey)
	if err != nil || n != 2 || sec {
		t.Errorf("requiredApprovals = %d %v %v, want 2 false nil", n, sec, err)
	}
}

func putCounts(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, srv, http.MethodPut, approvalCountsPath, adminToken, body)
}

// TestPutApprovalCountsOrder: the minimum unset is a 409 and local mode a 503, each before the body is read,
// the admin token included; the 409 comes first.
func TestPutApprovalCountsOrder(t *testing.T) {
	const bad = `{"default":{"required":0},"kinds":[{"kind":"no_such_kind","required":1}]}`
	if w := putCounts(t, countsServer(t, 0), bad); w.Code != http.StatusConflict || errorReason(w) != reasonGovernanceApprovalCountsMinimumUnset {
		t.Errorf("minimum unset = %d %s", w.Code, w.Body)
	}
	localPut := func(minimum int, bearer string) *httptest.ResponseRecorder {
		local := countsServer(t, minimum, func(c *Config) {
			c.LocalMode = true
			c.LocalOperator = "local:alice"
			c.OIDC = nil
		})
		req := httptest.NewRequest(http.MethodPut, approvalCountsPath, strings.NewReader(bad))
		req.Host = "127.0.0.1:8080"
		req.RemoteAddr = "127.0.0.1:54321"
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		panicFails(t, local.Handler()).ServeHTTP(w, req)
		return w
	}
	for _, bearer := range []string{"", adminToken} {
		if w := localPut(2, bearer); w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceApprovalCountsLocalMode {
			t.Errorf("local mode (bearer %q) = %d %s, want 503 %s", bearer, w.Code, w.Body, reasonGovernanceApprovalCountsLocalMode)
		}
		// Local mode with the minimum unset is the 409: the minimum is checked first.
		if w := localPut(0, bearer); w.Code != http.StatusConflict || errorReason(w) != reasonGovernanceApprovalCountsMinimumUnset {
			t.Errorf("local mode, minimum unset (bearer %q) = %d %s, want 409 %s", bearer, w.Code, w.Body, reasonGovernanceApprovalCountsMinimumUnset)
		}
	}
}

// TestPutApprovalCountsInvalid: one case for each value of the 400's arms. pass means the answer is not the 400:
// a count that passes is judged next against the people who can approve, which this store cannot read.
func TestPutApprovalCountsInvalid(t *testing.T) {
	for _, c := range []struct {
		name    string
		minimum int
		body    string
		pass    bool
	}{
		{"a kind below the minimum", 2, `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":1}]}`, false},
		{"a kind at the minimum", 2, `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":2}]}`, true},
		{"a kind at nine", 2, `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":9}]}`, true},
		{"a kind at ten", 2, `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":10}]}`, false},
		{"the default below the minimum", 2, `{"default":{"required":1},"kinds":[]}`, false},
		{"the default at ten", 2, `{"default":{"required":10},"kinds":[]}`, false},
		{"a kind at zero with a minimum of one", 1, `{"default":{"required":1},"kinds":[{"kind":"egress_baseline","required":0}]}`, false},
		{"a kind at one with a minimum of one", 1, `{"default":{"required":1},"kinds":[{"kind":"egress_baseline","required":1}]}`, true},
		{"an unknown kind", 2, `{"default":{"required":2},"kinds":[{"kind":"no_such_kind","required":2}]}`, false},
		{"the counts' own kind", 2, `{"default":{"required":2},"kinds":[{"kind":"governance_approval_counts","required":2}]}`, false},
		{"a kind twice", 2, `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":2},{"kind":"egress_baseline","required":3}]}`, false},
		{"an elevation", 2, `{"default":{"required":2},"kinds":[{"kind":"governance_elevation","required":2}]}`, false},
		{"an image request", 2, `{"default":{"required":2},"kinds":[{"kind":"governance_image_request","required":2}]}`, false},
		{"a run node record", 2, `{"default":{"required":2},"kinds":[{"kind":"run_node_record","required":2}]}`, false},
		{"an unknown field", 2, `{"default":{"required":2},"kinds":[],"extra":1}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := putCounts(t, countsServer(t, c.minimum), c.body)
			invalid := w.Code == http.StatusBadRequest && errorReason(w) == reasonGovernanceApprovalCountsInvalid
			if invalid == c.pass {
				t.Errorf("status %d %s, want pass %v", w.Code, w.Body, c.pass)
			}
			if invalid && !startsUpper(errorMessage(t, w)) {
				t.Errorf("refusal %q does not start with a capital letter", errorMessage(t, w))
			}
		})
	}
}

func startsUpper(s string) bool { return s != "" && strings.ToUpper(s[:1]) == s[:1] }

// errorMessage is the error sentence of a refusal.
func errorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode refusal %s: %v", w.Body, err)
	}
	return b.Error
}
