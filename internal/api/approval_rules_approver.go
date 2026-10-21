// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Policy as approver: an approval rule (approval_rules.go) approves a fresh tool_call or egress_domain row a
// service identity's run raises, by calling decide() as its own principal, policy:<rule_id>. Every gate a person's
// decision meets binds unchanged; this file only adds who the decider is and what it may reach.

// envApprovalRules switches the approver: off (default, unset), dry_run (every rule acts as dry_run) or on. Env-only,
// like the four-eyes switches: a switch that widens reach needs deploy access, not a console click.
const envApprovalRules = "WARDYN_APPROVAL_RULES"

const (
	approvalRulesOff    = "off"
	approvalRulesDryRun = "dry_run"
	approvalRulesOn     = "on"
)

// approvalRulesActor is the actor of a skip row written before any rule matched.
const approvalRulesActor = "wardyn/approval-rules"

// validateApprovalRulesSwitch is the boot check (ValidateEnvToggles): exactly off, dry_run or on, or unset.
func validateApprovalRulesSwitch() error {
	switch v := os.Getenv(envApprovalRules); v {
	case "", approvalRulesOff, approvalRulesDryRun, approvalRulesOn:
		return nil
	default:
		return fmt.Errorf("invalid %s=%q: want off, dry_run or on", envApprovalRules, v)
	}
}

// approvalRulesSwitch is the switch as a request reads it. Boot refused any other value, so one read here as off.
func approvalRulesSwitch() string {
	if validateApprovalRulesSwitch() != nil || os.Getenv(envApprovalRules) == "" {
		return approvalRulesOff
	}
	return os.Getenv(envApprovalRules)
}

// policyDecidableKinds is the positive allow-list of kinds a rule may decide. A kind added to types.ApprovalKinds
// later is never decidable until it is listed here.
var policyDecidableKinds = map[types.ApprovalKind]bool{
	types.ApprovalToolCall:     true,
	types.ApprovalEgressDomain: true,
}

// policyDecider is the decider on a policy context: the rule as matched, and whether this decide stops at the
// dry-run line. report is the draft dry run (POST /governance/approval-rules/dry-run), which writes no row at all.
type policyDecider struct {
	rule   types.ApprovalRule
	m      types.ApprovalPolicyMatch
	dryRun bool
	report bool
}

type policyDeciderKey struct{}

func withPolicyDecider(ctx context.Context, d *policyDecider) context.Context {
	return context.WithValue(ctx, policyDeciderKey{}, d)
}

func policyDeciderFrom(ctx context.Context) (*policyDecider, bool) {
	d, ok := ctx.Value(policyDeciderKey{}).(*policyDecider)
	return d, ok && d != nil
}

// policyMatchFrom is the match an approval.decide row carries, nil for any other decider.
func policyMatchFrom(ctx context.Context) *types.ApprovalPolicyMatch {
	if d, ok := policyDeciderFrom(ctx); ok {
		m := d.m
		return &m
	}
	return nil
}

// policyActor is the principal a rule decides as. The id is the rule's UUID, so a rename keeps the actor.
func policyActor(d *policyDecider) string { return "policy:" + d.m.RuleID.String() }

// runApprovalRules is the raise hook. Only a row this raise created reaches it (never a dedup or a rejoin), and
// only with the switch on: off reads no rule at all. The first rule by name that matches decides; the raise answer
// is not changed, the proxy and toolgate see the decision on their next poll.
func (s *Server) runApprovalRules(created types.ApprovalRequest) {
	sw := approvalRulesSwitch()
	if sw == approvalRulesOff || !policyDecidableKinds[created.Kind] || created.GrantID != nil {
		return
	}
	rs, ok := s.cfg.Store.(store.ApprovalRuleStore)
	if !ok {
		return
	}
	ctx := context.Background()
	run, err := s.cfg.Store.GetRun(ctx, created.RunID)
	if err != nil {
		s.recordPolicySkip(ctx, created, nil, "run_unreadable")
		return
	}
	if !isServicePrincipal(run.CreatedBy) {
		return
	}
	rules, err := rs.ListApprovalRules(ctx)
	if err != nil {
		s.recordPolicySkip(ctx, created, nil, "rules_unreadable")
		return
	}
	for _, rule := range rules {
		m, ok, err := s.matchApprovalRule(ctx, rule, created, run)
		if err != nil {
			s.recordPolicySkip(ctx, created, nil, "run_policy_unreadable")
			return
		}
		if !ok {
			continue
		}
		d := &policyDecider{rule: rule, m: m, dryRun: sw == approvalRulesDryRun || rule.Mode != types.ApprovalRuleEnforce}
		if code, reason := s.policyDecide(created.ID, d); code != http.StatusOK {
			s.recordPolicySkip(ctx, created, d, reason)
		}
		return
	}
}

// policyDecide calls decide() as the rule, on a fresh context that carries no sidecar or human identity, and
// returns the status and wire reason it answered.
func (s *Server) policyDecide(id uuid.UUID, d *policyDecider) (int, string) {
	var scope types.ApprovalScope // decide takes a scope on an egress_domain approval only
	if _, isHost := d.m.Matched["host"]; isHost {
		scope = d.rule.Approve.EgressScope
	}
	body := mustJSON(map[string]any{
		"decision_scope": scope,
		"reason":         fmt.Sprintf("approval rule %s v%d", d.rule.Name, d.rule.Version),
	})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	ctx := context.WithValue(withPolicyDecider(context.Background(), d), chi.RouteCtxKey, rctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, "request_build_failed"
	}
	sink := &policySink{header: http.Header{}}
	s.decide(sink, req, true)
	return sink.status(), sink.reason()
}

// policySink is the ResponseWriter decide() answers the approver on: it keeps the status and the wire reason.
type policySink struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (p *policySink) Header() http.Header { return p.header }

func (p *policySink) WriteHeader(code int) {
	if p.code == 0 {
		p.code = code
	}
}

func (p *policySink) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	if p.body.Len() > maxDecisionBody {
		return len(b), nil
	}
	return p.body.Write(b)
}

func (p *policySink) status() int {
	if p.code == 0 {
		return http.StatusOK
	}
	return p.code
}

func (p *policySink) reason() string {
	var e struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(p.body.Bytes(), &e) != nil || e.Reason == "" {
		return fmt.Sprintf("status_%d", p.status())
	}
	return e.Reason
}

// recordPolicySkip writes approval.policy.skip: a rule matched (d) or the hook could not tell (d nil), and the row
// stays PENDING for a person.
func (s *Server) recordPolicySkip(ctx context.Context, ap types.ApprovalRequest, d *policyDecider, reason string) {
	actor, data := approvalRulesActor, map[string]any{"approval_id": ap.ID, "kind": ap.Kind, "reason": reason}
	if d != nil {
		actor, data["policy"] = policyActor(d), d.m
	}
	s.recordAudit(withPolicyReport(ctx, d), s.auditEvent(&ap.RunID, types.ActorSystem, actor, "approval.policy.skip", ap.ID.String(), "failure", mustJSON(data)))
}

// withPolicyReport keeps a draft dry run's context on a row written outside decide(), so recordAudit drops it too.
func withPolicyReport(ctx context.Context, d *policyDecider) context.Context {
	if d != nil && d.report {
		return withPolicyDecider(ctx, d)
	}
	return ctx
}

// policyDryRunStop is decide()'s one-way line for a dry-run decider: the gates above it all ran, nothing below it
// runs. It writes approval.policy.would_approve and answers 200 without deciding.
func (s *Server) policyDryRunStop(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, scope types.ApprovalScope) bool {
	d, ok := policyDeciderFrom(r.Context())
	if !ok || !d.dryRun {
		return false
	}
	s.recordAudit(r.Context(), s.auditEvent(&ap.RunID, types.ActorSystem, policyActor(d), "approval.policy.would_approve", ap.ID.String(), "success",
		mustJSON(map[string]any{"approval_id": ap.ID, "kind": ap.Kind, "decision_scope": scope, "policy": d.m})))
	writeJSON(w, http.StatusOK, map[string]bool{"would_approve": true})
	return true
}

// authorizePolicyDecision is authorizeUserDecision's arm for a rule: the kind allow-list, never an Azure DevOps
// escalation (any tool_call with a grant id), a service identity's run, the rule re-read at the version it matched,
// its predicates re-run, then the reach bound of the kind. It loads both rows.
func (s *Server) authorizePolicyDecision(w http.ResponseWriter, r *http.Request, id uuid.UUID, d *policyDecider) (types.ApprovalRequest, types.AgentRun, bool, bool) {
	ctx := r.Context()
	var run types.AgentRun
	ap, err := s.cfg.Approvals.Get(ctx, id)
	if err != nil {
		writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
		return ap, run, false, false
	}
	if !policyDecidableKinds[ap.Kind] || ap.GrantID != nil {
		writeErrorReason(w, http.StatusForbidden, reasonApprovalRuleNotApplicable, "an approval rule never decides this approval")
		return ap, run, false, false
	}
	if run, err = s.cfg.Store.GetRun(ctx, ap.RunID); err != nil {
		writeServerError(w, r, "get run", err)
		return ap, run, false, false
	}
	if !isServicePrincipal(run.CreatedBy) {
		writeErrorReason(w, http.StatusForbidden, reasonApprovalRuleNotApplicable, "an approval rule decides only a service identity's runs")
		return ap, run, false, false
	}
	cur, err := s.rereadApprovalRule(ctx, d)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonApprovalRuleNotFound, "approval rule not found")
		return ap, run, false, false
	case err != nil:
		writeServerError(w, r, "read approval rule", err)
		return ap, run, false, false
	case cur.ID != d.m.RuleID || cur.Version != d.m.RuleVersion:
		writeErrorReason(w, http.StatusConflict, reasonApprovalRuleChanged, "the approval rule changed after it matched")
		return ap, run, false, false
	}
	if _, matched, err := s.matchApprovalRule(ctx, cur, ap, run); err != nil {
		writeServerError(w, r, "match approval rule", err)
		return ap, run, false, false
	} else if !matched {
		writeErrorReason(w, http.StatusForbidden, reasonApprovalRuleNotApplicable, "the approval rule no longer matches this approval")
		return ap, run, false, false
	}
	if ap.Kind == types.ApprovalEgressDomain {
		host := approvalHost(ap)
		allowed, err := s.serviceEgressHostAllowed(ctx, run, host)
		if err != nil {
			writeServerError(w, r, "resolve capability", err)
			return ap, run, false, false
		}
		if !allowed {
			s.refuse(w, r, authz.Deny(authz.ReasonCapabilityEgressHost, id.String(), "the service identity that created this run is not granted egress host "+host).
				OnRun(ap.RunID).With("host", host))
			return ap, run, false, false
		}
	}
	return ap, run, true, true
}

// rereadApprovalRule is the rule as it stands now. The draft dry run judges the rule it was handed, which no store
// holds yet.
func (s *Server) rereadApprovalRule(ctx context.Context, d *policyDecider) (types.ApprovalRule, error) {
	if d.report {
		return d.rule, nil
	}
	rs, ok := s.cfg.Store.(store.ApprovalRuleStore)
	if !ok {
		return types.ApprovalRule{}, store.ErrNotFound
	}
	return rs.GetApprovalRule(ctx, d.rule.Name)
}

// serviceEgressHostAllowed is the member rule a person deciding their own run meets (authorizeUserDecision's
// egress_host gate), asked at the service's own subjects: its principal and user type, no groups. It is
// capBatch.decide over the named-subject batch capExplain uses, never a second rule order.
func (s *Server) serviceEgressHostAllowed(ctx context.Context, run types.AgentRun, host string) (bool, error) {
	if host == "" {
		return false, nil
	}
	principal := types.CanonicalUserSubject(run.CreatedBy)
	si, err := s.serviceRecord(ctx, strings.TrimPrefix(principal, types.ServicePrincipalPrefix))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !si.Usable() {
		return false, nil
	}
	subj := callerSubjects{users: []string{principal}, userType: si.UserType}
	grants, err := s.cfg.Store.ListCapabilityGrantsFor(ctx, subj.users, nil, subj.userType)
	if err != nil {
		return false, err
	}
	enf, err := s.cfg.Store.GetCapabilityEnforcement(ctx)
	if err != nil {
		return false, err
	}
	restricted, err := s.cfg.Store.ListCapabilityRestrictions(ctx)
	if err != nil {
		return false, err
	}
	return explainBatch(s, subj, grants, enf, restricted).allowed(ctx, capEgressHost, host)
}

// matchApprovalRule evaluates rule against an approval and its run and freezes what matched. AND across fields, OR
// within one; a rule naming neither repos nor presets matches nothing.
func (s *Server) matchApprovalRule(ctx context.Context, rule types.ApprovalRule, ap types.ApprovalRequest, run types.AgentRun) (types.ApprovalPolicyMatch, bool, error) {
	var none types.ApprovalPolicyMatch
	if !policyDecidableKinds[ap.Kind] || ap.GrantID != nil || !slices.Contains(rule.Kinds, ap.Kind) {
		return none, false, nil
	}
	matched := map[string]string{}
	if ap.Kind == types.ApprovalEgressDomain {
		host := approvalHost(ap)
		if host == "" || !slices.Contains(rule.Approve.Hosts, host) {
			return none, false, nil
		}
		matched["host"] = host
	} else {
		tool := approvalTool(ap)
		if tool == "" || !slices.Contains(rule.Approve.Tools, tool) {
			return none, false, nil
		}
		matched["tool"] = tool
	}
	mt := rule.Match
	if len(mt.Repos) == 0 && len(mt.Presets) == 0 {
		return none, false, nil
	}
	if len(mt.Repos) > 0 {
		if run.Repo == "" || !slices.ContainsFunc(mt.Repos, func(r string) bool { return strings.EqualFold(r, run.Repo) }) {
			return none, false, nil
		}
		matched["repo"] = run.Repo
	}
	if len(mt.Presets) > 0 {
		if run.Preset == "" || !slices.Contains(mt.Presets, types.ApprovalRulePreset{Name: run.Preset, Version: run.PresetVersion}) {
			return none, false, nil
		}
		matched["preset"] = fmt.Sprintf("%s@%d", run.Preset, run.PresetVersion)
	}
	if len(mt.CapabilitiesWithin) > 0 {
		caps, err := s.runScopeCapabilities(ctx, run)
		if err != nil {
			return none, false, err
		}
		if len(caps) == 0 || slices.ContainsFunc(caps, func(c string) bool { return !slices.Contains(mt.CapabilitiesWithin, c) }) {
			return none, false, nil
		}
		matched["capabilities"] = strings.Join(caps, ",")
	}
	return types.ApprovalPolicyMatch{RuleID: rule.ID, RuleName: rule.Name, RuleVersion: rule.Version,
		RuleDigest: approvalRuleDigest(rule), Matched: matched}, true, nil
}

// runScopeCapabilities is the run's GitHub and Azure DevOps capability lists from the envelope dispatch recorded (the
// reader beneath handleGetRunPolicy); a run with no envelope yet has none, so capabilities_within cannot match it.
func (s *Server) runScopeCapabilities(ctx context.Context, run types.AgentRun) ([]string, error) {
	events, err := s.cfg.Store.QueryAuditEvents(ctx, run.ID, effectivePolicyAuditScan)
	if err != nil {
		return nil, err
	}
	env, _, found := latestPolicyResolve(events)
	if !found {
		return nil, nil
	}
	var caps []string
	for _, c := range env.GitHubCapabilities {
		caps = append(caps, string(c))
	}
	for _, c := range env.AzureDevOpsCapabilities {
		caps = append(caps, string(c))
	}
	return caps, nil
}

// approvalTool is a tool_call row's tool name, "" when it names none (a command-only hold matches no rule).
func approvalTool(ap types.ApprovalRequest) string {
	var scope struct {
		Tool string `json:"tool"`
	}
	if json.Unmarshal(ap.RequestedScope, &scope) != nil {
		return ""
	}
	return scope.Tool
}

// approvalRuleDigest names the exact text a decision matched: the mode and spec of that version.
func approvalRuleDigest(rule types.ApprovalRule) string {
	sum := sha256.Sum256(mustJSON(map[string]any{"mode": rule.Mode, "spec": rule.ApprovalRuleSpec}))
	return hex.EncodeToString(sum[:])
}
