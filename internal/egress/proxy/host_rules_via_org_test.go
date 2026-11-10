// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A via_org request is judged against the run's host rules at the organisation, per request: the engine
// keeps the run's policy, so its rules travel with the reduced config. Each case is a fresh engine with
// the run's via_org route kept, as wardynd's reduction keeps it, and rules naming the route's host.

// viaOrgRuleFixture is newViaOrgFixture with the via_org routes wardynd keeps (the header grant's host and
// the GitHub grant's) and rules on host.
func viaOrgRuleFixture(t *testing.T, rules ...types.HostRule) *viaOrgFixture {
	t.Helper()
	return newViaOrgFixture(t, nil, func(c *Config) {
		c.ViaOrgRoutes = []ViaOrgRoute{{GrantID: c.Injection[0].GrantID, Host: viaOrgHost}, {GrantID: c.GitGrants["octocat/hello-world"], Host: githubHost}}
		c.Policy.HostRules = []types.HostRuleSet{{Rules: rules}}
		// wardynd's reduction loads the stored config (LoadConfigBytes), which runs this check first.
		if err := c.validateHostRules(); err != nil {
			t.Fatalf("the run's config does not load: %v", err)
		}
	})
}

// wantViaOrgRuleOutcome checks one via_org request's status, whether the upstream was reached, and that a
// refusal wrote its policy:host-rule row (with cause, when given).
func wantViaOrgRuleOutcome(t *testing.T, f *viaOrgFixture, label string, status int, wantPass bool, cause string) {
	t.Helper()
	hits, _, _ := f.up.seen()
	rows := f.decisions(t)
	if wantPass {
		if status != http.StatusOK || hits != 1 {
			t.Errorf("%s: status %d, upstream hits %d, want 200 and one hit", label, status, hits)
		}
		return
	}
	if status != http.StatusForbidden || hits != 0 {
		t.Errorf("%s: status %d, upstream hits %d, want 403 and never reached", label, status, hits)
	}
	if !strings.Contains(rows, `"rule_source":"`+ruleSourceHostRule+`"`) || !strings.Contains(rows, cause) {
		t.Errorf("%s: no %s row with %q:\n%s", label, ruleSourceHostRule, cause, rows)
	}
}

// The git form: the engine's broker sends to github.com through roundTripUpstream, where the outbound gate
// judges the method and path GitHub receives.
func TestViaOrgGitBrokerJudgedByHostRules(t *testing.T) {
	const push = "/wardyn/gh/octocat/hello-world.git/git-upload-pack"
	t.Run("allow", func(t *testing.T) {
		f := viaOrgRuleFixture(t, types.HostRule{Host: githubHost, Methods: []string{"POST"}, Paths: []string{"/octocat/hello-world.git"}, Effect: types.HostRuleAllow})
		wantViaOrgRuleOutcome(t, f, "allowed fetch", f.broker(t, push, "0000").Code, true, "")
	})
	t.Run("deny", func(t *testing.T) {
		f := viaOrgRuleFixture(t,
			types.HostRule{Host: githubHost, Paths: []string{"/octocat/hello-world.git/git-upload-pack"}, Effect: types.HostRuleDeny},
			types.HostRule{Host: githubHost, Effect: types.HostRuleAllow})
		wantViaOrgRuleOutcome(t, f, "denied fetch", f.broker(t, push, "0000").Code, false, "rule 0 denies")
	})
	t.Run("no matching rule", func(t *testing.T) {
		f := viaOrgRuleFixture(t, types.HostRule{Host: githubHost, Methods: []string{"GET"}, Effect: types.HostRuleAllow})
		wantViaOrgRuleOutcome(t, f, "unmatched fetch", f.broker(t, push, "0000").Code, false, "no rule in it matches POST")
	})
	// A path not in normal form never reaches the gate on this form: the broker builds GitHub's path from
	// a checked owner, repository and verb, and refuses anything else first.
	t.Run("not normal form", func(t *testing.T) {
		f := viaOrgRuleFixture(t, types.HostRule{Host: githubHost, Effect: types.HostRuleAllow})
		w := f.broker(t, "/wardyn/gh/octocat/hello-world.git/./git-upload-pack", "0000")
		if hits, _, _ := f.up.seen(); w.Code < 400 || hits != 0 {
			t.Errorf("status %d, upstream hits %d, want a refusal before GitHub", w.Code, hits)
		}
	})
}
