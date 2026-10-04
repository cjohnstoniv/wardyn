// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func govContactForTest() *policyref.Contact {
	return &policyref.Contact{Owner: "Platform security", Email: "sec@example.com"}
}

// the census guard

// rawProfileRead is a call to the store's raw profile reads: a stored composed row carries no
// authority of its own, so only governance_compose.go may turn one into authority.
var rawProfileRead = regexp.MustCompile(`\.(Get|List|Resolve)GovernanceProfile(s|Chain)?\(`)

// rawProfileReadsOutsideTheResolver is every file of this package (tests excluded) that calls a raw
// profile read, comments stripped, other than the resolver's own.
func rawProfileReadsOutsideTheResolver(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(base, "_test.go") || base == "governance_compose.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if rawProfileRead.MatchString(stripGoComments(t, string(src))) {
			bad = append(bad, base)
		}
	}
	return bad
}

// TestEveryProfileAuthorityReadGoesThroughTheResolver is the census as a test: the reader left on the
// raw row is the fail-open this design exists to prevent, and it passes every create-path test.
func TestEveryProfileAuthorityReadGoesThroughTheResolver(t *testing.T) {
	if bad := rawProfileReadsOutsideTheResolver(t, "."); len(bad) > 0 {
		t.Errorf("these files read governance profiles without the resolver (governance_compose.go): %v", bad)
	}
}

// TestCensusGuardFlagsARawRead proves the guard goes red: a source file with a raw read is named.
func TestCensusGuardFlagsARawRead(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sneaky.go", "package api\nfunc f(s *Server) { _, _ = s.cfg.Store.ListGovernanceProfiles(nil) }\n")
	write("chain.go", "package api\nfunc g(s *Server) { _, _ = s.cfg.Store.GetGovernanceProfileChain(nil, nil) }\n")
	write("commented.go", "package api\n// s.cfg.Store.GetGovernanceProfile(ctx, id) is what not to call\n")
	write("governance_compose.go", "package api\nfunc h(s *Server) { _, _ = s.cfg.Store.ListGovernanceProfiles(nil) }\n")
	write("raw_test.go", "package api\nfunc i(s *Server) { _, _ = s.cfg.Store.ListGovernanceProfiles(nil) }\n")
	got := rawProfileReadsOutsideTheResolver(t, dir)
	slices.Sort(got)
	if want := []string{"chain.go", "sneaky.go"}; !slices.Equal(got, want) {
		t.Errorf("flagged %v, want %v: the resolver's own file, a comment and a test double are not raw reads", got, want)
	}
}

// the resolved type is not the row

func TestResolvedProfileIsNotTheStoredRow(t *testing.T) {
	if reflect.TypeOf(ResolvedProfile{}) == reflect.TypeOf(types.GovernanceProfile{}) {
		t.Fatal("ResolvedProfile and types.GovernanceProfile are one type: a reader left on the raw row would compile")
	}
	if _, ok := reflect.TypeOf(governanceCeiling{}).FieldByName("Profile"); !ok ||
		reflect.TypeOf(governanceCeiling{}.Profile) != reflect.TypeOf(&ResolvedProfile{}) {
		t.Error("governanceCeiling.Profile is not a *ResolvedProfile")
	}
}

// byte for byte

// legacyCeilingFromProfile is ceilingFromProfile's body as 0.8.5 shipped it: the standalone path the
// resolver must reproduce exactly.
func (s *Server) legacyCeilingFromProfile(p *types.GovernanceProfile) governanceCeiling {
	spec := p.Ceiling.Clone()
	spec.Resources = s.inheritDeploymentResources(spec.Resources)
	kept, warns := reintersectGovernanceGrants(spec.EligibleGrants, s.cfg.DefaultPolicy.EligibleGrants, p.Name)
	spec.EligibleGrants = kept
	warns = append(warns, droppedPushRulesWarning(spec, s.cfg.DefaultPolicy, p.Name)...)
	return governanceCeiling{Spec: spec, Limits: p.Limits, Warnings: warns}
}

// TestStandaloneProfilesResolveByteForByte pins the standalone path against the 0.8.5 body over the
// existing ceiling fixtures: every profile stored before 0125 is standalone, and must resolve to
// exactly what it did.
func TestStandaloneProfilesResolveByteForByte(t *testing.T) {
	deployment := govDeployment()
	deployment.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}
	deployment.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"secret_name":"corp-key","host":"api.vendor.example"}`)}}
	deployment.Resources = &types.ResourceLimits{CPUMillis: 1500, MemoryMiB: 3072}
	srv := &Server{cfg: Config{DefaultPolicy: deployment}}

	withGrants := govProfile("grants")
	withGrants.Ceiling.EligibleGrants = []types.GrantSpec{
		{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"secret_name":"corp-key","host":"api.vendor.example"}`)},
		{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"secret_name":"gone","host":"gone.example"}`)},
	}
	withPush := govProfile("push")
	withPush.Ceiling.PushRules = &types.PushRulesSpec{DenyPaths: []string{"secrets/**"}}
	sized := govProfile("sized")
	sized.Ceiling.Resources = &types.ResourceLimits{CPUMillis: 500}
	limited := limitsProfile("limited", types.GovernanceLimits{
		DenyInteractive: true, MaxConcurrentRuns: 3, RunLimits: types.RunLimits{MaxEndAheadSec: 3600, AllowNoEnd: true}})
	for _, p := range []*types.GovernanceProfile{
		{ID: uuid.New(), Name: "empty"}, govProfile("plain"), holdProfile("holds"), withGrants, withPush, sized, limited,
	} {
		t.Run(p.Name, func(t *testing.T) {
			got, err := srv.composeChain([]types.GovernanceProfile{*p})
			if err != nil {
				t.Fatalf("compose: %v", err)
			}
			want := srv.legacyCeilingFromProfile(p)
			if !reflect.DeepEqual(got.Ceiling, want.Spec) || !reflect.DeepEqual(got.Limits, want.Limits) ||
				!slices.Equal(got.Warnings, want.Warnings) {
				t.Errorf("standalone profile %q no longer resolves as on 0.8.5\n got  %+v %+v %v\n want %+v %+v %v",
					p.Name, got.Ceiling, got.Limits, got.Warnings, want.Spec, want.Limits, want.Warnings)
			}
			if got.ID != p.ID || got.Name != p.Name || got.Composed {
				t.Errorf("identity = %v %q composed=%v, want the row's own and not composed", got.ID, got.Name, got.Composed)
			}
			gb, _ := json.Marshal(got.Ceiling)
			wb, _ := json.Marshal(want.Spec)
			if string(gb) != string(wb) {
				t.Errorf("resolved ceiling JSON differs:\n got  %s\n want %s", gb, wb)
			}
		})
	}
}

// the resolver itself

func TestResolverRefusesAChainThatIsNotACleanLine(t *testing.T) {
	srv := &Server{cfg: Config{DefaultPolicy: govDeployment()}}
	a := types.GovernanceProfile{ID: uuid.New(), Name: "a", Overlay: &types.CeilingOverlay{}}
	b := types.GovernanceProfile{ID: uuid.New(), Name: "b", Overlay: &types.CeilingOverlay{}}
	a.BaseProfileID, b.BaseProfileID = &b.ID, &a.ID
	root := types.GovernanceProfile{ID: uuid.New(), Name: "root"}
	mk := func(name string, base *types.GovernanceProfile) types.GovernanceProfile {
		return types.GovernanceProfile{ID: uuid.New(), Name: name, BaseProfileID: &base.ID, Overlay: &types.CeilingOverlay{}}
	}
	l1 := mk("l1", &root)
	l2 := mk("l2", &l1)
	l3 := mk("l3", &l2)
	orphan := mk("orphan", &root)
	noOverlay := types.GovernanceProfile{ID: uuid.New(), Name: "bad", BaseProfileID: &root.ID}
	for name, chain := range map[string][]types.GovernanceProfile{
		"empty":                  nil,
		"a loop":                 {a, b, a},
		"a self loop":            {{ID: a.ID, Name: "a", BaseProfileID: &a.ID, Overlay: &types.CeilingOverlay{}}},
		"depth four":             {l3, l2, l1, root},
		"cut short by the bound": {l3, l2, l1},
		"a missing base":         {orphan},
		"a base but no overlay":  {noOverlay, root},
		"a broken link":          {l2, root},
	} {
		if res, err := srv.composeChain(chain); err == nil {
			t.Errorf("%s composed to %+v, want a refusal (never a chain composed on the deployment)", name, res)
		}
	}
	if _, err := srv.composeChain([]types.GovernanceProfile{l2, l1, root}); err != nil {
		t.Errorf("a clean three-deep chain refused: %v", err)
	}
}

// TestResolverWarningsNameTheLeafOnly: a warning the resolver emits for a chain step reaches a
// member's 201, so it names the leaf profile and never a base's name, id or overlay.
func TestResolverWarningsNameTheLeafOnly(t *testing.T) {
	srv := &Server{cfg: Config{DefaultPolicy: govDeployment()}}
	base := types.GovernanceProfile{ID: uuid.New(), Name: "company-baseline-secret-name", Ceiling: types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}}
	mid := types.GovernanceProfile{ID: uuid.New(), Name: "division-secret-name", BaseProfileID: &base.ID, Overlay: &types.CeilingOverlay{
		AllowedDomains: &[]string{"api.anthropic.com", "dropped.example"}}}
	leaf := types.GovernanceProfile{ID: uuid.New(), Name: "team", BaseProfileID: &mid.ID, Overlay: &types.CeilingOverlay{
		AllowedDomains: &[]string{"api.anthropic.com", "other.example"}}}
	got, err := srv.composeChain([]types.GovernanceProfile{leaf, mid, base})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(got.Warnings) == 0 || len(got.AdminWarnings) == 0 {
		t.Fatalf("warnings = %v admin = %v, want both: the overlays name hosts their bases do not cover", got.Warnings, got.AdminWarnings)
	}
	for _, w := range got.Warnings {
		for _, secret := range []string{base.Name, mid.Name, base.ID.String(), mid.ID.String(), "dropped.example"} {
			if strings.Contains(w, secret) {
				t.Errorf("a member-facing warning names %q: %s", secret, w)
			}
		}
		if !strings.Contains(w, `"team"`) {
			t.Errorf("warning %q does not name the leaf", w)
		}
	}
	if !strings.Contains(strings.Join(got.AdminWarnings, "\n"), mid.Name) {
		t.Errorf("admin warnings = %v, want the base-level detail", got.AdminWarnings)
	}
	if got.Name != "team" || got.ID != leaf.ID {
		t.Errorf("identity = %q %v, want the leaf's", got.Name, got.ID)
	}
}

// TestResolverNeverInheritsAContact: a base's contact would disclose the base.
func TestResolverNeverInheritsAContact(t *testing.T) {
	srv := &Server{cfg: Config{DefaultPolicy: govDeployment()}}
	base := types.GovernanceProfile{ID: uuid.New(), Name: "base"}
	base.Contact = govContactForTest()
	leaf := types.GovernanceProfile{ID: uuid.New(), Name: "leaf", BaseProfileID: &base.ID, Overlay: &types.CeilingOverlay{}}
	got, err := srv.composeChain([]types.GovernanceProfile{leaf, base})
	if err != nil {
		t.Fatal(err)
	}
	if got.Contact != nil {
		t.Errorf("leaf contact = %+v, want none: a base's contact is never inherited", got.Contact)
	}
}

// TestResolverReintersectsGrantsAgainstEachBase: an overlay's grants are bounded by the resolved base
// at the step, not only by the deployment.
func TestResolverReintersectsGrantsAgainstEachBase(t *testing.T) {
	grant := func(host string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"secret_name":"corp-key","host":"` + host + `"}`)}
	}
	deployment := govDeployment()
	deployment.EligibleGrants = []types.GrantSpec{grant("a.example"), grant("b.example")}
	srv := &Server{cfg: Config{DefaultPolicy: deployment}}
	base := types.GovernanceProfile{ID: uuid.New(), Name: "base", Ceiling: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{grant("a.example")}}}
	leaf := types.GovernanceProfile{ID: uuid.New(), Name: "leaf", BaseProfileID: &base.ID, Overlay: &types.CeilingOverlay{
		EligibleGrants: &[]types.GrantSpec{grant("a.example"), grant("b.example")}}}
	got, err := srv.composeChain([]types.GovernanceProfile{leaf, base})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ceiling.EligibleGrants) != 1 || !strings.Contains(string(got.Ceiling.EligibleGrants[0].Scope), "a.example") {
		t.Errorf("grants = %v, want only the one the base holds (b.example is within the deployment but not the base)", got.Ceiling.EligibleGrants)
	}
	if len(got.Warnings) != 1 || strings.Contains(strings.Join(got.Warnings, ""), "base") && !strings.Contains(got.Warnings[0], `"leaf"`) {
		t.Errorf("warnings = %v, want one naming the leaf", got.Warnings)
	}
}

// TestResolverStrictAtWriteMeetAtResolve: the same overlay a write refuses resolves, narrowed with a
// warning, once the base has narrowed since.
func TestResolverComposesOnTheDeploymentDefault(t *testing.T) {
	deployment := govDeployment()
	srv := &Server{cfg: Config{DefaultPolicy: deployment}}
	leaf := types.GovernanceProfile{ID: uuid.New(), Name: "leaf", Overlay: &types.CeilingOverlay{
		AllowedDomains: &[]string{deployment.AllowedDomains[0]}}}
	got, err := srv.composeChain([]types.GovernanceProfile{leaf})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Composed || !slices.Equal(got.Ceiling.AllowedDomains, []string{deployment.AllowedDomains[0]}) {
		t.Errorf("composed = %v allowed = %v, want the overlay's one domain", got.Composed, got.Ceiling.AllowedDomains)
	}
	if len(got.Ceiling.DeniedDomains) != len(deployment.DeniedDomains) {
		t.Errorf("denied = %v, want the deployment's inherited", got.Ceiling.DeniedDomains)
	}
}

// the unsatisfiable launch: create and preflight refuse, audited

func TestUnsatisfiableCompositionRefusesCreateAndPreflight(t *testing.T) {
	post := []string{"POST"}
	child := &types.GovernanceProfile{ID: uuid.New(), Name: "team", Overlay: &types.CeilingOverlay{AllowedMethods: &post}}
	cs := assignedStore(child)
	cs.govGraph = []types.GovernanceProfile{*child}
	srv, _, audit := govEscapeFixture(t, cs)
	// The deployment-borne base narrowed under the overlay: its methods and the overlay's are disjoint.
	srv.cfg.DefaultPolicy.AllowedMethods = []string{"GET"}

	for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		w := doSSO(t, srv, http.MethodPost, path, govSession(t, "sub-walled", []string{"eng"}, false), `{"agent":"claude-code","task":"t"}`)
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusForbidden || body.Reason != string(authz.ReasonGovernanceOverlayUnsatisfiable) {
			t.Errorf("POST %s = %d reason %q (%s), want 403 governance_overlay_unsatisfiable", path, w.Code, body.Reason, w.Body.String())
		}
	}
	if !recHasRefusal(audit, authz.ReasonGovernanceOverlayUnsatisfiable, "governance.ceiling") {
		t.Error("no authz.denied governance_overlay_unsatisfiable row for governance.ceiling")
	}
}

// profileChainOfRows backs the doubles with a graph, for a test of the resolver's store reads.
func TestProfileChainDoubleMatchesTheRecursiveStatement(t *testing.T) {
	root := types.GovernanceProfile{ID: uuid.New(), Name: "root"}
	c1 := types.GovernanceProfile{ID: uuid.New(), Name: "c1", BaseProfileID: &root.ID, Overlay: &types.CeilingOverlay{}}
	c2 := types.GovernanceProfile{ID: uuid.New(), Name: "c2", BaseProfileID: &c1.ID, Overlay: &types.CeilingOverlay{}}
	c3 := types.GovernanceProfile{ID: uuid.New(), Name: "c3", BaseProfileID: &c2.ID, Overlay: &types.CeilingOverlay{}}
	chain, err := profileChainOf([]types.GovernanceProfile{root, c1, c2, c3}, nil, c3.ID)
	if err != nil || len(chain) != maxProfileDepth || chain[0].ID != c3.ID {
		t.Fatalf("chain = %d rows err %v, want the leaf and two bases (bounded at 3)", len(chain), err)
	}
	if _, err := profileChainOf(nil, nil, c3.ID); err == nil {
		t.Error("an unknown leaf read as a chain")
	}
	_ = context.Background()
}
