// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const egressBaselinePath = "/api/v1/governance/egress-baseline"

func egressBaselineServer(t *testing.T, sc types.SiteConfig) (*Server, *fakeSiteConfigStore, *harness) {
	t.Helper()
	fake := &fakeSiteConfigStore{cfg: sc}
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), fake, h
}

func TestEgressBaselineIsASecurityOperatorWrite(t *testing.T) {
	srv, fake, h := egressBaselineServer(t, types.SiteConfig{
		Egress:        &types.SiteEgress{BaselineHosts: []string{"old.corp.example"}},
		InternalHosts: []types.InternalHost{{HostSuffix: "corp.example"}, {HostSuffix: "Svc.Corp.Example", Baseline: true}},
	})
	const body = `{"baseline_hosts":[" LLM.corp.example","llm.corp.example","b.corp.example"],"internal_host_suffixes":["corp.example"]}`
	member := ssoSession(t, "sub-m", "m@corp.example", oidc.RoleUser)
	if w := doSSO(t, srv, http.MethodPut, egressBaselinePath, member, body); w.Code != http.StatusForbidden {
		t.Fatalf("member PUT = %d %s, want 403", w.Code, w.Body)
	}
	if w := doSSO(t, srv, http.MethodGet, egressBaselinePath, member, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member GET = %d, want 403", w.Code)
	}
	if fake.putSeen != nil || len(govCovAudits(h, egressBaselineWriteName)) != 0 {
		t.Fatal("a refused member write reached the store or the audit log")
	}

	secAdmin := ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin)
	var got egressBaselineBody
	if w := doSSO(t, srv, http.MethodGet, egressBaselinePath, secAdmin, ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil ||
		!slices.Equal(got.BaselineHosts, []string{"old.corp.example"}) || !slices.Equal(got.InternalHostSuffixes, []string{"svc.corp.example"}) {
		t.Fatalf("GET = %d %s, want both lists (the security tier cannot read GET /site-config)", w.Code, w.Body)
	}
	if w := doSSO(t, srv, http.MethodPut, egressBaselinePath, secAdmin, body); w.Code != http.StatusOK {
		t.Fatalf("security admin PUT = %d %s, want 200", w.Code, w.Body)
	}
	if got := fake.cfg.Egress.BaselineHosts; !slices.Equal(got, []string{"b.corp.example", "llm.corp.example"}) {
		t.Errorf("stored hosts = %v, want the normalized, de-duplicated, sorted set", got)
	}
	// The write flips the mark on the stored entries: set where named, cleared where not.
	if marks := egressBaselineState(fake.cfg).InternalHostSuffixes; !slices.Equal(marks, []string{"corp.example"}) {
		t.Errorf("stored marks = %v, want only corp.example", marks)
	}
	rows := govCovAudits(h, egressBaselineWriteName)
	if len(rows) != 1 || rows[0].Actor != "sub-s" {
		t.Fatalf("audit rows = %+v, want one by the writer", rows)
	}
	d := govCovAuditData(t, rows[0])
	if !jsonListIs(d["before"], "old.corp.example") || !jsonListIs(d["after"], "b.corp.example", "llm.corp.example") ||
		!jsonListIs(d["added"], "b.corp.example", "llm.corp.example") || !jsonListIs(d["removed"], "old.corp.example") ||
		!jsonListIs(d["suffixes_before"], "svc.corp.example") || !jsonListIs(d["suffixes_after"], "corp.example") ||
		!jsonListIs(d["suffixes_added"], "corp.example") || !jsonListIs(d["suffixes_removed"], "svc.corp.example") {
		t.Errorf("audit data = %v, want the before, after, added and removed of both lists", d)
	}
	if n := len(govCovAudits(h, "governance.change.bypass")); n != 0 {
		t.Errorf("%d break-glass rows for a plain write", n)
	}
	if n := len(govCovAudits(h, "site_config.write")); n != 0 {
		t.Errorf("%d site_config.write rows: the baseline write is audited on its own action", n)
	}

	// An empty replacement clears both: the document reads as it did before the field existed.
	if w := doSSO(t, srv, http.MethodPut, egressBaselinePath, secAdmin, `{"baseline_hosts":[],"internal_host_suffixes":[]}`); w.Code != http.StatusOK ||
		fake.cfg.Egress != nil || len(egressBaselineState(fake.cfg).InternalHostSuffixes) != 0 {
		t.Errorf("clear = %d, egress %+v, want 200 and nothing declared", w.Code, fake.cfg.Egress)
	}
}

func jsonListIs(v any, want ...string) bool {
	l, _ := v.([]any)
	got := make([]string, len(l))
	for i, x := range l {
		got[i], _ = x.(string)
	}
	return slices.Equal(got, want)
}

func TestEgressBaselineRefusesAnythingButExactHostnames(t *testing.T) {
	srv, fake, _ := egressBaselineServer(t, types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example"}}})
	secAdmin := ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin)
	for _, bad := range []string{"*.corp.example", "https://llm.corp.example", "llm.corp.example:8443", "llm.corp.example/v1",
		"10.1.2.3", "::1", "localhost", "", "llm.corp.example.", "a b.corp.example"} {
		body, _ := json.Marshal(map[string]any{"baseline_hosts": []string{"ok.corp.example", bad}})
		w := doSSO(t, srv, http.MethodPut, egressBaselinePath, secAdmin, string(body))
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonEgressBaselineInvalid {
			t.Errorf("host %q = %d %s, want 400 %s", bad, w.Code, errorReason(w), reasonEgressBaselineInvalid)
		}
	}
	// A suffix is held to the same shape, must name a stored internal_hosts entry, and must not be a
	// public suffix (it would mark every host under a registry-controlled name).
	for _, bad := range []string{"*.corp.example", "corp.example:443", "10.1.2.3", "other.example", "co.uk", "github.io"} {
		body, _ := json.Marshal(map[string]any{"internal_host_suffixes": []string{bad}})
		w := doSSO(t, srv, http.MethodPut, egressBaselinePath, secAdmin, string(body))
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonEgressBaselineInvalid {
			t.Errorf("suffix %q = %d %s, want 400 %s", bad, w.Code, errorReason(w), reasonEgressBaselineInvalid)
		}
	}
	if fake.putSeen != nil {
		t.Error("a refused value reached the store")
	}
}

// A suffix naming no stored entry is refused even when it is well-formed, and a public suffix is
// refused even when an entry declares it: the width guard is on the mark, not on the declaration.
func TestEgressBaselineRefusesAPublicSuffixEvenWhenDeclared(t *testing.T) {
	srv, fake, _ := egressBaselineServer(t, types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "co.uk"}}})
	secAdmin := ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, srv, http.MethodPut, egressBaselinePath, secAdmin, `{"internal_host_suffixes":["co.uk"]}`); w.Code != http.StatusBadRequest || fake.putSeen != nil {
		t.Errorf("PUT = %d %s, want 400 with nothing stored", w.Code, w.Body)
	}
}

// PUT /site-config is not a door to the baseline: the egress block and the set of marked internal
// hosts must come back as stored, whoever asks and whether or not the second-human switch is on.
func TestPutSiteConfigNeverChangesTheEgressBaseline(t *testing.T) {
	stored := types.SiteConfig{
		Egress:        &types.SiteEgress{BaselineHosts: []string{"llm.corp.example"}},
		InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", Baseline: true}, {HostSuffix: "other.example"}},
	}
	srv, fake, _ := egressBaselineServer(t, stored)
	keeps := func(label string) {
		t.Helper()
		st := egressBaselineState(fake.cfg)
		if !slices.Equal(st.BaselineHosts, []string{"llm.corp.example"}) || !slices.Equal(st.InternalHostSuffixes, []string{"corp.example"}) {
			t.Fatalf("%s: stored declaration = %+v, want it as it was", label, st)
		}
	}

	// A body silent about both, and a GET round trip, which names both unchanged, are accepted.
	for _, body := range []string{`{}`,
		`{"egress":{"baseline_hosts":["llm.corp.example"]}}`,
		`{"egress":{"baseline_hosts":["llm.corp.example"]},"internal_hosts":[{"host_suffix":"corp.example","baseline":true},{"host_suffix":"other.example"}]}`,
		`{"internal_hosts":[{"host_suffix":"corp.example","baseline":true},{"host_suffix":"other.example","cidrs":["10.0.0.0/8"]}]}`,
	} {
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s, want 200", body, w.Code, w.Body)
		}
		keeps("after PUT " + body)
	}
	// A changed or cleared block, an added, dropped or moved mark, and a removed marked entry are
	// governance writes: refused here, even for the admin token.
	for _, body := range []string{
		`{"egress":{"baseline_hosts":["other.corp.example"]}}`,
		`{"egress":{"baseline_hosts":[]}}`,
		`{"egress":{"baseline_hosts":["*.x.example"]}}`,
		`{"internal_hosts":[{"host_suffix":"corp.example","baseline":true},{"host_suffix":"other.example","baseline":true}]}`,
		`{"internal_hosts":[{"host_suffix":"corp.example"},{"host_suffix":"other.example"}]}`,
		`{"internal_hosts":[{"host_suffix":"other.example"}]}`,
		`{"internal_hosts":[]}`,
	} {
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonSiteConfigEgressViaOwnRoute {
			t.Errorf("PUT %s = %d %s, want 400 %s", body, w.Code, errorReason(w), reasonSiteConfigEgressViaOwnRoute)
		}
		keeps("after refused PUT " + body)
	}
}

func TestInternalHostBaselineMarkIsValidatedAtTheSeedDoor(t *testing.T) {
	for _, bad := range []string{"10.0.0.5", "co.uk", "github.io"} {
		cfg := types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: bad, Baseline: true}}}
		if err := validateSiteConfig(cfg); err == nil {
			t.Errorf("a baseline mark on %q was accepted", bad)
		}
		cfg.InternalHosts[0].Baseline = false
		if bad != "10.0.0.5" && validateSiteConfig(cfg) != nil {
			t.Errorf("declaring %q without a mark must still be allowed", bad)
		}
	}
	if err := validateSiteConfig(types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", Baseline: true}}}); err != nil {
		t.Errorf("a marked corp.example = %v, want it accepted", err)
	}
}

// The grade endpoint, preflight and create all read the one egressBaseline: an internal-only
// allowlist is OPEN-graded (medium) today, and stays so on upgrade until the operator opts in.
func TestEgressGradeFollowsTheOperatorBaseline(t *testing.T) {
	const body = `{"spec":{"min_confinement_class":"CC2","allowed_domains":["llm.corp.example"]}}`
	level := func(sc types.SiteConfig) composer.RiskLevel {
		srv, _, _ := egressBaselineServer(t, sc)
		w := do(t, srv, http.MethodPost, "/api/v1/policies/grade", adminToken, body)
		var resp gradePolicyResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
			t.Fatalf("grade = %d %s", w.Code, w.Body)
		}
		for _, it := range resp.RiskAssessment {
			if it.Field == "allowed_domains" {
				return it.Level
			}
		}
		t.Fatal("no allowed_domains item")
		return ""
	}
	internal := []types.InternalHost{{HostSuffix: "corp.example"}}
	for _, tc := range []struct {
		name string
		sc   types.SiteConfig
		want composer.RiskLevel
	}{
		{"nothing declared", types.SiteConfig{}, composer.RiskMedium},
		{"upgrade default: an internal host not marked baseline", types.SiteConfig{InternalHosts: internal}, composer.RiskMedium},
		{"internal host marked baseline", types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", Baseline: true}}}, composer.RiskLow},
		{"declared baseline host", types.SiteConfig{Egress: &types.SiteEgress{BaselineHosts: []string{"llm.corp.example"}}}, composer.RiskLow},
	} {
		if got := level(tc.sc); got != tc.want {
			t.Errorf("%s: allowed_domains graded %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestEgressBaselineOfFoldsDeclaredHostsAndMarkedInternalHosts(t *testing.T) {
	got := egressBaselineOf(types.SiteConfig{
		Egress:        &types.SiteEgress{BaselineHosts: []string{"b.example", "a.example"}},
		InternalHosts: []types.InternalHost{{HostSuffix: "Corp.Example", Baseline: true}, {HostSuffix: "other.example"}},
	})
	if !slices.Equal(got.Hosts, []string{"a.example", "b.example"}) || !slices.Equal(got.Suffixes, []string{"corp.example"}) {
		t.Errorf("baseline = %+v", got)
	}
}

// With the second-human switch on, a human's replacement is held, never written.
func TestEgressBaselineReplacementByAHumanIsHeld(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	st := &govCovStore{proposeSaved: types.GovernanceChange{ID: govCovChangeID}}
	srv, _ := govCovServer(t, st)
	sec := govCovSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)

	w := doSSO(t, srv, http.MethodPut, egressBaselinePath, sec, `{"baseline_hosts":["llm.corp.example"]}`)
	ch := govCovHoldAnswer(t, w, st)
	if ch.TargetKind != govKindEgressBaseline || ch.Op != "replace" || ch.TargetKey != govEgressBaselineKey {
		t.Fatalf("stored change = %+v", ch)
	}
	var payload egressBaselineBody
	if err := json.Unmarshal(ch.Payload, &payload); err != nil || !slices.Equal(payload.BaselineHosts, []string{"llm.corp.example"}) {
		t.Fatalf("payload = %s (%v)", ch.Payload, err)
	}
	// A refused value is the direct 400, not a held change.
	st.proposed = nil
	if w := doSSO(t, srv, http.MethodPut, egressBaselinePath, sec, `{"baseline_hosts":["*.corp.example"]}`); w.Code != http.StatusBadRequest || len(st.proposed) != 0 {
		t.Errorf("wildcard = %d with %d held, want 400 and none", w.Code, len(st.proposed))
	}
}

// Baseline is a grading fact: the sidecar never reads it, and an older sidecar image refuses a key it
// does not know, so dispatch strips it and leaves the lift alone.
func TestProxyInternalHostsDropsTheBaselineMark(t *testing.T) {
	in := []types.InternalHost{{HostSuffix: "corp.example", CIDRs: []string{"10.0.0.0/8"}, Baseline: true}}
	out := proxyInternalHosts(in)
	if out[0].Baseline || out[0].HostSuffix != "corp.example" || !slices.Equal(out[0].CIDRs, []string{"10.0.0.0/8"}) {
		t.Errorf("out = %+v", out)
	}
	if !in[0].Baseline {
		t.Error("the stored declaration was mutated")
	}
}

// A seed never overwrites the database's value; one that differs only in the baseline mark differs.
func TestSiteConfigSeedTreatsTheBaselineMarkAsPartOfTheDeclaration(t *testing.T) {
	seed := &SiteConfigSeed{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", Baseline: true}}}
	sc := types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example"}}}
	if _, differs := seed.apply(&sc); !slices.Equal(differs, []string{seedSettingInternalHosts}) || sc.InternalHosts[0].Baseline {
		t.Errorf("differs = %v, stored baseline = %v", differs, sc.InternalHosts[0].Baseline)
	}
}

// Create, preflight and the policy preview all raise the class through enforcedConfinement, so the
// floor an api_key to an internal host trips follows the same baseline everywhere.
func TestEnforcedConfinementFloorFollowsTheBaseline(t *testing.T) {
	spec := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants:      []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"llm.corp.example"}`)}},
	}
	for _, tc := range []struct {
		name string
		sc   types.SiteConfig
		want types.ConfinementClass
	}{
		{"nothing declared", types.SiteConfig{}, types.CC3},
		{"upgrade default: internal host not marked", types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example"}}}, types.CC3},
		{"marked internal host", types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", Baseline: true}}}, types.CC2},
		{"declared host", types.SiteConfig{Egress: &types.SiteEgress{BaselineHosts: []string{"llm.corp.example"}}}, types.CC2},
	} {
		got, err := enforcedConfinement(spec, types.CC2, nil, egressBaselineOf(tc.sc))
		if err != nil || got != tc.want {
			t.Errorf("%s: enforced = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}
