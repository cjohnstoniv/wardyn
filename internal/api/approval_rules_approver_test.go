// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ruleStore is authzStore plus the stores the approver reads: approval rules, the service identity, capability
// grants and the boundary.
type ruleStore struct {
	*authzStore
	store.ServiceIdentityStore // only GetServiceIdentity is asked

	mu        sync.Mutex
	rules     []types.ApprovalRule
	ruleReads int
	// bumpOnGet is a rule write landing between the hook's match and decide()'s re-read.
	bumpOnGet bool
	listErr   error
	svc       types.ServiceIdentity
	grants    []types.CapabilityGrant
	enf       map[string]bool
	boundary  types.Boundary
	// endInDecide ends the run on decide()'s own read (a policy decider context): after a dry run's match.
	endInDecide bool
}

func (s *ruleStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if _, inDecide := policyDeciderFrom(ctx); inDecide && s.endInDecide {
		s.authzStore.mu.Lock()
		r := s.runs[id]
		r.State = types.RunCompleted
		s.runs[id] = r
		s.authzStore.mu.Unlock()
	}
	return s.authzStore.GetRun(ctx, id)
}

func (s *ruleStore) ListApprovalRules(context.Context) ([]types.ApprovalRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleReads++
	return slices.Clone(s.rules), s.listErr
}

func (s *ruleStore) GetApprovalRule(_ context.Context, name string) (types.ApprovalRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleReads++
	for i, r := range s.rules {
		if r.Name == name {
			if s.bumpOnGet {
				s.rules[i].Version++
			}
			return s.rules[i], nil
		}
	}
	return types.ApprovalRule{}, store.ErrNotFound
}

func (s *ruleStore) PutApprovalRule(context.Context, types.ApprovalRuleWrite, string) (types.ApprovalRule, error) {
	return types.ApprovalRule{}, errors.New("ruleStore: not written here")
}

func (s *ruleStore) DeleteApprovalRule(context.Context, string) (types.ApprovalRule, error) {
	return types.ApprovalRule{}, errors.New("ruleStore: not written here")
}

func (s *ruleStore) GetServiceIdentity(_ context.Context, name string) (types.ServiceIdentity, error) {
	if name != s.svc.Name {
		return types.ServiceIdentity{}, store.ErrNotFound
	}
	return s.svc, nil
}

func (s *ruleStore) ListCapabilityGrantsFor(_ context.Context, users, _ []string, userType string) ([]types.CapabilityGrant, error) {
	var out []types.CapabilityGrant
	for _, g := range s.grants {
		if g.SubjectType == types.CapabilitySubjectAll ||
			(g.SubjectType == types.CapabilitySubjectUser && slices.Contains(users, g.Subject)) ||
			(g.SubjectType == types.CapabilitySubjectUserType && g.Subject == userType) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *ruleStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return s.enf, nil
}

func (s *ruleStore) GetBoundary(context.Context) (types.Boundary, error) { return s.boundary, nil }

func (s *ruleStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return types.Boundary{}, errors.New("ruleStore: not written here")
}

// ruleApprovals runs the real approval FSM (dedup on raise, the approval.decide row on decide) over authzApprovals'
// rows, so a pin counts the rows production writes.
type ruleApprovals struct {
	*authzApprovals
	rec       *recRecorder
	decideErr error
	decides   int
	created   []types.ApprovalRequest
}

func (a *ruleApprovals) Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	got, err := approval.RequestApproval(ctx, fsmStore{a}, req)
	a.created = append(a.created, got)
	return got, err
}

func (a *ruleApprovals) Decide(ctx context.Context, id uuid.UUID, t types.ActorType, d types.ApprovalDecision) (types.ApprovalRequest, error) {
	if a.decideErr != nil {
		return types.ApprovalRequest{}, a.decideErr
	}
	a.decides++
	return approval.Decide(ctx, fsmStore{a}, id, t, d)
}

// CancelForRun runs the real cascade, so its state change and its approval.cancel row (st.Record) reach the
// fake store and the recorder a test counts.
func (a *ruleApprovals) CancelForRun(ctx context.Context, runID uuid.UUID, reason string) (map[string]int, error) {
	return approval.CancelForRun(ctx, fsmStore{a}, runID, reason)
}

type fsmStore struct{ a *ruleApprovals }

func (f fsmStore) CreateApproval(_ context.Context, ap types.ApprovalRequest) (types.ApprovalRequest, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	f.a.byID[ap.ID] = ap
	return ap, nil
}

func (f fsmStore) GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return f.a.Get(ctx, id)
}

func (f fsmStore) ListExpiredPendingApprovals(context.Context, time.Time, time.Time, uuid.UUID, int) ([]types.ApprovalRequest, error) {
	return nil, nil
}

func (f fsmStore) ListPendingApprovalsForRuns(_ context.Context, ids []uuid.UUID) ([]types.ApprovalRequest, error) {
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	var out []types.ApprovalRequest
	for _, ap := range f.a.byID {
		if ap.State == types.ApprovalPending && slices.Contains(ids, ap.RunID) {
			out = append(out, ap)
		}
	}
	return out, nil
}

func (f fsmStore) DecideApproval(ctx context.Context, id uuid.UUID, d types.ApprovalDecision) (types.ApprovalRequest, error) {
	return f.a.authzApprovals.Decide(ctx, id, "", d)
}

func (f fsmStore) Record(ctx context.Context, ev types.AuditEvent) error {
	return f.a.rec.Record(ctx, ev)
}

type ruleFix struct {
	t   *testing.T
	h   *harness
	srv *Server
	st  *ruleStore
	aps *ruleApprovals
	run types.AgentRun
	tok string
}

func testApprovalRule() types.ApprovalRule {
	return types.ApprovalRule{ID: uuid.New(), Name: "ci-review", Version: 1, Mode: types.ApprovalRuleEnforce,
		ApprovalRuleSpec: types.ApprovalRuleSpec{
			Kinds: []types.ApprovalKind{types.ApprovalToolCall, types.ApprovalEgressDomain},
			Match: types.ApprovalRuleMatch{Repos: []string{"Org/Repo"}},
			Approve: types.ApprovalRuleApprove{Tools: []string{"mcp__github__create_pull_request_review"},
				Hosts: []string{"registry.npmjs.org"}, EgressScope: types.ScopeRun},
		}}
}

// newRuleFix is a server with one running service-identity run (service:ci, sponsored by sub-sponsor) and the rule
// testApprovalRule, edited by mutate before the server is built.
func newRuleFix(t *testing.T, mutate ...func(*ruleFix)) *ruleFix {
	t.Helper()
	h := newHarness(t)
	ast := newAuthzStore()
	f := &ruleFix{t: t, h: h,
		st:  &ruleStore{authzStore: ast, svc: types.ServiceIdentity{Name: "ci", Sponsor: "sub-sponsor", UserType: "ci-bots"}, rules: []types.ApprovalRule{testApprovalRule()}},
		aps: &ruleApprovals{authzApprovals: newAuthzApprovals(ast), rec: h.audit},
		run: types.AgentRun{ID: uuid.New(), CreatedBy: "service:ci", HumanAuthor: "sub-sponsor", State: types.RunRunning, Repo: "org/repo"},
	}
	for _, m := range mutate {
		m(f)
	}
	ast.runs[f.run.ID] = f.run
	cfg := baseTestConfig(h, f.st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = f.aps
	f.srv = New(cfg)
	f.tok = h.mintRunToken(t, f.run.ID)
	return f
}

const (
	ruleToolScope   = `{"tool":"mcp__github__create_pull_request_review"}`
	ruleEgressScope = `{"host":"registry.npmjs.org"}`
)

// raise posts the sidecar's raise and returns the answer and the row it created.
func (f *ruleFix) raise(kind types.ApprovalKind, scope string) (*httptest.ResponseRecorder, types.ApprovalRequest) {
	f.t.Helper()
	w := do(f.t, f.srv, http.MethodPost, "/api/v1/internal/approvals", f.tok, `{"kind":"`+string(kind)+`","requested_scope":`+scope+`}`)
	if w.Code != http.StatusCreated {
		f.t.Fatalf("raise %s: %d %s", kind, w.Code, w.Body)
	}
	return w, f.aps.created[len(f.aps.created)-1]
}

// answerIsTheCreatedRow: the raise answers the row as it was created, byte for byte, whatever a rule did after.
func (f *ruleFix) answerIsTheCreatedRow(w *httptest.ResponseRecorder, created types.ApprovalRequest) {
	f.t.Helper()
	want := httptest.NewRecorder()
	writeJSON(want, http.StatusCreated, created)
	if !bytes.Equal(w.Body.Bytes(), want.Body.Bytes()) || created.State != types.ApprovalPending {
		f.t.Fatalf("raise answer changed:\n got %s\nwant %s", w.Body, want.Body)
	}
}

func (f *ruleFix) state(id uuid.UUID) types.ApprovalState {
	f.t.Helper()
	ap, err := f.aps.Get(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return ap.State
}

func (f *ruleFix) rows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// pending asserts the row stayed PENDING with no decision and no would-approve, and returns its skip rows.
func (f *ruleFix) pending(id uuid.UUID) []types.AuditEvent {
	f.t.Helper()
	return f.undecided(id, types.ApprovalPending)
}

// undecided asserts the row is in state with no decision and no would-approve, and returns its skip rows.
func (f *ruleFix) undecided(id uuid.UUID, state types.ApprovalState) []types.AuditEvent {
	f.t.Helper()
	if s := f.state(id); s != state {
		f.t.Fatalf("approval state = %s, want %s", s, state)
	}
	if n := len(f.rows("approval.decide")); n != 0 || f.aps.decides != 0 {
		f.t.Fatalf("approval.decide rows = %d, decides = %d; want none", n, f.aps.decides)
	}
	if n := len(f.rows("approval.policy.would_approve")); n != 0 {
		f.t.Fatalf("approval.policy.would_approve rows = %d, want none", n)
	}
	return f.rows("approval.policy.skip")
}

func skipReason(t *testing.T, ev types.AuditEvent) string {
	t.Helper()
	var d struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.Reason
}

// TestApprovalRuleApprovesAMatchingHold: with the switch on, an enforce rule decides a fresh tool_call and a fresh
// egress_domain row of a service identity's run through decide(), as system/policy:<rule_id>, and the
// approval.decide row names the rule, its version, its digest and what matched. The raise answer is unchanged.
func TestApprovalRuleApprovesAMatchingHold(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	for _, tc := range []struct {
		kind         types.ApprovalKind
		scope, key   string
		wantScope    types.ApprovalScope
		matchedValue string
	}{
		{types.ApprovalToolCall, ruleToolScope, "tool", "", "mcp__github__create_pull_request_review"},
		{types.ApprovalEgressDomain, ruleEgressScope, "host", types.ScopeRun, "registry.npmjs.org"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			f := newRuleFix(t)
			w, created := f.raise(tc.kind, tc.scope)
			f.answerIsTheCreatedRow(w, created)
			ap, _ := f.aps.Get(context.Background(), created.ID)
			rule := f.st.rules[0]
			if ap.State != types.ApprovalApproved || ap.DecidedBy != "policy:"+rule.ID.String() || ap.DecisionScope != tc.wantScope {
				t.Fatalf("row = %s by %q scope %q; want APPROVED by policy:%s scope %q", ap.State, ap.DecidedBy, ap.DecisionScope, rule.ID, tc.wantScope)
			}
			rows := f.rows("approval.decide")
			if len(rows) != 1 || rows[0].ActorType != types.ActorSystem || rows[0].Actor != "policy:"+rule.ID.String() {
				t.Fatalf("approval.decide rows = %+v; want one, system/policy:<rule_id>", rows)
			}
			var data struct {
				Reason string                     `json:"reason"`
				Policy *types.ApprovalPolicyMatch `json:"policy"`
			}
			if err := json.Unmarshal(rows[0].Data, &data); err != nil || data.Policy == nil {
				t.Fatalf("approval.decide data %s: %v", rows[0].Data, err)
			}
			p := data.Policy
			if p.RuleID != rule.ID || p.RuleName != "ci-review" || p.RuleVersion != 1 || p.RuleDigest != approvalRuleDigest(rule) ||
				p.Matched["repo"] != "org/repo" || p.Matched[tc.key] != tc.matchedValue || data.Reason != "approval rule ci-review v1" {
				t.Fatalf("data = %s", rows[0].Data)
			}
		})
	}
}

// TestApprovalRuleKindsAreClosed: the allow-list is exactly tool_call and egress_domain, and every kind Wardyn has
// is classified here, so a kind added later is never decidable until someone decides it should be.
func TestApprovalRuleKindsAreClosed(t *testing.T) {
	classified := map[types.ApprovalKind]bool{
		types.ApprovalToolCall: true, types.ApprovalEgressDomain: true,
		types.ApprovalCredential: false, types.ApprovalCredentialReauth: false, types.ApprovalPushContent: false,
		types.ApprovalHumanTakeover: false,
	}
	for _, k := range types.ApprovalKinds {
		want, ok := classified[k]
		if !ok {
			t.Fatalf("approval kind %q is not classified for approval rules", k)
		}
		if policyDecidableKinds[k] != want {
			t.Fatalf("policyDecidableKinds[%q] = %v, want %v", k, policyDecidableKinds[k], want)
		}
	}
	if len(policyDecidableKinds) != 2 {
		t.Fatalf("policyDecidableKinds = %v, want exactly tool_call and egress_domain", policyDecidableKinds)
	}
}

// TestApprovalRuleNeverDecides: a rule that names every kind leaves a credential, credential_reauth, push_content
// or Azure DevOps escalation row PENDING, at the hook and inside decide() alike, with no decision and no
// would-approve.
func TestApprovalRuleNeverDecides(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	grant := uuid.New()
	for name, ap := range map[string]types.ApprovalRequest{
		"credential":        {Kind: types.ApprovalCredential, RequestedScope: json.RawMessage(`{}`)},
		"credential_reauth": {Kind: types.ApprovalCredentialReauth, RequestedScope: json.RawMessage(`{}`)},
		"push_content":      {Kind: types.ApprovalPushContent, RequestedScope: json.RawMessage(`{"paths":["a"]}`)},
		"ado_escalation": {Kind: types.ApprovalToolCall, GrantID: &grant,
			RequestedScope: json.RawMessage(`{"lane":"azure_devops","grant_id":"` + grant.String() + `","capability":"pr_create","tool":"mcp__github__create_pull_request_review"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRuleFix(t, func(f *ruleFix) {
				f.st.rules[0].Kinds = append([]types.ApprovalKind(nil), types.ApprovalKinds...)
			})
			ap.ID, ap.RunID, ap.State, ap.RequestedAt = uuid.New(), f.run.ID, types.ApprovalPending, time.Now()
			f.aps.byID[ap.ID] = ap
			f.srv.runApprovalRules(ap)
			rule := f.st.rules[0]
			d := &policyDecider{rule: rule, m: types.ApprovalPolicyMatch{RuleID: rule.ID, RuleName: rule.Name, RuleVersion: rule.Version}}
			if code, _ := f.srv.policyDecide(ap.ID, d); code == http.StatusOK {
				t.Fatalf("decide() answered 200 for a %s row", name)
			}
			f.pending(ap.ID)
		})
	}
}

// TestApprovalRuleMeetsEveryDecideGate: every gate a person's decision meets binds a rule's. Each case leaves the
// row PENDING with exactly one skip row naming that gate's refusal.
func TestApprovalRuleMeetsEveryDecideGate(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	other := []string{"other.example"}
	cases := []struct {
		name, scope string
		kind        types.ApprovalKind
		mutate      func(*ruleFix)
		env         string
		want        string
		direct      bool                // the run's token no longer raises: drive the hook itself
		state       types.ApprovalState // the row's state after; PENDING unless set
	}{
		{name: "four_eyes", kind: types.ApprovalEgressDomain, scope: ruleEgressScope, env: envEgressSecondHuman,
			want: "second_human_required"},
		{name: "boundary", kind: types.ApprovalEgressDomain, scope: ruleEgressScope, want: reasonBoundaryHost,
			mutate: func(f *ruleFix) {
				f.st.boundary = types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{AllowedDomains: &other}}
			}},
		{name: "run_ended", kind: types.ApprovalToolCall, scope: ruleToolScope, want: reasonApprovalRunEnded, direct: true,
			state:  types.ApprovalCancelled, // decide cancels an ended run's approvals, as it does for a person
			mutate: func(f *ruleFix) { f.run.State = types.RunCompleted }},
		{name: "always_forced", kind: types.ApprovalEgressDomain, scope: ruleEgressScope, want: "security_admin_surface",
			mutate: func(f *ruleFix) { f.st.rules[0].Approve.EgressScope = types.ScopeAlways }},
		{name: "stale_version", kind: types.ApprovalToolCall, scope: ruleToolScope, want: reasonApprovalRuleChanged,
			mutate: func(f *ruleFix) { f.st.bumpOnGet = true }},
		{name: "egress_host_not_granted", kind: types.ApprovalEgressDomain, scope: ruleEgressScope, want: "capability_egress_host",
			mutate: func(f *ruleFix) { f.st.enf = map[string]bool{capEgressHost: true} }},
		{name: "egress_host_type_deny_wins", kind: types.ApprovalEgressDomain, scope: ruleEgressScope, want: "capability_egress_host",
			mutate: func(f *ruleFix) {
				f.st.grants = []types.CapabilityGrant{
					{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "service:ci", Capability: capEgressHost, Value: "registry.npmjs.org", Effect: types.CapabilityAllow},
					{ID: uuid.New(), SubjectType: types.CapabilitySubjectUserType, Subject: "ci-bots", Capability: capEgressHost, Value: "registry.npmjs.org", Effect: types.CapabilityDeny},
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(tc.env, "true")
			}
			var mutate []func(*ruleFix)
			if tc.mutate != nil {
				mutate = append(mutate, tc.mutate)
			}
			f := newRuleFix(t, mutate...)
			var created types.ApprovalRequest
			if tc.direct {
				var err error
				if created, err = f.aps.Request(context.Background(), types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, Kind: tc.kind, RequestedScope: json.RawMessage(tc.scope)}); err != nil {
					t.Fatal(err)
				}
				f.srv.runApprovalRules(created)
			} else {
				w, raised := f.raise(tc.kind, tc.scope)
				f.answerIsTheCreatedRow(w, raised)
				created = raised
			}
			skips := f.undecided(created.ID, cmp.Or(tc.state, types.ApprovalPending))
			if len(skips) != 1 || skipReason(t, skips[0]) != tc.want {
				t.Fatalf("skip rows = %d (%v), want one with reason %s", len(skips), skips, tc.want)
			}
			if skips[0].Actor != "policy:"+f.st.rules[0].ID.String() {
				t.Fatalf("skip actor = %q, want the rule's", skips[0].Actor)
			}
		})
	}
}

// TestApprovalRuleEgressRefusalIsAuditedAsTheRule: a gate refusal inside decide() writes the authz.denied row a
// person's refusal writes, with the rule as its actor.
func TestApprovalRuleEgressRefusalIsAuditedAsTheRule(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t, func(f *ruleFix) { f.st.enf = map[string]bool{capEgressHost: true} })
	_, created := f.raise(types.ApprovalEgressDomain, ruleEgressScope)
	f.pending(created.ID)
	denied := f.rows("authz.denied")
	if len(denied) != 1 || denied[0].ActorType != types.ActorSystem || denied[0].Actor != "policy:"+f.st.rules[0].ID.String() {
		t.Fatalf("authz.denied rows = %+v; want one with actor policy:<rule_id>", denied)
	}
}

// TestApprovalRulesOffReadsNothing: with the switch unset the raise reads no rule and answers byte for byte.
func TestApprovalRulesOffReadsNothing(t *testing.T) {
	t.Setenv(envApprovalRules, "")
	f := newRuleFix(t)
	w, created := f.raise(types.ApprovalToolCall, ruleToolScope)
	f.answerIsTheCreatedRow(w, created)
	f.pending(created.ID)
	if f.st.ruleReads != 0 || len(f.rows("approval.policy.skip")) != 0 {
		t.Fatalf("switch off: %d rule reads, %d skip rows; want none", f.st.ruleReads, len(f.rows("approval.policy.skip")))
	}
}

// TestApprovalRuleDryRunApprovesNothing: a dry_run rule runs decide()'s gates and stops before the one-way line: one
// would_approve row, no decision, and the proxy's poll still reads PENDING.
func TestApprovalRuleDryRunApprovesNothing(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t, func(f *ruleFix) { f.st.rules[0].Mode = types.ApprovalRuleDryRun })
	w, created := f.raise(types.ApprovalEgressDomain, ruleEgressScope)
	f.answerIsTheCreatedRow(w, created)
	ruleDryRunOnly(t, f, created.ID)
}

// TestApprovalRuleEnvDryRunOverridesEnforce: WARDYN_APPROVAL_RULES=dry_run makes an enforce rule a dry run.
func TestApprovalRuleEnvDryRunOverridesEnforce(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesDryRun)
	f := newRuleFix(t)
	_, created := f.raise(types.ApprovalToolCall, ruleToolScope)
	ruleDryRunOnly(t, f, created.ID)
}

func ruleDryRunOnly(t *testing.T, f *ruleFix, id uuid.UUID) {
	t.Helper()
	if s := f.state(id); s != types.ApprovalPending || f.aps.decides != 0 || len(f.rows("approval.decide")) != 0 {
		t.Fatalf("dry run decided: state %s, %d decides, %d approval.decide rows", s, f.aps.decides, len(f.rows("approval.decide")))
	}
	would := f.rows("approval.policy.would_approve")
	if len(would) != 1 || would[0].Actor != "policy:"+f.st.rules[0].ID.String() || !strings.Contains(string(would[0].Data), `"rule_version":1`) {
		t.Fatalf("would_approve rows = %+v; want one naming the rule and its version", would)
	}
	poll := do(t, f.srv, http.MethodGet, "/api/v1/internal/approvals/"+id.String(), f.tok, "")
	if poll.Code != http.StatusOK || !strings.Contains(poll.Body.String(), `"state":"PENDING"`) {
		t.Fatalf("proxy poll = %d %s; want PENDING", poll.Code, poll.Body)
	}
}

// TestApprovalRuleDecideErrorLeavesTheRaiseAlone: decide() failing inside the hook (a store error on the decision)
// leaves the raise answer byte-equal, the row PENDING and exactly one skip row.
func TestApprovalRuleDecideErrorLeavesTheRaiseAlone(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t, func(f *ruleFix) { f.aps.decideErr = errors.New("injected store error") })
	w, created := f.raise(types.ApprovalToolCall, ruleToolScope)
	f.answerIsTheCreatedRow(w, created)
	if skips := f.pending(created.ID); len(skips) != 1 || skipReason(t, skips[0]) != reasonInternalError {
		t.Fatalf("skip rows = %v; want exactly one with reason %s", skips, reasonInternalError)
	}
}

// TestApprovalRuleListErrorIsOneSkip: the hook failing to read the rules writes one skip and decides nothing.
func TestApprovalRuleListErrorIsOneSkip(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t, func(f *ruleFix) { f.st.listErr = errors.New("injected store error") })
	w, created := f.raise(types.ApprovalToolCall, ruleToolScope)
	f.answerIsTheCreatedRow(w, created)
	if skips := f.pending(created.ID); len(skips) != 1 || skipReason(t, skips[0]) != "rules_unreadable" {
		t.Fatalf("skip rows = %v; want one rules_unreadable", skips)
	}
}

// TestApprovalRuleNeverDecidesAnOlderRow: a row raised before the rule existed is not decided when the rule is
// written, nor when the same raise rejoins it.
func TestApprovalRuleNeverDecidesAnOlderRow(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t, func(f *ruleFix) { f.st.rules = nil })
	_, first := f.raise(types.ApprovalToolCall, ruleToolScope)
	f.st.rules = []types.ApprovalRule{testApprovalRule()}
	_, again := f.raise(types.ApprovalToolCall, ruleToolScope)
	if again.ID != first.ID {
		t.Fatalf("the second raise created %s, want the dedup of %s", again.ID, first.ID)
	}
	if skips := f.pending(first.ID); len(skips) != 0 {
		t.Fatalf("a rejoin met a rule: %v", skips)
	}
}

// TestApprovalRuleMatchPredicates: a preset version bump stops the match, as does a human creator, another repo
// or a capability outside capabilities_within; none of them writes a row.
func TestApprovalRuleMatchPredicates(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	preset := func(f *ruleFix) {
		f.st.rules[0].Match = types.ApprovalRuleMatch{Presets: []types.ApprovalRulePreset{{Name: "ci-review", Version: 4}}}
	}
	for name, mutate := range map[string]func(*ruleFix){
		"preset_version_bumped": func(f *ruleFix) { preset(f); f.run.Preset, f.run.PresetVersion = "ci-review", 5 },
		"human_creator":         func(f *ruleFix) { f.run.CreatedBy = "sub-person" },
		"other_repo":            func(f *ruleFix) { f.run.Repo = "org/other" },
		"capability_outside": func(f *ruleFix) {
			f.st.rules[0].Match.CapabilitiesWithin = []string{"contents:read"} // no envelope recorded: no capabilities
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRuleFix(t, mutate)
			_, created := f.raise(types.ApprovalToolCall, ruleToolScope)
			if skips := f.pending(created.ID); len(skips) != 0 {
				t.Fatalf("a non-match wrote skip rows: %v", skips)
			}
		})
	}
	t.Run("preset_at_its_version", func(t *testing.T) {
		f := newRuleFix(t, func(f *ruleFix) { preset(f); f.run.Preset, f.run.PresetVersion = "ci-review", 4 })
		_, created := f.raise(types.ApprovalToolCall, ruleToolScope)
		if s := f.state(created.ID); s != types.ApprovalApproved {
			t.Fatalf("state = %s, want APPROVED", s)
		}
	})
}

// TestPolicyDeciderIsNeverTheAdminToken (C1): a policy context has no human, which every identity function would
// otherwise read as the admin token or an operator.
func TestPolicyDeciderIsNeverTheAdminToken(t *testing.T) {
	t.Setenv(envEgressSecondHuman, "true")
	s := newHarness(t).srv
	id := uuid.New()
	ctx := withPolicyDecider(context.Background(), &policyDecider{m: types.ApprovalPolicyMatch{RuleID: id}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil).WithContext(ctx)
	at, actor := actorFromRequest(r)
	for name, bad := range map[string]bool{
		"isOperator":           s.isOperator(ctx),
		"isSecurityOperator":   s.isSecurityOperator(ctx),
		"principalFromRequest": principalFromRequest(r) != "policy:"+id.String(),
		"actorFromRequest":     at != types.ActorSystem || actor != "policy:"+id.String(),
		"break_glass_compare":  isAdminTokenCaller(r),
		"secondHumanAllows":    s.secondHumanAllows(r, types.ApprovalRequest{Kind: types.ApprovalEgressDomain}, types.AgentRun{CreatedBy: "sub-person"}),
	} {
		if bad {
			t.Errorf("%s reads the policy decider as the admin token or an operator", name)
		}
	}
}

// TestApprovalsDecideHasOneCaller: Approvals.Decide is called only in decide() and in handleDeclineTakeover (a
// takeover request is declined under the takeover entry rule), and decide() only by the approve and deny handlers
// and the approval-rule approver. The test keeps its name.
func TestApprovalsDecideHasOneCaller(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	decideCallers, fsmCallers := map[string]int{}, map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "decide": // Server.decide; capBatch has a decide of its own, so the receiver must be a *Server
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == serverRecv(fn) {
						decideCallers[fn.Name.Name]++
					}
				case "Decide":
					if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "Approvals" {
						fsmCallers[fn.Name.Name]++
					}
				}
				return true
			})
		}
	}
	// handleDeclineTakeover is the one other caller: a takeover request is declined under the takeover
	// entry rule (loadTakeover, human_takeover only, always DENIED) and ends in the same approvalResolved.
	if len(fsmCallers) != 2 || fsmCallers["decide"] != 1 || fsmCallers["handleDeclineTakeover"] != 1 {
		t.Fatalf("Approvals.Decide callers = %v, want decide and handleDeclineTakeover only", fsmCallers)
	}
	want := map[string]int{"handleApproveApproval": 1, "handleDenyApproval": 1, "policyDecide": 1}
	if len(decideCallers) != len(want) {
		t.Fatalf("decide callers = %v, want %v", decideCallers, want)
	}
	for k, v := range want {
		if decideCallers[k] != v {
			t.Fatalf("decide callers = %v, want %v", decideCallers, want)
		}
	}
}

// serverRecv is fn's receiver name when fn is a *Server method, else "".
func serverRecv(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
		return ""
	}
	if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
		if id, ok := star.X.(*ast.Ident); ok && id.Name == "Server" {
			return fn.Recv.List[0].Names[0].Name
		}
	}
	return ""
}
