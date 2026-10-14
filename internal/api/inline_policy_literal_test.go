// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// allowAllCeiling is a deployment whose operator allows every public host and
// lists the entries given.
func allowAllCeiling(domains ...string) types.RunPolicySpec {
	return types.RunPolicySpec{MinConfinementClass: types.CC2, AllowAllEgress: true, AllowedDomains: domains}
}

// TestInlinePolicy_AllowAllCeilingKeepsPublicEntries: under an allow-all ceiling
// a member's own allowlist is a narrowing, and its public entries, names and
// public literal addresses alike, are kept as authored with nothing said.
func TestInlinePolicy_AllowAllCeilingKeepsPublicEntries(t *testing.T) {
	h := capPolicyServer(t, nil, nil)
	h.srv.cfg.DefaultPolicy = allowAllCeiling()
	authored := []string{"pypi.org", "*.npmjs.org", "93.184.216.34", "93.184.216.34:8443", "[2606:2800:220:1::1]:443"}
	got, warns := resolveInline(t, h, types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: slices.Clone(authored)})
	if !slices.Equal(got.AllowedDomains, authored) {
		t.Errorf("allowed_domains = %q, want the authored %q", got.AllowedDomains, authored)
	}
	if d := dropWarns(warns); len(d) != 0 {
		t.Errorf("drop warnings = %q, want none", d)
	}
	if got.AllowAllEgress {
		t.Error("the run is allow-all, and its author never asked for that")
	}
}

// TestInlinePolicy_AllowAllCeilingBoundsPrivateLiterals: an exact entry for a
// private-range address is what the proxy's address guard trusts, so under an
// allow-all ceiling a member's own entry survives only where the ceiling lists
// the same address for every port the entry names. trusted and refused are what
// the compiled result then answers the guard (Policy.AllowsLiteralIP).
func TestInlinePolicy_AllowAllCeilingBoundsPrivateLiterals(t *testing.T) {
	type dest struct {
		addr string
		port int
	}
	for _, tc := range []struct {
		name     string
		ceiling  []string
		authored []string
		want     []string
		trusted  []dest
		refused  []dest
	}{
		{name: "a bare entry the ceiling does not list", authored: []string{"pypi.org", "10.0.0.5", "93.184.216.34"},
			want: []string{"pypi.org", "93.184.216.34"}, refused: []dest{{"10.0.0.5", 443}, {"10.0.0.5", 22}}},
		{name: "a port-qualified entry the ceiling does not list", authored: []string{"10.0.0.5:443"}, refused: []dest{{"10.0.0.5", 443}}},
		{name: "the ceiling lists the same bare entry", ceiling: []string{"10.0.0.5"}, authored: []string{"10.0.0.5"},
			want: []string{"10.0.0.5"}, trusted: []dest{{"10.0.0.5", 443}, {"10.0.0.5", 22}}},
		{name: "the ceiling's bare entry covers one port of it", ceiling: []string{"10.0.0.5"}, authored: []string{"10.0.0.5:443"},
			want: []string{"10.0.0.5:443"}, trusted: []dest{{"10.0.0.5", 443}}, refused: []dest{{"10.0.0.5", 22}}},
		{name: "the ceiling lists the same port", ceiling: []string{"10.0.0.5:443"}, authored: []string{"10.0.0.5:443"},
			want: []string{"10.0.0.5:443"}, trusted: []dest{{"10.0.0.5", 443}}, refused: []dest{{"10.0.0.5", 8443}}},
		{name: "another port than the ceiling's", ceiling: []string{"10.0.0.5:443"}, authored: []string{"10.0.0.5:8443"}, refused: []dest{{"10.0.0.5", 8443}, {"10.0.0.5", 443}}},
		{name: "a bare entry over the ceiling's one port", ceiling: []string{"10.0.0.5:443"}, authored: []string{"10.0.0.5"}, refused: []dest{{"10.0.0.5", 443}, {"10.0.0.5", 22}}},
		{name: "an IPv4-mapped spelling", authored: []string{"::ffff:10.0.0.5", "[::ffff:10.0.0.5]:443"}, refused: []dest{{"10.0.0.5", 443}}},
		{name: "an IPv4-mapped spelling of a listed address", ceiling: []string{"10.0.0.5"}, authored: []string{"[::ffff:10.0.0.5]:443"},
			want: []string{"[::ffff:10.0.0.5]:443"}, trusted: []dest{{"10.0.0.5", 443}}, refused: []dest{{"10.0.0.5", 22}}},
		{name: "IPv6 unique-local spellings", authored: []string{"fd12:3456::1", "[FD12:3456:0:0::1]:443"}, refused: []dest{{"fd12:3456::1", 443}}},
		{name: "an IPv6 unique-local address the ceiling lists", ceiling: []string{"fd12:3456::1"}, authored: []string{"[FD12:3456:0:0::1]:22"},
			want: []string{"[FD12:3456:0:0::1]:22"}, trusted: []dest{{"fd12:3456::1", 22}}, refused: []dest{{"fd12:3456::1", 443}}},
		{name: "a shared-address-space entry", authored: []string{"100.64.0.9"}, refused: []dest{{"100.64.0.9", 443}}},
		{name: "a ceiling wildcard is not authority", ceiling: []string{"*.0.0.5", "*.corp.example"}, authored: []string{"10.0.0.5"}, refused: []dest{{"10.0.0.5", 443}}},
		{name: "a ceiling name is not authority", ceiling: []string{"db.corp.example"}, authored: []string{"10.0.0.5", "db.corp.example"},
			want: []string{"db.corp.example"}, refused: []dest{{"10.0.0.5", 443}}},
		{name: "an address no entry can open is left as authored", authored: []string{"127.0.0.1", "169.254.169.254"},
			want: []string{"127.0.0.1", "169.254.169.254"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := capPolicyServer(t, nil, nil)
			h.srv.cfg.DefaultPolicy = allowAllCeiling(tc.ceiling...)
			got, warns := resolveInline(t, h, types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: slices.Clone(tc.authored)})
			if !slices.Equal(got.AllowedDomains, tc.want) {
				t.Errorf("allowed_domains = %q, want %q", got.AllowedDomains, tc.want)
			}
			drops := dropWarns(warns)
			if len(drops) != len(tc.authored)-len(tc.want) {
				t.Errorf("drop warnings = %q, want one per dropped entry", drops)
			}
			for _, d := range tc.authored {
				if named := slices.Contains(drops, privateLiteralDropWarning(d)); named == slices.Contains(tc.want, d) {
					t.Errorf("drop warnings = %q: %q kept = %v", drops, d, !named)
				}
			}
			pol := proxy.CompilePolicy(got)
			for _, d := range tc.trusted {
				if !pol.AllowsLiteralIP(d.addr, d.port) {
					t.Errorf("the result does not trust %s on %d, which the ceiling lists", d.addr, d.port)
				}
			}
			for _, d := range tc.refused {
				if pol.AllowsLiteralIP(d.addr, d.port) {
					t.Errorf("the result trusts %s on %d, which the ceiling does not list", d.addr, d.port)
				}
			}
		})
	}
}

// An egress_host grant is not authority for the address either: the entry goes
// with the capability unenforced, and with it enforced and granted.
func TestInlinePolicy_EgressHostGrantDoesNotCoverAPrivateLiteral(t *testing.T) {
	for name, enf := range map[string]map[string]bool{"unenforced": nil, "enforced and granted": {capEgressHost: true}} {
		t.Run(name, func(t *testing.T) {
			h := capPolicyServer(t, []types.CapabilityGrant{
				grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "10.0.0.5", types.CapabilityAllow),
				grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", types.CapabilityAllow),
			}, enf)
			h.srv.cfg.DefaultPolicy = allowAllCeiling()
			got, warns := resolveInline(t, h, types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"10.0.0.5", "pypi.org"}})
			if !slices.Equal(got.AllowedDomains, []string{"pypi.org"}) || len(dropWarns(warns)) != 1 {
				t.Fatalf("allowed_domains = %q, warnings = %q; want only pypi.org and one drop", got.AllowedDomains, warns)
			}
		})
	}
}

// literalStoredFixture is one member, an allow-all ceiling listing ceilingDomains
// (a governance profile when assigned, else the deployment default) and one
// stored policy row holding stored.
func literalStoredFixture(t *testing.T, assigned bool, ceilingDomains, stored []string) (*Server, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	ceiling := allowAllCeiling(ceilingDomains...)
	st := &memberBoundStore{policy: types.RunPolicy{ID: uuid.New(), Name: "row",
		Spec: types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: stored}}}
	if assigned {
		st.profile = &types.GovernanceProfile{ID: uuid.New(), Name: "walled", Ceiling: ceiling}
	}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = ceiling
	return New(cfg), st.policy.ID
}

// TestStoredPolicy_PrivateLiteralFollowsTheMemberClamp: a stored row is bounded
// as a whole only for a member with an assigned profile, and there its private
// address entry is held to the ceiling like an inline one. For a member with
// none the row stays as its admin wrote it.
func TestStoredPolicy_PrivateLiteralFollowsTheMemberClamp(t *testing.T) {
	stored := []string{"10.0.0.5", "pypi.org"}
	for _, tc := range []struct {
		name     string
		assigned bool
		ceiling  []string
		want     []string
	}{
		{"assigned, the ceiling does not list it", true, nil, []string{"pypi.org"}},
		{"assigned, the ceiling lists it", true, []string{"10.0.0.5"}, stored},
		{"no assigned profile", false, nil, stored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, policyID := literalStoredFixture(t, tc.assigned, tc.ceiling, stored)
			r, ctx := boundMemberRequest(t)
			got, _, warns, _, ok := srv.resolveRunPolicy(ctx, httptest.NewRecorder(), r, &createRunRequest{Agent: "claude-code", PolicyID: &policyID}, false)
			if !ok {
				t.Fatal("the stored policy was refused")
			}
			if !slices.Equal(got.AllowedDomains, tc.want) {
				t.Errorf("allowed_domains = %q, want %q", got.AllowedDomains, tc.want)
			}
			if wantDrop := len(tc.want) < len(stored); wantDrop != (len(dropWarns(warns)) == 1) {
				t.Errorf("drop warnings = %q, want one exactly when the entry went", dropWarns(warns))
			}
		})
	}
}

// TestOverrideAdds_PrivateLiteralAdditionIsBounded: where the policy a person
// starts from is not clamped as a whole, only what they added is bounded. An
// added private address entry the ceiling does not list goes, and the policy's
// own entry for another address stays.
func TestOverrideAdds_PrivateLiteralAdditionIsBounded(t *testing.T) {
	srv, _ := literalStoredFixture(t, false, nil, nil)
	r, ctx := boundMemberRequest(t)
	ceiling, err := srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"10.0.0.5", "pypi.org", "10.0.0.9", "files.example.com"}}
	edits := &overrideEdits{addedHosts: []string{"10.0.0.9", "files.example.com"}}
	got, warns, refusal := srv.boundOverrideAdds(ctx, r, spec, edits, ceiling, true)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal)
	}
	if want := []string{"10.0.0.5", "pypi.org", "files.example.com"}; !slices.Equal(got.AllowedDomains, want) {
		t.Errorf("allowed_domains = %q, want %q", got.AllowedDomains, want)
	}
	if d := dropWarns(warns); !slices.Equal(d, []string{privateLiteralDropWarning("10.0.0.9")}) {
		t.Errorf("drop warnings = %q, want one naming the added address only", d)
	}
}
