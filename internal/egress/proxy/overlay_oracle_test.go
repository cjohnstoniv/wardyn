// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// These are the enforcement oracles for the governance-profile meet
// (composer.ApplyOverlay). They are independent of the meet's own comparator:
// base, overlay and result are each compiled by THIS package's real policy
// compiler, and the result is probed through the proxy's full decision
// pipeline (the literal-IP guard, the host verdict, the method check, and the
// private-address vet with an internal_hosts lift), the approval client's hold
// limits, and the broker's push bounds. The rule under test is the one the
// design states: the result must refuse whatever either side refuses.

var (
	oracleHosts   = []string{"git.corp.example", "corp.example", "x.y.corp.example", "api.vendor.example", "files.pypi.org", "evil.example", "db.corp.internal", "10.0.0.5", "10.0.0.6", "127.0.0.1", "169.254.169.254"}
	oraclePorts   = []int{22, 80, 443}
	oracleMethods = []string{"CONNECT", "GET", "POST", "DELETE"}
)

// oracleResolver answers every name with a public address, except the
// internal host, which resolves into a private range its declaration lifts.
func oracleResolver() fakeResolver {
	m := map[string][]net.IP{"db.corp.internal": ips("10.40.1.5")}
	for _, h := range oracleHosts {
		if net.ParseIP(h) == nil && m[h] == nil {
			m[h] = ips("93.184.216.34")
		}
	}
	return fakeResolver{m: m}
}

func oracleProxy(t *testing.T, spec types.RunPolicySpec) *Proxy {
	t.Helper()
	p, _ := newInternalHostsProxy(t, spec, oracleResolver(),
		[]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}}}, nil, nil, "127.0.0.1:1")
	return p
}

// oracleReach is every (host, port, method) the whole pipeline lets through.
func oracleReach(t *testing.T, spec types.RunPolicySpec) map[string]bool {
	t.Helper()
	p := oracleProxy(t, spec)
	out := map[string]bool{}
	for _, h := range oracleHosts {
		for _, port := range oraclePorts {
			for _, m := range oracleMethods {
				if d, _, _ := p.evaluate(context.Background(), h, port, m, ""); d == egress.Allow {
					out[fmt.Sprintf("%s|%s|%d", h, m, port)] = true
				}
			}
		}
	}
	return out
}

func assertNoWiderReach(t *testing.T, label string, base, got types.RunPolicySpec) {
	t.Helper()
	br, gr := oracleReach(t, base), oracleReach(t, got)
	for k := range gr {
		if !br[k] {
			t.Errorf("%s: the result reaches %s and the base does not", label, k)
		}
	}
	// The two answers the design names: the trust an exact entry grants a literal
	// IP, and the private-address vet.
	bp, gp := CompilePolicy(base), CompilePolicy(got)
	b, g := oracleProxy(t, base), oracleProxy(t, got)
	for _, h := range oracleHosts {
		for _, port := range oraclePorts {
			if net.ParseIP(h) != nil && gp.AllowsLiteralIP(h, port) && !bp.AllowsLiteralIP(h, port) {
				t.Errorf("%s: AllowsLiteralIP(%s, %d) is true on the result and false on the base", label, h, port)
			}
		}
		if !g.vetHost(h).Denied && b.vetHost(h).Denied {
			t.Errorf("%s: vetHost(%s) admits on the result and denies on the base", label, h)
		}
	}
}

func authority(c types.RunPolicySpec) composer.Authority { return composer.Authority{Ceiling: c} }

func pi(v int) *int { return &v }

// An allow-all base never covers an entry: an overlay naming a private literal
// IP or an internal host under one is refused at write, and the lenient meet
// hands it nothing the base would not.
func TestOverlayOracleAllowAllNeverLendsAnEntry(t *testing.T) {
	base := authority(types.RunPolicySpec{AllowAllEgress: true})
	ov := composer.Overlay{Ceiling: types.CeilingOverlay{AllowedDomains: &[]string{"10.0.0.5", "db.corp.internal"}}}
	var oe *composer.OverlayError
	if err := composer.ValidateOverlay(base, ov); !errors.As(err, &oe) || oe.Reason != composer.ReasonOverlayInvalid {
		t.Fatalf("ValidateOverlay = %v, want governance_overlay_invalid", err)
	}
	got, _, err := composer.ApplyOverlay(base, ov)
	if err != nil {
		t.Fatal(err)
	}
	assertNoWiderReach(t, "allow-all base", base.Ceiling, got.Ceiling)
	if oracleProxy(t, got.Ceiling).policy.AllowsLiteralIP("10.0.0.5", 22) {
		t.Error("the result trusts the private literal 10.0.0.5, which the allow-all base never did")
	}
	// The base itself must not trust it either, or this test proves nothing.
	if d, _, _ := oracleProxy(t, base.Ceiling).evaluate(context.Background(), "10.0.0.5", 22, "GET", ""); d == egress.Allow {
		t.Fatal("the allow-all base reached a private literal IP; the oracle's premise is wrong")
	}
}

// An allow-all flip: an overlay cannot turn allow-all on over a base without it.
func TestOverlayOracleAllowAllFlip(t *testing.T) {
	base := authority(types.RunPolicySpec{AllowedDomains: []string{"api.vendor.example"}})
	ov := composer.Overlay{Ceiling: types.CeilingOverlay{AllowAllEgress: pb(true)}}
	got, _, err := composer.ApplyOverlay(base, ov)
	if err != nil {
		t.Fatal(err)
	}
	assertNoWiderReach(t, "allow-all flip", base.Ceiling, got.Ceiling)
	if oracleReach(t, got.Ceiling)["evil.example|GET|80"] {
		t.Error("an overlay turned allow-all on")
	}
}

func pb(v bool) *bool { return &v }

// Disjoint allowed_methods has no representable result: [] would read as "every
// method" to the proxy, which is the widest answer there is.
func TestOverlayOracleDisjointMethodsAreNeverEmpty(t *testing.T) {
	base := authority(types.RunPolicySpec{AllowedDomains: []string{"api.vendor.example"}, AllowedMethods: []string{"GET"}})
	ov := composer.Overlay{Ceiling: types.CeilingOverlay{AllowedMethods: &[]string{"POST"}}}
	if _, _, err := composer.ApplyOverlay(base, ov); err == nil {
		t.Fatal("a disjoint method list composed to a result")
	}
	// What the meet refuses to write is exactly what the proxy would have read as everything.
	if !CompilePolicy(types.RunPolicySpec{AllowedMethods: []string{}}).methodAllowed("DELETE") {
		t.Fatal("the premise changed: an empty allowed_methods no longer allows every method")
	}
}

// meet(0, 64) over raw pack values is 64; the broker reads the base's 0 as 32.
func TestOverlayOraclePackDefaults(t *testing.T) {
	base := authority(types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/**"}}})
	got, _, err := composer.ApplyOverlay(base, composer.Overlay{Ceiling: types.CeilingOverlay{PushRules: &types.PushRulesOverlay{MaxInspectPackMiB: pi(64)}}})
	if err != nil {
		t.Fatal(err)
	}
	b, g := compilePushRules(base.Ceiling.PushRules), compilePushRules(got.Ceiling.PushRules)
	if b.inspectMax != 32<<20 {
		t.Fatalf("the broker's default pack cap is %d bytes, not 32 MiB: the meet's constant is stale", b.inspectMax)
	}
	if g.inspectMax > b.inspectMax {
		t.Errorf("the result inspects up to %d bytes, the base %d", g.inspectMax, b.inspectMax)
	}
	// With no active rule on the base, an overlay's cap is the only bound, and it is taken as written.
	got, _, err = composer.ApplyOverlay(authority(types.RunPolicySpec{}), composer.Overlay{Ceiling: types.CeilingOverlay{PushRules: &types.PushRulesOverlay{MaxInspectPackMiB: pi(64)}}})
	if err != nil {
		t.Fatal(err)
	}
	if rs := compilePushRules(got.Ceiling.PushRules); rs == nil || rs.inspectMax != 64<<20 {
		t.Errorf("a base with no rule must not lend its default: %+v", rs)
	}
}

// Every runtime default the meet normalises against, pinned to what the sidecar does.
func TestOverlayOracleNormalisersMatchTheRuntime(t *testing.T) {
	values := []int{0, 1, 5, 29, 30, 31, 119, 120, 121, 599, 600, 601, 100000}
	holdOf := func(sec int) time.Duration {
		ap := newApprovalClient("http://127.0.0.1:1", nil, uuid.New(), nil)
		ap.configureHold(types.FirstUseWaitForReview, time.Duration(sec)*time.Second, 0)
		return ap.holdTimeout
	}
	holdsOf := func(n int) int {
		ap := newApprovalClient("http://127.0.0.1:1", nil, uuid.New(), nil)
		ap.configureHold(types.FirstUseWaitForReview, 0, n)
		return cap(ap.holdSem)
	}
	pushHoldOf := func(sec int) time.Duration {
		rs := compilePushRules(&types.PushRulesSpec{DenyPaths: []string{"a"}, HoldSeconds: sec})
		return rs.hold
	}
	packOf := func(mib int) int64 {
		return compilePushRules(&types.PushRulesSpec{DenyPaths: []string{"a"}, MaxInspectPackMiB: mib}).inspectMax
	}
	for _, b := range values {
		for _, o := range values {
			ov := func(c types.CeilingOverlay) types.RunPolicySpec {
				got, _, err := composer.ApplyOverlay(authority(types.RunPolicySpec{FirstUseHoldSeconds: b, MaxHolds: b,
					PushRules: &types.PushRulesSpec{DenyPaths: []string{"a"}, HoldSeconds: b, MaxInspectPackMiB: b}}),
					composer.Overlay{Ceiling: c})
				if err != nil {
					t.Fatal(err)
				}
				return got.Ceiling
			}
			got := ov(types.CeilingOverlay{FirstUseHoldSeconds: pi(o), MaxHolds: pi(o),
				PushRules: &types.PushRulesOverlay{HoldSeconds: pi(o), MaxInspectPackMiB: pi(o)}})
			base := types.RunPolicySpec{FirstUseHoldSeconds: b, MaxHolds: b, PushRules: &types.PushRulesSpec{DenyPaths: []string{"a"}, HoldSeconds: b, MaxInspectPackMiB: b}}
			alone := types.RunPolicySpec{FirstUseHoldSeconds: o, MaxHolds: o, PushRules: &types.PushRulesSpec{DenyPaths: []string{"a"}, HoldSeconds: o, MaxInspectPackMiB: o}}
			for name, fn := range map[string]func(types.RunPolicySpec) int64{
				"first_use_hold": func(s types.RunPolicySpec) int64 { return int64(holdOf(s.FirstUseHoldSeconds)) },
				"max_holds":      func(s types.RunPolicySpec) int64 { return int64(holdsOf(s.MaxHolds)) },
				"push_hold":      func(s types.RunPolicySpec) int64 { return int64(pushHoldOf(s.PushRules.HoldSeconds)) },
				"pack":           func(s types.RunPolicySpec) int64 { return packOf(s.PushRules.MaxInspectPackMiB) },
			} {
				if fn(got) > fn(base) || fn(got) > fn(alone) {
					t.Errorf("%s: base %d overlay %d: the result enforces %d, base %d, overlay alone %d", name, b, o, fn(got), fn(base), fn(alone))
				}
			}
		}
	}
}

func TestOverlayOracleFirstUseModeAndTools(t *testing.T) {
	base := authority(types.RunPolicySpec{FirstUseApproval: types.FirstUseDenyWithReview,
		ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}, {Tool: "Bash", Effect: types.ToolDeny}}})
	got, _, err := composer.ApplyOverlay(base, composer.Overlay{Ceiling: types.CeilingOverlay{
		FirstUseApproval: &[]types.FirstUseMode{types.FirstUseWaitForReview}[0],
		ToolRules:        &[]types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}, {Tool: "Read", Effect: types.ToolHold}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if m := CompilePolicy(got.Ceiling).FirstUseMode(); m != types.FirstUseDenyWithReview {
		t.Errorf("first-use mode = %q, want the base's stricter deny_with_review", m)
	}
	pol := CompilePolicy(got.Ceiling)
	for tool, want := range map[string]types.ToolEffect{"Bash": types.ToolDeny, "Read": types.ToolHold, "Edit": types.ToolHold} {
		if e, _ := pol.ToolEffectFor(tool); e != want {
			t.Errorf("%s = %q, want %q", tool, e, want)
		}
	}
}

// Clamp is run against the base and the result, over the same proposals. What
// a member can end up with under the composed ceiling stays inside both the
// composed ceiling and the base.
func TestOverlayOracleClampUnderTheResult(t *testing.T) {
	base := authority(types.RunPolicySpec{
		AllowedDomains: []string{"*.corp.example", "api.vendor.example"}, AllowedMethods: []string{"GET", "POST"},
		FirstUseApproval: types.FirstUseDenyWithReview, MinConfinementClass: types.CC1,
		Resources: &types.ResourceLimits{CPUMillis: 4000, MemoryMiB: 8192},
	})
	got, _, err := composer.ApplyOverlay(base, composer.Overlay{Ceiling: types.CeilingOverlay{
		AllowedDomains: &[]string{"git.corp.example"}, AllowedMethods: &[]string{"GET"},
		FirstUseApproval: &[]types.FirstUseMode{types.FirstUseAlwaysDeny}[0], MinConfinementClass: &[]types.ConfinementClass{types.CC3}[0],
		Resources: &types.ResourcesOverlay{CPUMillis: pi(1000)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	proposals := []types.RunPolicySpec{
		{AllowAllEgress: true, AllowedDomains: []string{"git.corp.example", "evil.example", "api.vendor.example"}, FirstUseApproval: types.FirstUseWaitForReview},
		{AllowedDomains: []string{"x.y.corp.example"}, AllowedMethods: []string{"POST", "DELETE"}, Resources: &types.ResourceLimits{CPUMillis: 64000}},
		{},
	}
	for i, p := range proposals {
		underResult, _ := composer.Clamp(p, got.Ceiling, types.GovernanceLimits{})
		underBase, _ := composer.Clamp(p, base.Ceiling, types.GovernanceLimits{})
		assertNoWiderReach(t, "clamp under the result (vs the result)", got.Ceiling, underResult)
		assertNoWiderReach(t, "clamp under the result (vs the base)", base.Ceiling, underResult)
		if underResult.MinConfinementClass.Rank() < underBase.MinConfinementClass.Rank() {
			t.Errorf("proposal %d: confinement under the result %q is below the base's %q", i, underResult.MinConfinementClass, underBase.MinConfinementClass)
		}
		if rank(underResult.FirstUseApproval) < rank(underBase.FirstUseApproval) {
			t.Errorf("proposal %d: first-use under the result is looser than under the base", i)
		}
		if underResult.Resources.CPUMillis > underBase.Resources.CPUMillis {
			t.Errorf("proposal %d: cpu under the result %d exceeds the base's %d", i, underResult.Resources.CPUMillis, underBase.Resources.CPUMillis)
		}
	}
}

func rank(m types.FirstUseMode) int {
	return slices.Index([]types.FirstUseMode{types.FirstUseWaitForReview, types.FirstUseDenyWithReview, types.FirstUseAlwaysDeny}, m.Normalize())
}

// A removed rubric cap: an overlay that raises or omits a level never lifts the
// base's cap, judged by folding both rubrics over every posture.
func TestOverlayOracleRubricCapIsNeverRemoved(t *testing.T) {
	baseRubric := &types.AutonomyRubric{EgressOpen: types.AutonomyL0, SecretsPowerful: types.AutonomyL1, ConfinementCC2: types.AutonomyL2}
	base := composer.Authority{Limits: types.GovernanceLimits{AutonomyRubric: baseRubric}}
	got, _, err := composer.ApplyOverlay(base, composer.Overlay{Limits: types.LimitsOverlay{AutonomyRubric: &types.AutonomyRubric{
		EgressOpen: types.AutonomyL3, SecretsPowerful: types.AutonomyL3, SecretsNone: types.AutonomyL1}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, eg := range []types.AutonomyEgressPosture{types.AutonomyEgressOpen, types.AutonomyEgressReviewed, types.AutonomyEgressSealed} {
		for _, sec := range []types.AutonomySecretsPosture{types.AutonomySecretsPowerful, types.AutonomySecretsBaseline, types.AutonomySecretsNone} {
			for _, cc := range []types.ConfinementClass{types.CC1, types.CC2, types.CC3} {
				posture := types.AutonomyPosture{Egress: eg, Secrets: sec, Confinement: cc}
				lb, _ := composer.FoldAutonomy(*baseRubric, posture)
				lg, _ := composer.FoldAutonomy(*got.Limits.AutonomyRubric, posture)
				if lb != "" && (lg == "" || lg.Rank() > lb.Rank()) {
					t.Errorf("posture %+v: base caps at %q, the result at %q", posture, lb, lg)
				}
			}
		}
	}
}

// Random compositions, each judged through the whole pipeline. Seeded so a
// failure names its case.
func TestOverlayOracleRandomCompositions(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	domains := []string{"*.corp.example", "corp.example", "git.corp.example", "api.corp.example:443", "api.vendor.example", "*.pypi.org", "10.0.0.5", "*.example", "db.corp.internal"}
	pickSome := func(xs []string) []string {
		out := []string{}
		for _, x := range xs {
			if r.Intn(3) == 0 {
				out = append(out, x)
			}
		}
		return out
	}
	for i := 0; i < 150; i++ {
		base := authority(types.RunPolicySpec{
			AllowedDomains: pickSome(domains), DeniedDomains: pickSome(domains[:4]), AllowAllEgress: r.Intn(3) == 0,
			AllowedMethods: pickSome([]string{"GET", "POST", "DELETE"}),
		})
		var c types.CeilingOverlay
		if r.Intn(2) == 0 {
			d := pickSome(domains)
			c.AllowedDomains = &d
		}
		if r.Intn(3) == 0 {
			c.AllowAllEgress = pb(r.Intn(2) == 0)
		}
		if m := pickSome([]string{"GET", "POST", "DELETE", "PUT"}); len(m) > 0 && r.Intn(2) == 0 {
			c.AllowedMethods = &m
		}
		if r.Intn(3) == 0 {
			d := pickSome(domains)
			c.DeniedDomains = &d
		}
		got, _, err := composer.ApplyOverlay(base, composer.Overlay{Ceiling: c})
		if err != nil {
			continue
		}
		assertNoWiderReach(t, "random composition "+string(rune('A'+i%26)), base.Ceiling, got.Ceiling)
		if t.Failed() {
			t.Fatalf("case %d: base %+v overlay %+v", i, base.Ceiling, c)
		}
	}
}
