// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An approval rule against a decision a person already made on the same run: the real raise and decide
// routes over a real Postgres, an enforce rule, the egress_host gate on. A raise here is byte for byte what a
// run's proxy sends when it has no entry for the host, and a replaced proxy has none: it holds a new token and
// an empty cache, so a second token is a replacement as the control plane sees it. The proxy reads the row the
// way polled reads it, so APPROVED is a connection that goes through.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	ruleRowsSponsor = "sub-rule-rows-sponsor"
	ruleRowsHost    = "registry.npmjs.org"
	ruleRowsOther   = "pypi.org"
	ruleRowsRunBody = `{"decision_scope":"run"}`
)

// ruleRowsRaise is approvalClient.egressRaise's body for host under wait_for_review.
func ruleRowsRaise(host string) string {
	return `{"kind":"egress_domain","requested_scope":{"host":"` + host + `","mode":"wait_for_review","hold_seconds":30}}`
}

// ruleRowsUntilBody is a decision body at scope until, an hour out.
func ruleRowsUntilBody() string {
	return `{"decision_scope":"until","decision_expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
}

type ruleRowsEnv struct {
	*govEnv
	service string
	runs    int
}

// newRuleRowsEnv is one deployment with a live service identity allowed both hosts under an enforced egress_host.
func newRuleRowsEnv(t *testing.T) *ruleRowsEnv {
	t.Helper()
	t.Setenv(envApprovalRules, approvalRulesOn)
	pool := throwawayPGPool(t)
	e := newGovEnvOn(t, pool, func(c *Config) {
		c.Approvals = pgApprovalService{PG: store.NewPG(pool), rec: c.Audit}
	})
	ctx := context.Background()
	if _, err := e.pg.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: ruleRowsSponsor, Issuer: "https://idp.example", TenantID: "t",
		ObjectID: "o-rule-rows", Email: "sponsor@corp.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	si, err := e.pg.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-egress", Sponsor: ruleRowsSponsor, UserType: types.UserTypeStandard, CreatedBy: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{ruleRowsHost, ruleRowsOther} {
		if _, err := e.pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{SubjectType: types.CapabilitySubjectUser, Subject: si.Principal(),
			Capability: capEgressHost, Value: host, Effect: types.CapabilityAllow, CreatedBy: "seed"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pg.PutCapabilityEnforcement(ctx, map[string]bool{capEgressHost: true}); err != nil {
		t.Fatal(err)
	}
	return &ruleRowsEnv{govEnv: e, service: si.Principal()}
}

// ruleRowsRun is one service run on a repo of its own, so another run's rule never matches it.
type ruleRowsRun struct {
	id   uuid.UUID
	repo string
	tok  string
}

func (e *ruleRowsEnv) newRun() ruleRowsRun {
	e.t.Helper()
	e.runs++
	now, id, repo := time.Now().UTC(), uuid.New(), fmt.Sprintf("org/rule-rows-%d", e.runs)
	if _, err := e.pg.CreateRun(context.Background(), types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: e.service, HumanAuthor: ruleRowsSponsor,
		Agent: "claude-code", Repo: repo, ConfinementClass: types.CC2, State: types.RunRunning,
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker"}); err != nil {
		e.t.Fatalf("create run: %v", err)
	}
	return ruleRowsRun{id: id, repo: repo, tok: e.h.mintRunToken(e.t, id)}
}

// replaceProxy is the run's proxy replaced: a new token, and nothing cached.
func (e *ruleRowsEnv) replaceProxy(run ruleRowsRun) ruleRowsRun {
	e.t.Helper()
	run.tok = e.h.mintRunToken(e.t, run.id)
	return run
}

// raise posts the raise under the run's token and returns the row it answered.
func (e *ruleRowsEnv) raise(run ruleRowsRun, host string) types.ApprovalRequest {
	e.t.Helper()
	w := do(e.t, e.srv, http.MethodPost, "/api/v1/internal/approvals", run.tok, ruleRowsRaise(host))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("raise %s: %d %s", host, w.Code, w.Body)
	}
	var ap types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &ap); err != nil {
		e.t.Fatal(err)
	}
	return ap
}

// polled is the state the proxy's next poll reads (approvalClient.fetch).
func (e *ruleRowsEnv) polled(run ruleRowsRun, id uuid.UUID) types.ApprovalState {
	e.t.Helper()
	w := do(e.t, e.srv, http.MethodGet, "/api/v1/internal/approvals/"+id.String(), run.tok, "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("poll %s: %d %s", id, w.Code, w.Body)
	}
	var got sandboxApproval
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		e.t.Fatal(err)
	}
	return got.State
}

// decide is a security admin's decision through the real route; verb is approve or deny.
func (e *ruleRowsEnv) decide(id uuid.UUID, verb, body string) {
	e.t.Helper()
	if w := e.call(e.alice, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, body); w.Code != http.StatusOK {
		e.t.Fatalf("the person's %s: %d %s", verb, w.Code, w.Body)
	}
}

// lapse moves a decided row's until into the past: decide refuses to write one there.
func (e *ruleRowsEnv) lapse(id uuid.UUID) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE approvals SET decision_expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
}

// putRule writes an enforce rule for the run's repo. It starts matching only now, as a dry_run promoted to enforce does.
func (e *ruleRowsEnv) putRule(run ruleRowsRun) types.ApprovalRule {
	e.t.Helper()
	rule, err := e.pg.PutApprovalRule(context.Background(), types.ApprovalRuleWrite{Name: fmt.Sprintf("rule-rows-%d", e.runs), Mode: types.ApprovalRuleEnforce,
		ApprovalRuleSpec: types.ApprovalRuleSpec{Kinds: []types.ApprovalKind{types.ApprovalEgressDomain},
			Match:   types.ApprovalRuleMatch{Repos: []string{run.repo}},
			Approve: types.ApprovalRuleApprove{Hosts: []string{ruleRowsHost, ruleRowsOther}, EgressScope: types.ScopeRun}}}, "seed")
	if err != nil {
		e.t.Fatal(err)
	}
	return rule
}

// ruleDecided reports whether an approval.decide row names the rule as the decider of approval id.
func (e *ruleRowsEnv) ruleDecided(rule types.ApprovalRule, id uuid.UUID) bool {
	return slices.ContainsFunc(e.audits("approval.decide"), func(ev types.AuditEvent) bool {
		return ev.Actor == "policy:"+rule.ID.String() && ev.Target == id.String()
	})
}

// wantLeftForAPerson asserts the rule did not decide the row, and that its one skip row tells a reader why.
func (e *ruleRowsEnv) wantLeftForAPerson(run ruleRowsRun, rule types.ApprovalRule, ap types.ApprovalRequest) {
	e.t.Helper()
	if got := e.polled(run, ap.ID); got != types.ApprovalPending || e.ruleDecided(rule, ap.ID) {
		e.t.Errorf("the row after a deny that stands: state %s, decided by the rule %v; want PENDING, undecided", got, e.ruleDecided(rule, ap.ID))
	}
	var skips []types.AuditEvent
	for _, ev := range e.audits("approval.policy.skip") {
		if ev.Target == ap.ID.String() {
			skips = append(skips, ev)
		}
	}
	if len(skips) != 1 || skips[0].Actor != "policy:"+rule.ID.String() || skipReason(e.t, skips[0]) != "approval_rule_host_denied" {
		e.t.Errorf("skip rows = %+v; want one, by the rule, reason approval_rule_host_denied", skips)
	}
}

// wantRuleApproved asserts the rule decided the row: the host is reachable, and the rule is live on this run.
func (e *ruleRowsEnv) wantRuleApproved(run ruleRowsRun, rule types.ApprovalRule, ap types.ApprovalRequest) {
	e.t.Helper()
	if got := e.polled(run, ap.ID); got != types.ApprovalApproved || !e.ruleDecided(rule, ap.ID) {
		e.t.Errorf("state %s, decided by the rule %v; want the rule's APPROVED", got, e.ruleDecided(rule, ap.ID))
	}
}

// TestPG_ApprovalRuleAndAPersonsDenyAcrossAProxyReplacement: a deny still in force keeps the host from a rule on a row a
// replaced proxy raises. A deny no longer in force (a once, a lapsed until) is the ordinary re-ask a rule may answer.
// A second host on the same run shows the rule was live throughout.
func TestPG_ApprovalRuleAndAPersonsDenyAcrossAProxyReplacement(t *testing.T) {
	e := newRuleRowsEnv(t)
	for _, tc := range []struct {
		name, denyBody string
		lapse, stands  bool
	}{
		{name: "run", denyBody: ruleRowsRunBody, stands: true},
		{name: "no scope named", denyBody: "", stands: true},
		{name: "until, not lapsed", denyBody: ruleRowsUntilBody(), stands: true},
		{name: "until, lapsed", denyBody: ruleRowsUntilBody(), lapse: true},
		{name: "once", denyBody: `{"decision_scope":"once"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.t = t
			run := e.newRun()
			first := e.raise(run, ruleRowsHost)
			if got := e.polled(run, first.ID); got != types.ApprovalPending {
				t.Fatalf("first raise with no rule yet: %s, want PENDING", got)
			}
			e.decide(first.ID, "deny", tc.denyBody)
			if tc.lapse {
				e.lapse(first.ID)
			}
			rule := e.putRule(run)

			run = e.replaceProxy(run)
			second := e.raise(run, ruleRowsHost)
			if tc.stands {
				e.wantLeftForAPerson(run, rule, second)
			} else {
				e.wantRuleApproved(run, rule, second)
			}
			if got := e.polled(run, first.ID); got != types.ApprovalDenied {
				t.Errorf("the person's own row reads %s, want DENIED", got)
			}
			e.wantRuleApproved(run, rule, e.raise(run, ruleRowsOther))
		})
	}
}

// TestPG_ApprovalRuleAfterAPersonsLaterAllow: a person can always undo their own deny. A later allow still in force
// lets a rule answer the next row. A once allow released its one connection and a lapsed until is over, so the deny
// stands again.
func TestPG_ApprovalRuleAfterAPersonsLaterAllow(t *testing.T) {
	e := newRuleRowsEnv(t)
	for _, tc := range []struct {
		name, allowBody string
		lapse, undoes   bool
	}{
		{name: "run", allowBody: ruleRowsRunBody, undoes: true},
		{name: "no scope named", allowBody: "", undoes: true},
		{name: "until, not lapsed", allowBody: ruleRowsUntilBody(), undoes: true},
		{name: "until, lapsed", allowBody: ruleRowsUntilBody(), lapse: true},
		{name: "once", allowBody: `{"decision_scope":"once"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.t = t
			run := e.newRun()
			e.decide(e.raise(run, ruleRowsHost).ID, "deny", ruleRowsRunBody)
			rule := e.putRule(run)

			run = e.replaceProxy(run)
			second := e.raise(run, ruleRowsHost)
			e.wantLeftForAPerson(run, rule, second)
			e.decide(second.ID, "approve", tc.allowBody)
			if got := e.polled(run, second.ID); got != types.ApprovalApproved {
				t.Fatalf("the person's allow reads %s, want APPROVED", got)
			}
			if tc.lapse {
				e.lapse(second.ID)
			}

			run = e.replaceProxy(run)
			third := e.raise(run, ruleRowsHost)
			if tc.undoes {
				e.wantRuleApproved(run, rule, third)
			} else {
				e.wantLeftForAPerson(run, rule, third)
			}
		})
	}
}

// TestPG_ApprovalRuleAfterAPersonDeniesAgain: the newest decision wins, so a deny after an allow stands.
func TestPG_ApprovalRuleAfterAPersonDeniesAgain(t *testing.T) {
	e := newRuleRowsEnv(t)
	run := e.newRun()
	e.decide(e.raise(run, ruleRowsHost).ID, "deny", ruleRowsRunBody)
	run = e.replaceProxy(run)
	e.decide(e.raise(run, ruleRowsHost).ID, "approve", ruleRowsRunBody)
	run = e.replaceProxy(run)
	e.decide(e.raise(run, ruleRowsHost).ID, "deny", ruleRowsRunBody)
	rule := e.putRule(run)

	run = e.replaceProxy(run)
	e.wantLeftForAPerson(run, rule, e.raise(run, ruleRowsHost))
}

// TestPG_ApprovalRuleLeavesARowThatStillExpires: the row a rule leaves is an ordinary PENDING row. The sweep expires it
// with nobody deciding, which undoes nothing: the next raise is left for a person too.
func TestPG_ApprovalRuleLeavesARowThatStillExpires(t *testing.T) {
	e := newRuleRowsEnv(t)
	run := e.newRun()
	e.decide(e.raise(run, ruleRowsHost).ID, "deny", ruleRowsRunBody)
	rule := e.putRule(run)

	run = e.replaceProxy(run)
	left := e.raise(run, ruleRowsHost)
	e.wantLeftForAPerson(run, rule, left)
	if n, err := approval.ExpireStale(context.Background(), pgApprovalService{PG: e.pg, rec: e.cfg.Audit}, 0); err != nil || n != 1 {
		t.Fatalf("the sweep expired %d rows, %v; want the one left", n, err)
	}
	if got := e.polled(run, left.ID); got != types.ApprovalExpired {
		t.Fatalf("the swept row reads %s, want EXPIRED", got)
	}
	e.wantLeftForAPerson(run, rule, e.raise(run, ruleRowsHost))
}
