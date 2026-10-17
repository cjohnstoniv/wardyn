// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The property tests below judge a composition against ENFORCEMENT: the
// proxy's compiled policy answers "is this host reachable", never the meet's
// own comparator, and every scalar is re-derived here from the runtime's
// defaults rather than read back through meet.go's normalisers. A test that
// used the meet to grade the meet would agree with it whatever it did.

var (
	genDomains = []string{
		"*.corp.example", "corp.example", "git.corp.example", "api.corp.example:443", "*.corp.example:443",
		"api.vendor.example", "pypi.org", "*.pypi.org", "10.0.0.5", "10.0.0.5:22", "*.example", "x.y.corp.example", "*.y.corp.example",
	}
	genHosts   = append(slices.Clone(genDomains[:0:0]), "git.corp.example", "corp.example", "x.y.corp.example", "api.vendor.example", "files.pypi.org", "pypi.org", "evil.example", "10.0.0.5", "10.0.0.6", "127.0.0.1", "db.corp.internal")
	genPorts   = []int{22, 80, 443, 8443}
	genMethods = []string{"GET", "POST", "PUT", "DELETE", "HEAD"}
	genTools   = []string{"*", "Bash", "Read", "WebFetch", "Edit"}
	genPaths   = []string{".github/**", "infra/", "docs/*.md", "src/**/*.go"}
	genCaps    = []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapWorkRead, adoscope.CapBuildRead, adoscope.CapWikiRead}
	genLevels  = []types.AutonomyLevel{"", types.AutonomyL0, types.AutonomyL1, types.AutonomyL2, types.AutonomyL3}
)

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.Intn(len(xs))] }

func subset[T any](r *rand.Rand, xs []T) []T {
	out := []T{}
	for _, x := range xs {
		if r.Intn(3) == 0 {
			out = append(out, x)
		}
	}
	return out
}

func some[T any](r *rand.Rand, v T) *T {
	if r.Intn(5) < 2 {
		return &v
	}
	return nil
}

func smallInt(r *rand.Rand, xs ...int) int { return xs[r.Intn(len(xs))] }

// genAuthority is a random base: every field populated or left unset at random.
func genAuthority(r *rand.Rand) composer.Authority {
	c := types.RunPolicySpec{
		AllowedDomains: subset(r, genDomains), DeniedDomains: subset(r, genDomains[:6]),
		AllowAllEgress:      r.Intn(4) == 0,
		FirstUseApproval:    pick(r, []types.FirstUseMode{"", types.FirstUseAlwaysDeny, types.FirstUseDenyWithReview, types.FirstUseWaitForReview}),
		FirstUseHoldSeconds: smallInt(r, 0, 5, 30, 120, 900), MaxHolds: smallInt(r, 0, 4, 16, 64, 1000),
		AllowedMethods:      subset(r, genMethods),
		MinConfinementClass: pick(r, []types.ConfinementClass{"", types.CC1, types.CC2, types.CC3}),
		AutoStopAfterSec:    smallInt(r, -1, 0, 300, 3600), GitPushAnyBranch: r.Intn(3) == 0,
		UIApps: []types.UIApp{{Name: "jupyter", Port: 8888}, {Name: "docs", Port: 8000}}[:r.Intn(3)],
	}
	if r.Intn(2) == 0 {
		c.Resources = &types.ResourceLimits{CPUMillis: smallInt(r, 0, 500, 4000, 64000), MemoryMiB: smallInt(r, 0, 256, 8192), PidsLimit: smallInt(r, 0, 64, 4096), DiskMiB: smallInt(r, 0, 1024)}
	}
	for _, tool := range genTools {
		if r.Intn(3) == 0 {
			c.ToolRules = append(c.ToolRules, types.ToolRule{Tool: tool, Effect: pick(r, []types.ToolEffect{types.ToolAllow, types.ToolHold, types.ToolDeny})})
		}
	}
	if r.Intn(2) == 0 {
		c.PushRules = &types.PushRulesSpec{DenyPaths: subset(r, genPaths), RequireReviewPaths: subset(r, genPaths),
			MaxInspectPackMiB: smallInt(r, 0, 8, 32, 64), HoldSeconds: smallInt(r, 0, 30, 300, 900), MaxFileSizeMiB: smallInt(r, 0, 5, 50)}
	}
	if r.Intn(3) == 0 {
		c.AzureDevOpsCapabilities = subset(r, genCaps)
	}
	if r.Intn(4) == 0 {
		rw := r.Intn(2) == 0
		c.WorkspaceMounts = []types.WorkspaceMount{{Source: "/a", Target: "/w/a", ReadOnly: &rw}, {Source: "/b", Target: "/w/b"}}[:r.Intn(3)]
	}
	l := types.GovernanceLimits{
		DenyInteractive: r.Intn(3) == 0, DenyUIApps: r.Intn(3) == 0, MaxConcurrentRuns: smallInt(r, 0, 2, 10),
		MaxCPUMillis: smallInt(r, 0, 1000, 8000), MaxMemoryMiB: smallInt(r, 0, 512),
		RunLimits: types.RunLimits{MaxEndAheadSec: smallInt(r, 0, 600, 7200), MaxWaitSec: smallInt(r, 0, 300), AllowNoEnd: r.Intn(2) == 0,
			UserChangesLimits: r.Intn(2) == 0, PauseIdleAfterSec: smallInt(r, 0, 120)},
	}
	l.DefaultEndSec = min(smallInt(r, 0, 300, 3600), cmp.Or(l.MaxEndAheadSec, 1<<30))
	l.DefaultWaitSec = min(smallInt(r, 0, 100), cmp.Or(l.MaxWaitSec, 1<<30))
	if r.Intn(2) == 0 {
		l.AutonomyRubric = &types.AutonomyRubric{EgressOpen: pick(r, genLevels), SecretsNone: pick(r, genLevels), ConfinementCC2: pick(r, genLevels)}
		if *l.AutonomyRubric == (types.AutonomyRubric{}) {
			l.AutonomyRubric = nil
		}
	}
	return composer.Authority{Ceiling: c, Limits: l}
}

// genOverlay is a random overlay: each field present with probability 2/5.
func genOverlay(r *rand.Rand, withGrants bool) composer.Overlay {
	c := types.CeilingOverlay{
		AllowedDomains: some(r, subset(r, genDomains)), DeniedDomains: some(r, subset(r, genDomains[:6])),
		AllowAllEgress:      some(r, r.Intn(2) == 0),
		FirstUseApproval:    some(r, pick(r, []types.FirstUseMode{types.FirstUseAlwaysDeny, types.FirstUseDenyWithReview, types.FirstUseWaitForReview})),
		FirstUseHoldSeconds: some(r, smallInt(r, 0, 5, 30, 120, 900)), MaxHolds: some(r, smallInt(r, 0, 4, 16, 64, 1000)),
		MinConfinementClass: some(r, pick(r, []types.ConfinementClass{types.CC1, types.CC2, types.CC3})),
		AutoStopAfterSec:    some(r, smallInt(r, -1, 0, 300, 3600)), GitPushAnyBranch: some(r, r.Intn(2) == 0),
	}
	if ms := subset(r, genMethods); len(ms) > 0 && r.Intn(2) == 0 {
		c.AllowedMethods = &ms
	}
	if cs := subset(r, genCaps); len(cs) > 0 && r.Intn(3) == 0 {
		c.AzureDevOpsCapabilities = &cs
	}
	if r.Intn(3) == 0 {
		c.UIApps = &[]types.UIApp{{Name: "jupyter", Port: 8888}, {Name: "docs", Port: 8000}}
		*c.UIApps = (*c.UIApps)[:r.Intn(3)]
	}
	if r.Intn(3) == 0 {
		c.Resources = &types.ResourcesOverlay{CPUMillis: some(r, smallInt(r, 0, 500, 4000, 64000)), MemoryMiB: some(r, smallInt(r, 0, 256, 8192)),
			PidsLimit: some(r, smallInt(r, 0, 64)), DiskMiB: some(r, smallInt(r, 0, 512))}
	}
	if r.Intn(3) == 0 {
		var rules []types.ToolRule
		for _, tool := range genTools {
			if r.Intn(3) == 0 {
				rules = append(rules, types.ToolRule{Tool: tool, Effect: pick(r, []types.ToolEffect{types.ToolAllow, types.ToolHold, types.ToolDeny})})
			}
		}
		c.ToolRules = &rules
	}
	if r.Intn(2) == 0 {
		c.PushRules = &types.PushRulesOverlay{DenyPaths: some(r, subset(r, genPaths)), RequireReviewPaths: some(r, subset(r, genPaths)),
			MaxInspectPackMiB: some(r, smallInt(r, 0, 8, 32, 64)), HoldSeconds: some(r, smallInt(r, 0, 30, 300, 900)), MaxFileSizeMiB: some(r, smallInt(r, 0, 5, 50))}
	}
	if withGrants && r.Intn(3) == 0 {
		c.EligibleGrants = &[]types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.vendor.example"}`)}}
	}
	l := types.LimitsOverlay{
		DenyInteractive: some(r, r.Intn(2) == 0), MaxConcurrentRuns: some(r, smallInt(r, 0, 2, 10)), MaxCPUMillis: some(r, smallInt(r, 0, 1000, 8000)),
		MaxEndAheadSec: some(r, smallInt(r, 0, 600, 7200)), DefaultEndSec: some(r, smallInt(r, 0, 300, 3600)), AllowNoEnd: some(r, r.Intn(2) == 0),
		MaxWaitSec: some(r, smallInt(r, 0, 300)), DefaultWaitSec: some(r, smallInt(r, 0, 100)), PauseIdleAfterSec: some(r, smallInt(r, 0, 120)),
		UserChangesLimits: some(r, r.Intn(2) == 0),
	}
	if r.Intn(3) == 0 {
		l.AutonomyRubric = &types.AutonomyRubric{EgressOpen: pick(r, genLevels), SecretsNone: pick(r, genLevels), ConfinementCC1: pick(r, genLevels)}
	}
	return composer.Overlay{Ceiling: c, Limits: l}
}

type composition struct {
	base composer.Authority
	o1   composer.Overlay
	o2   composer.Overlay
}

func (composition) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(composition{base: genAuthority(r), o1: genOverlay(r, true), o2: genOverlay(r, false)})
}

var quickCfg = &quick.Config{MaxCount: 4000}

// reach is every (host, port, method) the compiled policy lets through, plus
// the literal-IP trust the proxy grants an exact entry. Enforcement's answer.
func reach(spec types.RunPolicySpec) map[string]bool {
	ev := proxy.NewBuiltinEvaluator(spec)
	pol := proxy.CompilePolicy(spec)
	out := map[string]bool{}
	for _, h := range genHosts {
		for _, p := range genPorts {
			v, _ := ev.EvaluateHost(context.Background(), egress.Request{Host: h, Port: p})
			for _, m := range append([]string{"CONNECT"}, genMethods...) {
				if v == egress.VerdictAllow && ev.MethodAllowed(m) {
					out[fmt.Sprintf("%s|%s|%d", h, m, p)] = true
				}
			}
			if net.ParseIP(h) != nil && pol.AllowsLiteralIP(h, p) {
				out[fmt.Sprintf("literal|%s|%d", h, p)] = true
			}
		}
	}
	return out
}

// effTool is the effect a compiled policy gives a tool, with the toolgate's own
// hold fallback for a policy that carries no rule at all.
func effTool(spec types.RunPolicySpec, tool string) int {
	eff, ok := proxy.CompilePolicy(spec).ToolEffectFor(tool)
	if !ok {
		eff = types.ToolHold
	}
	return map[types.ToolEffect]int{types.ToolAllow: 0, types.ToolHold: 1, types.ToolDeny: 2}[eff]
}

// The runtime's own readings of the defaulted fields, written out here from the
// proxy's constants rather than imported from the meet.
func effHold(x int) int {
	if x <= 0 {
		return 30
	}
	return min(x, 600)
}

func effHolds(x int) int {
	if x <= 0 {
		return 16
	}
	return min(x, 256)
}

func effPackMiB(p *types.PushRulesSpec) int { // 0 = no inspection at all
	switch {
	case !p.IsSet():
		return 1 << 30
	case p.MaxInspectPackMiB > 0:
		return p.MaxInspectPackMiB
	}
	return 32
}

func effPushHold(p *types.PushRulesSpec) int {
	if p == nil || p.HoldSeconds <= 0 {
		return 120
	}
	return min(p.HoldSeconds, 600)
}

func unbounded(x int) int {
	if x <= 0 {
		return 1 << 30
	}
	return x
}

func fuaRank(m types.FirstUseMode) int {
	return map[types.FirstUseMode]int{types.FirstUseWaitForReview: 1, types.FirstUseDenyWithReview: 2, types.FirstUseAlwaysDeny: 3}[m.Normalize()]
}

func res(c types.RunPolicySpec) types.ResourceLimits {
	r := types.ResourceLimits{}
	if c.Resources != nil {
		r = *c.Resources
	}
	return types.ResourceLimits{
		CPUMillis: cmp.Or(r.CPUMillis, 2000), MemoryMiB: cmp.Or(r.MemoryMiB, 4096), PidsLimit: cmp.Or(r.PidsLimit, 512), DiskMiB: unbounded(r.DiskMiB),
	}
}

// narrower reports why got is not at most as permissive as base, or "".
func narrower(base, got composer.Authority) string {
	b, g := base.Ceiling, got.Ceiling
	br, gr := reach(b), reach(g)
	for k := range gr {
		if !br[k] {
			return "reach widened: " + k
		}
	}
	switch {
	case fuaRank(g.FirstUseApproval) < fuaRank(b.FirstUseApproval):
		return "first_use_approval loosened"
	case effHold(g.FirstUseHoldSeconds) > effHold(b.FirstUseHoldSeconds):
		return "first_use_hold_seconds longer"
	case effHolds(g.MaxHolds) > effHolds(b.MaxHolds):
		return "max_holds larger"
	case g.MinConfinementClass.Rank() < b.MinConfinementClass.Rank():
		return "min_confinement_class lowered"
	case unbounded(g.AutoStopAfterSec) > unbounded(b.AutoStopAfterSec):
		return "auto_stop_after_sec looser"
	case g.GitPushAnyBranch && !b.GitPushAnyBranch:
		return "git_push_any_branch enabled"
	case g.AllowAllEgress && !b.AllowAllEgress:
		return "allow_all_egress enabled"
	}
	if why := narrowerLists(b, g); why != "" {
		return why
	}
	if rb, rg := res(b), res(g); rg.CPUMillis > rb.CPUMillis || rg.MemoryMiB > rb.MemoryMiB || rg.PidsLimit > rb.PidsLimit || rg.DiskMiB > rb.DiskMiB {
		return "resources larger"
	}
	for _, tool := range genTools {
		if effTool(g, tool) < effTool(b, tool) {
			return "tool " + tool + " loosened"
		}
	}
	if effPackMiB(g.PushRules) > effPackMiB(b.PushRules) || (b.PushRules.IsSet() && effPushHold(g.PushRules) > effPushHold(b.PushRules)) {
		return "push bound looser"
	}
	return narrowerLimits(base.Limits, got.Limits)
}

func narrowerLists(b, g types.RunPolicySpec) string {
	for _, d := range b.DeniedDomains {
		if !slices.ContainsFunc(g.DeniedDomains, func(x string) bool { return strings.EqualFold(x, d) }) &&
			!slices.Contains(g.DeniedDomains, strings.ToLower(d)) {
			return "denied_domains lost " + d
		}
	}
	if len(b.AllowedMethods) > 0 && (len(g.AllowedMethods) == 0 || len(g.AllowedMethods) > len(b.AllowedMethods)) {
		return "allowed_methods widened"
	}
	if len(b.AzureDevOpsCapabilities) > 0 && (len(g.AzureDevOpsCapabilities) == 0 || len(g.AzureDevOpsCapabilities) > len(b.AzureDevOpsCapabilities)) {
		return "azure_devops_capabilities widened"
	}
	for _, a := range g.UIApps {
		if !slices.ContainsFunc(b.UIApps, func(x types.UIApp) bool { return x.Name == a.Name && x.Port == a.Port }) {
			return "ui_app added " + a.Name
		}
	}
	for _, m := range g.WorkspaceMounts {
		i := slices.IndexFunc(b.WorkspaceMounts, func(x types.WorkspaceMount) bool { return x.Source == m.Source && x.Target == m.Target })
		if i < 0 || (b.WorkspaceMounts[i].ReadOnlyOrDefault() && !m.ReadOnlyOrDefault()) {
			return "mount added or made writable " + m.Source
		}
	}
	if b.PushRules.IsSet() {
		for _, p := range b.PushRules.DenyPaths {
			if g.PushRules == nil || !slices.Contains(g.PushRules.DenyPaths, p) {
				return "push deny_path lost " + p
			}
		}
	}
	return ""
}

func narrowerLimits(b, g types.GovernanceLimits) string {
	for _, p := range [][2]int{
		{b.MaxConcurrentRuns, g.MaxConcurrentRuns}, {b.MaxCPUMillis, g.MaxCPUMillis}, {b.MaxMemoryMiB, g.MaxMemoryMiB},
		{b.MaxEndAheadSec, g.MaxEndAheadSec}, {b.MaxWaitSec, g.MaxWaitSec}, {b.PauseIdleAfterSec, g.PauseIdleAfterSec},
		{cmp.Or(b.DefaultEndSec, b.MaxEndAheadSec), cmp.Or(g.DefaultEndSec, g.MaxEndAheadSec)},
		{cmp.Or(b.DefaultWaitSec, b.MaxWaitSec), cmp.Or(g.DefaultWaitSec, g.MaxWaitSec)},
	} {
		if unbounded(p[1]) > unbounded(p[0]) {
			return "a limit loosened"
		}
	}
	if (b.DenyInteractive && !g.DenyInteractive) || (b.DenyUIApps && !g.DenyUIApps) || (g.AllowNoEnd && !b.AllowNoEnd) || (g.UserChangesLimits && !b.UserChangesLimits) {
		return "a switch loosened"
	}
	if br := b.AutonomyRubric; br != nil {
		gr := g.AutonomyRubric
		for i, lv := range []types.AutonomyLevel{br.EgressOpen, br.SecretsNone, br.ConfinementCC2} {
			if lv == "" {
				continue
			}
			var got types.AutonomyLevel
			if gr != nil {
				got = []types.AutonomyLevel{gr.EgressOpen, gr.SecretsNone, gr.ConfinementCC2}[i]
			}
			if got == "" || got.Rank() > lv.Rank() {
				return "an autonomy cap was removed or raised"
			}
		}
	}
	return ""
}

func canonOv(o composer.Overlay) string {
	buf, _ := json.Marshal(o)
	return string(buf)
}

func canon(a composer.Authority) string {
	buf, _ := json.Marshal(a)
	return string(buf)
}

// Narrowing: whatever the overlay says, the composed ceiling is at most as
// permissive as its base, judged by enforcement.
func TestMeetNarrowsAgainstEnforcement(t *testing.T) {
	err := quick.Check(func(c composition) bool {
		got, _, err := composer.ApplyOverlay(c.base, c.o1)
		if err != nil {
			return true
		}
		if why := narrower(c.base, got); why != "" {
			t.Logf("base %s\noverlay %s\ngot %s\nwhy: %s", canon(c.base), canonOv(c.o1), canon(got), why)
			return false
		}
		return true
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}

// Idempotence: applying an overlay twice is applying it once.
func TestMeetIsIdempotent(t *testing.T) {
	err := quick.Check(func(c composition) bool {
		once, _, err := composer.ApplyOverlay(c.base, c.o1)
		if err != nil {
			return true
		}
		twice, _, err := composer.ApplyOverlay(once, c.o1)
		if err != nil || canon(once) != canon(twice) {
			t.Logf("base %s\noverlay %s\nonce  %s\ntwice %s (err %v)", canon(c.base), canonOv(c.o1), canon(once), canon(twice), err)
			return false
		}
		return true
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}

// Order independence: two overlays compose to the same ceiling in either order,
// or fail in both. eligible_grants is outside the property (the resolver owns
// it, step by step, against each resolved base), and so is a push_rules overlay
// over a base with no active rule: there the broker's "0 means the default"
// only starts to apply once a rule is active, so which overlay activates it
// first is part of the answer.
func TestMeetIsOrderIndependent(t *testing.T) {
	err := quick.Check(func(c composition) bool {
		if !c.base.Ceiling.PushRules.IsSet() {
			c.o1.Ceiling.PushRules, c.o2.Ceiling.PushRules = nil, nil
		}
		c.o1.Ceiling.EligibleGrants, c.o2.Ceiling.EligibleGrants = nil, nil
		ab, _, errA1 := composer.ApplyOverlay(c.base, c.o1)
		var ba composer.Authority
		var errB1 error
		var errA2, errB2 error
		if errA1 == nil {
			ab, _, errA2 = composer.ApplyOverlay(ab, c.o2)
		}
		ba, _, errB1 = composer.ApplyOverlay(c.base, c.o2)
		if errB1 == nil {
			ba, _, errB2 = composer.ApplyOverlay(ba, c.o1)
		}
		failA, failB := errA1 != nil || errA2 != nil, errB1 != nil || errB2 != nil
		if failA != failB || (!failA && canon(ab) != canon(ba)) {
			t.Logf("base %s\no1 %s\no2 %s\nA %s (%v %v)\nB %s (%v %v)", canon(c.base), canonOv(c.o1), canonOv(c.o2), canon(ab), errA1, errA2, canon(ba), errB1, errB2)
			return false
		}
		return true
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}

// Strict and lenient agree where the overlay narrows: an overlay ValidateOverlay
// accepts composes to the same result with no warning.
func TestMeetStrictAcceptanceMeansNoWarning(t *testing.T) {
	err := quick.Check(func(c composition) bool {
		if composer.ValidateOverlay(c.base, c.o1) != nil {
			return true
		}
		_, warns, err := composer.ApplyOverlay(c.base, c.o1)
		return err == nil && len(warns) == 0
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}
