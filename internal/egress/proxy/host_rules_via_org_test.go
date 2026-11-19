// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	return viaOrgRuleFixtureWith(t, &fakeOrg{}, rules...)
}

// viaOrgRuleFixtureWith is viaOrgRuleFixture on org, so two engines can share one organisation.
func viaOrgRuleFixtureWith(t *testing.T, org *fakeOrg, rules ...types.HostRule) *viaOrgFixture {
	t.Helper()
	return newViaOrgFixtureWith(t, org, nil, func(c *Config) {
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

// viaOrgHoldRules hold POST /v1/messages on the via_org host and allow everything else there.
func viaOrgHoldRules() []types.HostRule {
	return []types.HostRule{
		{Host: viaOrgHost, Methods: []string{"POST"}, Paths: []string{"/v1/messages"}, Effect: types.HostRuleHold},
		{Host: viaOrgHost, Effect: types.HostRuleAllow},
	}
}

// shortHostCallWindow makes the hold window and its poll milliseconds for the test.
func shortHostCallWindow(t *testing.T) {
	prevPoll, prevSecond := holdPollInterval, hostCallSecond
	holdPollInterval, hostCallSecond = 2*time.Millisecond, time.Millisecond
	t.Cleanup(func() { holdPollInterval, hostCallSecond = prevPoll, prevSecond })
}

// A host rule hold on a via_org host is held by the organisation's engine: undecided, the sandbox reads the
// pending 403; approved, the request reaches the host; declined, it is refused. The answers live in that
// engine, so a new engine for the run asks again.
func TestViaOrgHoldIsHeldAtTheOrganisation(t *testing.T) {
	shortHostCallWindow(t)
	for _, tc := range []struct {
		state  types.ApprovalState
		status int
		pass   bool
		source string
	}{
		{"", http.StatusForbidden, false, ruleSourceHostRuleApprovalWait},
		{types.ApprovalApproved, http.StatusOK, true, ruleSourceHostRuleApprovalApproved},
		{types.ApprovalDenied, http.StatusForbidden, false, ruleSourceHostRuleApprovalDenied},
	} {
		org := &fakeOrg{pollState: tc.state}
		f := viaOrgRuleFixtureWith(t, org, viaOrgHoldRules()...)
		w := f.inject(http.MethodPost, "/v1/messages", nil)
		hits, _, _ := f.up.seen()
		if w.Code != tc.status || (hits == 1) != tc.pass {
			t.Errorf("%q: status %d, upstream hits %d, want %d and reached=%v", tc.state, w.Code, hits, tc.status, tc.pass)
		}
		if tc.state == "" && w.Header().Get(egressHeaderStatus) != egressRefusalPending {
			t.Errorf("undecided: %v, want the pending answer", w.Header())
		}
		if rows := f.decisions(t); !strings.Contains(rows, `"rule_source":"`+tc.source+`"`) {
			t.Errorf("%q: no %s row:\n%s", tc.state, tc.source, rows)
		}
	}

	org := &fakeOrg{pollState: types.ApprovalApproved}
	for i := range 2 {
		f := viaOrgRuleFixtureWith(t, org, viaOrgHoldRules()...)
		if w := f.inject(http.MethodPost, "/v1/messages", nil); w.Code != http.StatusOK {
			t.Fatalf("engine %d: status %d, want 200", i, w.Code)
		}
		_ = f.engine.Close(context.Background())
	}
	if org.mu.Lock(); org.raised != 2 {
		t.Errorf("two engines raised %d approvals, want each to ask", org.raised)
	}
	org.mu.Unlock()
}

// deadlineWriter records the read deadline a handler sets on its connection.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (d *deadlineWriter) SetReadDeadline(t time.Time) error { d.deadline = t; return nil }

// A via_org request the host rules hold waits at the organisation, up to the hold window, before its body
// is read, so the sandbox proxy gives the body its full read time after that window. A request the rules
// allow keeps the deadline its headers set.
func TestViaOrgHeldRequestBodyKeepsItsReadTime(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Resolver: publicResolver{},
		Sink:         &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		ViaOrgRoutes: []ViaOrgRoute{{GrantID: uuid.New(), Host: viaOrgHost}},
		Policy:       CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{viaOrgHost}, HostRules: []types.HostRuleSet{{Rules: viaOrgHoldRules()}}})})
	for _, tc := range []struct {
		method string
		held   bool
	}{{http.MethodPost, true}, {http.MethodGet, false}} {
		w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		start := time.Now()
		p.serveMITMRequest(w, httptest.NewRequest(tc.method, "https://"+viaOrgHost+"/v1/messages", strings.NewReader("{}")), viaOrgHost, 443)
		want := start.Add(hostCallWindow*hostCallSecond + mitmReadTimeout)
		if tc.held && w.deadline.Before(want) {
			t.Errorf("held %s: read deadline %v, want at least %v (the window plus the read time)", tc.method, w.deadline, want)
		}
		if !tc.held && !w.deadline.IsZero() {
			t.Errorf("allowed %s: read deadline set to %v, want the one its headers set", tc.method, w.deadline)
		}
	}
}
