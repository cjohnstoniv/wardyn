// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer_test

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func leqGrant(kind types.GrantKind, scope string, approval, ownerOnly bool, ttl int) types.GrantSpec {
	return types.GrantSpec{Kind: kind, Scope: json.RawMessage(scope), RequiresApproval: approval, OwnerOnly: ownerOnly, TTLSeconds: ttl}
}

const (
	leqAPIScope  = `{"host":"api.vendor.example","secret_name":"vendor-key"}`
	leqPATScope  = `{"host":"git.corp.example","secret_name":"corp-pat","repos":["team/a","team/b"],"access":"read"}`
	leqBaseGrant = 3600
)

// leqBaseGrants is the grant ceiling the corpus bases carry.
func leqBaseGrants() []types.GrantSpec {
	return []types.GrantSpec{
		leqGrant(types.GrantAPIKey, leqAPIScope, true, true, leqBaseGrant),
		leqGrant(types.GrantGitPAT, leqPATScope, false, false, 900),
	}
}

// resolverGrants is what the resolver does with an overlay's grants after the
// meet passed them through: keep each one that is within the resolved base.
func resolverGrants(got *composer.Authority, base composer.Authority) {
	var kept []types.GrantSpec
	for _, g := range got.Ceiling.EligibleGrants {
		if composer.GrantWithin(g, base.Ceiling.EligibleGrants) == nil {
			kept = append(kept, g)
		}
	}
	got.Ceiling.EligibleGrants = kept
}

// overlayGrants is a random grant list for an overlay: the base's own grants,
// some tightened, some widened (which the resolver drops).
func overlayGrants(r *rand.Rand) *[]types.GrantSpec {
	if r.Intn(2) == 0 {
		return nil
	}
	out := []types.GrantSpec{}
	if r.Intn(2) == 0 {
		out = append(out, leqGrant(types.GrantAPIKey, leqAPIScope, true, true, pick(r, []int{300, leqBaseGrant, 0})))
	}
	if r.Intn(2) == 0 {
		out = append(out, leqGrant(types.GrantGitPAT, pick(r, []string{
			leqPATScope,
			`{"host":"git.corp.example","secret_name":"corp-pat","repos":["team/a"],"access":"read"}`,
			`{"host":"git.corp.example","secret_name":"corp-pat","repos":["team/a"],"access":"write"}`,
			`{"host":"git.corp.example","secret_name":"corp-pat","repos":["other/x"],"access":"read"}`,
		}), r.Intn(2) == 0, r.Intn(2) == 0, pick(r, []int{300, 900, 3600})))
	}
	return &out
}

type leqCase struct{ composition }

func (leqCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := composition{base: genAuthority(r), o1: genOverlay(r, false), o2: genOverlay(r, false)}
	c.base.Ceiling.EligibleGrants = leqBaseGrants()
	c.o1.Ceiling.EligibleGrants = overlayGrants(r)
	c.o2.Ceiling.EligibleGrants = overlayGrants(r)
	return reflect.ValueOf(leqCase{c})
}

func leqOf(a, b composer.Authority) bool {
	return composer.Leq(a.Ceiling, b.Ceiling, a.Limits, b.Limits)
}

// A composed ceiling is at most as permissive as its base.
func TestLeqHoldsForEveryComposition(t *testing.T) {
	err := quick.Check(func(c leqCase) bool {
		got, _, err := composer.ApplyOverlay(c.base, c.o1)
		if err != nil {
			return true
		}
		resolverGrants(&got, c.base)
		if !leqOf(got, c.base) {
			t.Logf("base %s\noverlay %s\ngot %s", canon(c.base), canonOv(c.o1), canon(got))
			return false
		}
		return true
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}

// Leq is judged against ENFORCEMENT, not against its own comparators: whenever
// it says x is at most as permissive as y, the independent oracle that compiles
// both through the proxy finds nothing x reaches that y does not. The pairs are
// two compositions of one base, so Leq is true often enough to be tested.
func TestLeqIsSoundAgainstEnforcement(t *testing.T) {
	proved := 0
	err := quick.Check(func(c leqCase) bool {
		x, _, errX := composer.ApplyOverlay(c.base, c.o1)
		y, _, errY := composer.ApplyOverlay(c.base, c.o2)
		if errX != nil || errY != nil {
			return true
		}
		for _, p := range [][2]composer.Authority{{x, y}, {y, x}, {x, c.base}, {c.base, x}} {
			if !leqOf(p[0], p[1]) {
				continue
			}
			proved++
			// narrower's denied_domains check compares spellings; its reach check
			// runs first and is the enforcement answer, and a wildcard denial that
			// covers a listed host is the same denial as far as the proxy goes.
			if why := narrower(p[1], p[0]); why != "" && !strings.HasPrefix(why, "denied_domains lost") {
				t.Logf("Leq(a, b) held but the oracle says %s\na %s\nb %s", why, canon(p[0]), canon(p[1]))
				return false
			}
		}
		return true
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
	if proved < 1000 {
		t.Errorf("Leq held for only %d pairs: the soundness check is not exercising it", proved)
	}
}

func TestLeqIsReflexiveAndTransitive(t *testing.T) {
	err := quick.Check(func(c leqCase) bool {
		x, _, errX := composer.ApplyOverlay(c.base, c.o1)
		if errX != nil {
			return true
		}
		y, _, errY := composer.ApplyOverlay(x, c.o2)
		if !leqOf(c.base, c.base) || !leqOf(x, x) {
			return false
		}
		if errY != nil {
			return true
		}
		// base >= x >= y, so y must be within base whenever each step is proved.
		return !(leqOf(x, c.base) && leqOf(y, x)) || leqOf(y, c.base)
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}

// leqFixture is one base the widening table edits. It is written out by hand so
// that no row's expectation is read back from the code under test.
func leqFixture() (types.RunPolicySpec, types.GovernanceLimits) {
	ro := true
	return types.RunPolicySpec{
			AllowedDomains:      []string{"*.corp.example", "pypi.org"},
			DeniedDomains:       []string{"evil.example"},
			FirstUseApproval:    types.FirstUseDenyWithReview,
			FirstUseHoldSeconds: 30,
			MaxHolds:            16,
			AllowedMethods:      []string{"GET", "POST"},
			MinConfinementClass: types.CC2,
			EligibleGrants:      leqBaseGrants(),
			AutoStopAfterSec:    600,
			WorkspaceMounts:     []types.WorkspaceMount{{Source: "/a", Target: "/w/a", ReadOnly: &ro}},
			WorkspaceRepos:      []types.WorkspaceRepo{{Repo: "corp/app", Target: "/w/app"}},
			UIApps:              []types.UIApp{{Name: "docs", Port: 8000}},
			Resources:           &types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096, PidsLimit: 256, DiskMiB: 1024},
			ToolRules:           []types.ToolRule{{Tool: "*", Effect: types.ToolHold}, {Tool: "Bash", Effect: types.ToolDeny}},
			PushRules:           &types.PushRulesSpec{DenyPaths: []string{".github/**"}, MaxInspectPackMiB: 8, HoldSeconds: 60},
		}, types.GovernanceLimits{
			DenyInteractive:   true,
			MaxConcurrentRuns: 4,
			MaxCPUMillis:      4000,
			AutonomyRubric:    &types.AutonomyRubric{EgressOpen: types.AutonomyL1},
			RunLimits: types.RunLimits{
				MaxEndAheadSec: 3600, DefaultEndSec: 600, MaxWaitSec: 300, PauseIdleAfterSec: 120,
			},
		}
}

func TestLeqFixtureIsWithinItself(t *testing.T) {
	c, l := leqFixture()
	if !composer.Leq(c, c, l, l) {
		t.Fatal("the fixture is not at most as permissive as itself")
	}
}

// Each row widens one thing against the fixture (after an optional edit to the
// base side), and Leq must say no.
func TestLeqIsFalseForEveryWidening(t *testing.T) {
	grant := func(c *types.RunPolicySpec, i int, edit func(*types.GrantSpec)) {
		c.EligibleGrants = slices.Clone(c.EligibleGrants)
		edit(&c.EligibleGrants[i])
	}
	rows := []struct {
		name string
		base func(*types.RunPolicySpec, *types.GovernanceLimits)
		wide func(*types.RunPolicySpec, *types.GovernanceLimits)
	}{
		{name: "an added domain", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedDomains = append(slices.Clone(c.AllowedDomains), "api.vendor.example")
		}},
		{name: "an exact host under the base's wildcard", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedDomains = []string{"*.vendor.example"}
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedDomains = []string{"api.vendor.example"}
		}},
		{name: "a wildcard widened past the base's", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedDomains = []string{"*.example"}
		}},
		{name: "a port dropped from an entry", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedDomains = []string{"api.corp.example:443"}
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedDomains = []string{"api.corp.example"}
		}},
		{name: "a lost denial", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.DeniedDomains = nil }},
		{name: "an allow-all flip", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.AllowAllEgress = true }},
		{name: "a removed method restriction", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.AllowedMethods = nil }},
		{name: "an added method", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedMethods = []string{"GET", "POST", "DELETE"}
		}},
		{name: "a looser first-use approval", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.FirstUseApproval = types.FirstUseWaitForReview
		}},
		{name: "a longer first-use hold", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.FirstUseHoldSeconds = 300 }},
		{name: "a hold of 0, which reads as the 30 s default, over a base of 5", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.FirstUseHoldSeconds = 5
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.FirstUseHoldSeconds = 0 }},
		{name: "more concurrent holds", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.MaxHolds = 64 }},
		{name: "a lowered confinement floor", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.MinConfinementClass = types.CC1 }},
		{name: "a cleared auto-stop", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.AutoStopAfterSec = 0 }},
		{name: "a writable mount", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			rw := false
			c.WorkspaceMounts = []types.WorkspaceMount{{Source: "/a", Target: "/w/a", ReadOnly: &rw}}
		}},
		{name: "an added mount", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.WorkspaceMounts = append(slices.Clone(c.WorkspaceMounts), types.WorkspaceMount{Source: "/b", Target: "/w/b"})
		}},
		{name: "a substituted workspace repo", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.WorkspaceRepos = []types.WorkspaceRepo{{Repo: "corp/other", Target: "/w/app"}}
		}},
		{name: "a dropped llm inspection", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.LLMInspection = &types.LLMInspectionSpec{Mode: "block"}
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.LLMInspection = nil }},
		{name: "a different llm inspection", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.LLMInspection = &types.LLMInspectionSpec{Mode: "block"}
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.LLMInspection = &types.LLMInspectionSpec{Mode: "warn"}
		}},
		{name: "an added ui app", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.UIApps = append(slices.Clone(c.UIApps), types.UIApp{Name: "jupyter", Port: 8888})
		}},
		{name: "a ui app moved to another path", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.UIApps = []types.UIApp{{Name: "docs", Port: 8000, Path: "/admin"}}
		}},
		{name: "a larger sandbox", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.Resources = &types.ResourceLimits{CPUMillis: 64000, MemoryMiB: 4096, PidsLimit: 256, DiskMiB: 1024}
		}},
		{name: "an unset disk limit", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.Resources = &types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096, PidsLimit: 256}
		}},
		{name: "a tool rule softened", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.ToolRules = []types.ToolRule{{Tool: "*", Effect: types.ToolHold}, {Tool: "Bash", Effect: types.ToolAllow}}
		}},
		{name: "the tool default softened", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.ToolRules = []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}, {Tool: "Bash", Effect: types.ToolDeny}}
		}},
		{name: "push-branch confinement turned off", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.GitPushAnyBranch = true }},
		{name: "a dropped push deny path", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.PushRules = &types.PushRulesSpec{DenyPaths: []string{"docs/"}, MaxInspectPackMiB: 8, HoldSeconds: 60}
		}},
		{name: "the pack cap above the default", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/**"}}
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/**"}, MaxInspectPackMiB: 64}
		}},
		{name: "push inspection dropped", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.PushRules = nil }},
		{name: "a longer push hold", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/**"}, MaxInspectPackMiB: 8, HoldSeconds: 300}
		}},
		{name: "a larger push file bound", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.PushRules.MaxFileSizeMiB = 5
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/**"}, MaxInspectPackMiB: 8, HoldSeconds: 60, MaxFileSizeMiB: 50}
		}},
		{name: "a capability added", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AzureDevOpsCapabilities = []adoscope.Capability{adoscope.CapCodeRead}
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AzureDevOpsCapabilities = []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapWorkRead}
		}},
		{name: "a capability list under an empty base", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AzureDevOpsCapabilities = []adoscope.Capability{adoscope.CapCodeRead}
		}},
		{name: "a GitHub capability added", base: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.GitHubCapabilities = []ghscope.Capability{ghscope.CapCodeRead}
		}, wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.GitHubCapabilities = []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapPR}
		}},
		{name: "a GitHub capability list under an empty base", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.GitHubCapabilities = []ghscope.Capability{ghscope.CapCodeRead}
		}},

		// Grants.
		{name: "a cleared owner_only", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			grant(c, 0, func(g *types.GrantSpec) { g.OwnerOnly = false })
		}},
		{name: "a stripped requires_approval", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			grant(c, 0, func(g *types.GrantSpec) { g.RequiresApproval = false })
		}},
		{name: "a ttl of 0, which reads as an hour, under a base of 900", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			grant(c, 1, func(g *types.GrantSpec) { g.TTLSeconds = 0 })
		}},
		{name: "a git_pat read to write", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			grant(c, 1, func(g *types.GrantSpec) {
				g.Scope = json.RawMessage(`{"host":"git.corp.example","secret_name":"corp-pat","repos":["team/a","team/b"],"access":"write"}`)
			})
		}},
		{name: "a git_pat access left unset, which reads as write", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			grant(c, 1, func(g *types.GrantSpec) {
				g.Scope = json.RawMessage(`{"host":"git.corp.example","secret_name":"corp-pat","repos":["team/a","team/b"]}`)
			})
		}},
		{name: "a substituted git_pat repo", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			grant(c, 1, func(g *types.GrantSpec) {
				g.Scope = json.RawMessage(`{"host":"git.corp.example","secret_name":"corp-pat","repos":["other/x"],"access":"read"}`)
			})
		}},
		{name: "git_pat repos left unset, which reads as every repository", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			grant(c, 1, func(g *types.GrantSpec) {
				g.Scope = json.RawMessage(`{"host":"git.corp.example","secret_name":"corp-pat","access":"read"}`)
			})
		}},
		{name: "git_pat api false to true", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			grant(c, 1, func(g *types.GrantSpec) {
				g.Scope = json.RawMessage(`{"host":"git.corp.example","secret_name":"corp-pat","repos":["team/a","team/b"],"access":"read","api":true}`)
			})
		}},
		{name: "a grant the base does not list", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.EligibleGrants = append(slices.Clone(c.EligibleGrants),
				leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"other-key"}`, true, true, 300))
		}},
		{name: "a grant of an unknown kind", wide: func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.EligibleGrants = append(slices.Clone(c.EligibleGrants), types.GrantSpec{Kind: "mystery"})
		}},

		// Limits.
		{name: "a lifted interactive denial", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DenyInteractive = false }},
		{name: "a lifted exec denial", base: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DenyTaskModeExec = true }, wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DenyTaskModeExec = false }},
		{name: "a lifted drive denial", base: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DenyUserDrive = true }, wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DenyUserDrive = false }},
		{name: "a lifted ui-app denial", base: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DenyUIApps = true }, wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DenyUIApps = false }},
		{name: "a raised concurrent-run cap", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxConcurrentRuns = 40 }},
		{name: "an unbounded concurrent-run cap", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxConcurrentRuns = 0 }},
		{name: "a raised cpu cap", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxCPUMillis = 64000 }},
		{name: "a raised memory cap", base: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxMemoryMiB = 512 }, wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxMemoryMiB = 8192 }},
		{name: "a raised disk cap", base: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxEphemeralDiskMiB = 512 }, wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxEphemeralDiskMiB = 8192 }},
		{name: "a raised drive cap", base: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxDriveSizeMiB = 512 }, wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxDriveSizeMiB = 8192 }},
		{name: "a raised rubric cap", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) {
			l.AutonomyRubric = &types.AutonomyRubric{EgressOpen: types.AutonomyL3}
		}},
		{name: "a lifted guardrail lock", base: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) {
			l.AutonomyRubric = &types.AutonomyRubric{AgentGuardrailLocks: true}
		}, wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) {
			l.AutonomyRubric = &types.AutonomyRubric{EgressOpen: types.AutonomyL1}
		}},
		{name: "a removed rubric cap", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.AutonomyRubric = nil }},
		{name: "a longer run lifetime", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxEndAheadSec = 86400 }},
		{name: "no run lifetime bound", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxEndAheadSec = 0 }},
		{name: "a longer default end", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DefaultEndSec = 3000 }},
		{name: "a default end of 0, which reads as the maximum", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DefaultEndSec = 0 }},
		{name: "runs with no end allowed", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.AllowNoEnd = true }},
		{name: "a longer wait", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxWaitSec = 3000 }},
		{name: "a longer default wait", base: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DefaultWaitSec = 60 }, wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DefaultWaitSec = 250 }},
		{name: "members may change limits", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.UserChangesLimits = true }},
		{name: "no idle pause", wide: func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.PauseIdleAfterSec = 0 }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b, lb := leqFixture()
			if r.base != nil {
				r.base(&b, &lb)
			}
			a, la := leqFixture()
			if r.base != nil {
				r.base(&a, &la)
			}
			if !composer.Leq(a, b, la, lb) {
				t.Fatal("the edited base is not within itself; the row is mis-built")
			}
			r.wide(&a, &la)
			if composer.Leq(a, b, la, lb) {
				t.Errorf("Leq held for a widening\na %+v %+v\nb %+v %+v", a, la, b, lb)
			}
		})
	}
}

// Narrowings the table above would be worthless without: each must be true.
func TestLeqIsTrueForNarrowings(t *testing.T) {
	rows := []struct {
		name   string
		narrow func(*types.RunPolicySpec, *types.GovernanceLimits)
	}{
		{"fewer domains", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedDomains = []string{"pypi.org"}
		}},
		{"no domains", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.AllowedDomains = []string{} }},
		{"a narrower wildcard", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.AllowedDomains = []string{"*.y.corp.example"}
		}},
		{"an added denial", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.DeniedDomains = []string{"evil.example", "worse.example"}
		}},
		{"one method", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.AllowedMethods = []string{"GET"} }},
		{"a stricter first-use approval", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.FirstUseApproval = types.FirstUseAlwaysDeny }},
		{"a higher confinement floor", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.MinConfinementClass = types.CC3 }},
		{"a shorter auto-stop", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.AutoStopAfterSec = 60 }},
		{"a smaller sandbox", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.Resources = &types.ResourceLimits{CPUMillis: 500, MemoryMiB: 1024, PidsLimit: 64, DiskMiB: 100}
		}},
		{"an added tool denial", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.ToolRules = []types.ToolRule{{Tool: "*", Effect: types.ToolDeny}, {Tool: "Bash", Effect: types.ToolDeny}}
		}},
		{"an added push deny path", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/**", "infra/"}, MaxInspectPackMiB: 4, HoldSeconds: 30}
		}},
		{"read-only for a read-write mount", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			ro := true
			c.WorkspaceMounts = []types.WorkspaceMount{{Source: "/a", Target: "/w/a", ReadOnly: &ro}}
		}},
		{"an added llm inspection", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.LLMInspection = &types.LLMInspectionSpec{Mode: "block"}
		}},
		{"a shorter grant ttl", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.EligibleGrants = slices.Clone(c.EligibleGrants)
			c.EligibleGrants[1].TTLSeconds = 300
		}},
		{"fewer git_pat repos", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) {
			c.EligibleGrants = slices.Clone(c.EligibleGrants)
			c.EligibleGrants[1].Scope = json.RawMessage(`{"host":"git.corp.example","secret_name":"corp-pat","repos":["team/a"],"access":"read"}`)
		}},
		{"fewer grants", func(c *types.RunPolicySpec, _ *types.GovernanceLimits) { c.EligibleGrants = c.EligibleGrants[:1] }},
		{"a lower rubric cap", func(_ *types.RunPolicySpec, l *types.GovernanceLimits) {
			l.AutonomyRubric = &types.AutonomyRubric{EgressOpen: types.AutonomyL0, SecretsNone: types.AutonomyL1}
		}},
		{"a shorter lifetime", func(_ *types.RunPolicySpec, l *types.GovernanceLimits) {
			l.MaxEndAheadSec = 1800
			l.DefaultEndSec = 600
		}},
		{"a smaller run cap", func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.MaxConcurrentRuns = 1 }},
		{"a new denial", func(_ *types.RunPolicySpec, l *types.GovernanceLimits) { l.DenyUserDrive = true }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b, lb := leqFixture()
			a, la := leqFixture()
			r.narrow(&a, &la)
			if !composer.Leq(a, b, la, lb) {
				t.Errorf("Leq refused a narrowing\na %+v %+v", a, la)
			}
			if composer.Leq(b, a, lb, la) && !reflect.DeepEqual(a, b) {
				t.Errorf("Leq held in both directions for a strict narrowing")
			}
		})
	}
}

// The ceiling's grant order, and the order of two grants for the same host, must
// not change the answer.
func TestLeqSameHostGrantsAgreeInBothOrders(t *testing.T) {
	strict := leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"strict-key"}`, true, true, 300)
	loose := leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"loose-key"}`, false, false, 3600)
	proposals := []types.GrantSpec{
		strict, loose,
		leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"strict-key"}`, false, true, 300), // strips approval
		leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"strict-key"}`, true, false, 300), // strips owner_only
		leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"strict-key"}`, true, true, 3600), // lengthens ttl
		leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"loose-key"}`, true, true, 60),    // tightens the loose pairing
		leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"unlisted-key"}`, true, true, 60), // no such pairing
		leqGrant(types.GrantAPIKey, `{"host":"API.Vendor.Example.","secret_name":"strict-key"}`, true, true, 300), // host spelling
	}
	want := []bool{true, true, false, false, false, true, false, true}
	for i, p := range proposals {
		var answers []bool
		for _, ceiling := range [][]types.GrantSpec{{strict, loose}, {loose, strict}} {
			a, b := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{p}}, types.RunPolicySpec{EligibleGrants: ceiling}
			answers = append(answers, composer.Leq(a, b, types.GovernanceLimits{}, types.GovernanceLimits{}))
			if got := composer.GrantWithin(p, ceiling) == nil; got != answers[len(answers)-1] {
				t.Errorf("proposal %d: GrantWithin = %v but Leq = %v", i, got, answers[len(answers)-1])
			}
		}
		if answers[0] != answers[1] || answers[0] != want[i] {
			t.Errorf("proposal %d (%s): got %v, want %v in both orders", i, p.Scope, answers, want[i])
		}
	}
}

func TestGrantWithinFailsClosed(t *testing.T) {
	ceiling := leqBaseGrants()
	for name, g := range map[string]types.GrantSpec{
		"an unknown kind":        {Kind: "mystery"},
		"an undecodable scope":   leqGrant(types.GrantAPIKey, `{not json`, true, true, 60),
		"a scope with no secret": leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example"}`, true, true, 60),
	} {
		if err := composer.GrantWithin(g, ceiling); err == nil {
			t.Errorf("%s: GrantWithin = nil, want a refusal", name)
		}
	}
}
