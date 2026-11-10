// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A via_org request is judged against the run's host rules at the organisation, per request: the engine
// keeps the run's policy, so its rules travel with the reduced config. Each case is a fresh engine whose
// rules name a via_org route's host.

// viaOrgRuleFixture is newViaOrgFixture with rules, loaded beside the via_org routes wardynd keeps (the
// header grant's host and the GitHub grant's).
func viaOrgRuleFixture(t *testing.T, rules ...types.HostRule) *viaOrgFixture {
	t.Helper()
	return newViaOrgFixture(t, nil, func(c *Config) {
		c.ViaOrgRoutes = []ViaOrgRoute{{GrantID: c.Injection[0].GrantID, Host: viaOrgHost}, {GrantID: c.GitGrants["octocat/hello-world"], Host: githubHost}}
		c.Policy.HostRules = []types.HostRuleSet{{Rules: rules}}
		// wardynd's reduction loads the stored config, routes included (LoadConfigBytes runs this check),
		// then builds the engine without the routes (viaOrgEngine), so the engine serves them itself.
		if err := c.validateHostRules(); err != nil {
			t.Fatalf("the run's config does not load: %v", err)
		}
		c.ViaOrgRoutes = nil
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

// The header-injected form: the engine terminates nothing itself, so the destination check does not ask
// for an interception certificate it has no use for; the request's own method, host and path are judged.
func TestViaOrgInjectedJudgedByHostRules(t *testing.T) {
	allowV1 := types.HostRule{Host: viaOrgHost, Methods: []string{"POST"}, Paths: []string{"/v1"}, Effect: types.HostRuleAllow}
	t.Run("allow", func(t *testing.T) {
		f := viaOrgRuleFixture(t, allowV1)
		wantViaOrgRuleOutcome(t, f, "allowed POST", f.inject(http.MethodPost, "/v1/messages", nil).Code, true, "")
	})
	t.Run("deny", func(t *testing.T) {
		f := viaOrgRuleFixture(t, types.HostRule{Host: viaOrgHost, Paths: []string{"/v1/admin"}, Effect: types.HostRuleDeny}, allowV1)
		wantViaOrgRuleOutcome(t, f, "denied POST", f.inject(http.MethodPost, "/v1/admin", nil).Code, false, "rule 0 denies")
	})
	t.Run("another method", func(t *testing.T) {
		f := viaOrgRuleFixture(t, allowV1)
		wantViaOrgRuleOutcome(t, f, "DELETE", f.inject(http.MethodDelete, "/v1/messages", nil).Code, false, "no rule in it matches DELETE")
	})
	for _, path := range []string{"/v1/x/../admin", "/v1//messages", "/v1/messages;x=1", "/v1/messages%3Bx=1"} {
		t.Run("not normal form "+path, func(t *testing.T) {
			f := viaOrgRuleFixture(t, allowV1)
			wantViaOrgRuleOutcome(t, f, path, f.inject(http.MethodPost, path, nil).Code, false, "not in normal form")
		})
	}
}

// A sandbox's own CONNECT to a rule host with no interception certificate is still refused as opaque: the
// engine's allowance is for the request it already holds, not for a tunnel.
func TestViaOrgEngineAllowanceIsNotASandboxConnect(t *testing.T) {
	p, _, _ := hrProxy(t, hrOpts{noCA: true}, captureUpstream(t, true, "ok").srv)
	p.engine = true
	if d, _, log := p.evaluate(t.Context(), hrHost, 443, http.MethodConnect, ""); d != egress.Deny || log == nil || log.RuleSource != ruleSourceHostRuleNeedsInspection {
		t.Fatalf("an engine's CONNECT outside ServeInjected = %s %+v, want deny %s", d, log, ruleSourceHostRuleNeedsInspection)
	}
}
