// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The approval-rule write surface: GET /governance/approval-rules, PUT and DELETE /governance/approval-rules/{name},
// POST /governance/approval-rules/dry-run, on the security tier (routes.go): the tier that decides tool calls. The
// routes work with WARDYN_APPROVAL_RULES off, so rules can be staged. A PUT is a covered governance write and never
// exempt from four-eyes, an edit included, since one edit can change the mode and widen hosts or tools together. A
// DELETE is the only exempt write: it can only narrow.

const (
	govKindApprovalRule      = types.GovernanceTargetApprovalRule
	approvalRuleWriteAction  = "governance.approval_rule.write"
	approvalRuleDeleteAction = "governance.approval_rule.delete"

	// maxApprovalRuleEntries bounds each list in a rule; a rule is read on every matching raise.
	maxApprovalRuleEntries = 256
	// approvalRuleHistory and approvalRuleHistoryScan bound the draft dry run's predicate-only look back.
	approvalRuleHistory     = 7 * 24 * time.Hour
	approvalRuleHistoryScan = 1000
)

// approvalRuleNameRE is migration 0202's CHECK. "dry-run" is refused too: it is the dry-run route's own segment.
var approvalRuleNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// validateApprovalRule refuses a rule the approver cannot hold, and normalises what it can: an omitted mode is
// dry_run, repos are case-folded.
func validateApprovalRule(w *types.ApprovalRuleWrite) error {
	if !approvalRuleNameRE.MatchString(w.Name) || w.Name == "dry-run" {
		return errors.New("name must be 1 to 63 lower-case letters, digits or hyphens, starting with a letter or digit, and not dry-run")
	}
	if w.Mode == "" {
		w.Mode = types.ApprovalRuleDryRun
	}
	if w.Mode != types.ApprovalRuleDryRun && w.Mode != types.ApprovalRuleEnforce {
		return errors.New("mode must be dry_run or enforce")
	}
	m, a := &w.Match, &w.Approve
	for _, n := range []int{len(w.Kinds), len(m.Repos), len(m.Presets), len(m.CapabilitiesWithin), len(a.Tools), len(a.Hosts)} {
		if n > maxApprovalRuleEntries {
			return fmt.Errorf("a rule's lists hold at most %d entries each", maxApprovalRuleEntries)
		}
	}
	if len(w.Kinds) == 0 {
		return errors.New("kinds is required: tool_call, egress_domain or both")
	}
	for i, k := range w.Kinds {
		if !policyDecidableKinds[k] || slices.Contains(w.Kinds[:i], k) {
			return fmt.Errorf("kinds: %q is not tool_call or egress_domain, or is named twice; an approval rule never decides any other kind", k)
		}
	}
	if len(m.Repos) == 0 && len(m.Presets) == 0 {
		return errors.New("match names no repo and no preset: a rule must name at least one")
	}
	for i, r := range m.Repos {
		if m.Repos[i] = strings.ToLower(strings.TrimSpace(r)); m.Repos[i] == "" {
			return errors.New("match.repos: an empty repo matches nothing")
		}
	}
	for _, p := range m.Presets {
		if strings.TrimSpace(p.Name) == "" || p.Version < 1 {
			return errors.New("match.presets: each preset needs a name and a version of 1 or more")
		}
	}
	if slices.Contains(m.CapabilitiesWithin, "") {
		return errors.New("match.capabilities_within: an empty capability matches nothing")
	}
	if err := validateApprovalRuleApprove(w.Kinds, *a); err != nil {
		return err
	}
	return nil
}

func validateApprovalRuleApprove(kinds []types.ApprovalKind, a types.ApprovalRuleApprove) error {
	tools, egress := slices.Contains(kinds, types.ApprovalToolCall), slices.Contains(kinds, types.ApprovalEgressDomain)
	if tools != (len(a.Tools) > 0) {
		return errors.New("approve.tools is required with kind tool_call, and refused without it")
	}
	if egress != (len(a.Hosts) > 0) {
		return errors.New("approve.hosts is required with kind egress_domain, and refused without it")
	}
	for i, t := range a.Tools {
		if t == "" || t != strings.TrimSpace(t) || len(t) > maxToolRuleNameLen || strings.ContainsAny(t, "*?[") || slices.Contains(a.Tools[:i], t) {
			return fmt.Errorf("approve.tools: %q is not an exact tool name (no glob, no *, no whitespace, named once)", t)
		}
	}
	for i, h := range a.Hosts {
		if !hostrules.ValidApprovedHost(h) || slices.Contains(a.Hosts[:i], h) {
			return fmt.Errorf("approve.hosts: %q is not an exact lower-case hostname (no wildcard, port or path, named once)", h)
		}
	}
	switch a.EgressScope {
	case "", types.ScopeOnce, types.ScopeRun:
	default:
		return errors.New("approve.egress_scope must be once or run: an approval rule never decides until or always")
	}
	if a.EgressScope != "" && !egress {
		return errors.New("approve.egress_scope is only valid with kind egress_domain")
	}
	return nil
}

// approvalRuleBoundaryRefusal refuses a host the Wardyn boundary (read by boundary: the pool on a direct write, the
// decision transaction on an approved one) does not allow while it is in hard_limits.
func approvalRuleBoundaryRefusal(boundary func() (types.Boundary, error), w types.ApprovalRuleWrite) (*unitWriteError, error) {
	b, err := boundary()
	if err != nil || !b.HardLimits() {
		return nil, err
	}
	for _, h := range w.Approve.Hosts {
		if !boundaryAdmitsHost(b, h) {
			return writeRefusal(http.StatusConflict, reasonApprovalRuleHost,
				"the Wardyn boundary does not allow %s, so no approval rule can approve it", h), nil
		}
	}
	return nil, nil
}

func (s *Server) approvalRuleStore(w http.ResponseWriter) (store.ApprovalRuleStore, bool) {
	rs, ok := s.cfg.Store.(store.ApprovalRuleStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonApprovalRuleInvalid, "this store keeps no approval rules")
	}
	return rs, ok
}

// approvalRuleState is the part of a rule a write replaces and an approval compares; nil when there is none.
func approvalRuleState(r types.ApprovalRule, found bool) *types.ApprovalRuleWrite {
	if !found {
		return nil
	}
	return &types.ApprovalRuleWrite{Name: r.Name, Mode: r.Mode, ApprovalRuleSpec: r.ApprovalRuleSpec}
}

// currentApprovalRule reads the rule a write replaces; found=false is a create.
func currentApprovalRule(ctx context.Context, q store.Querier, name string) (types.ApprovalRule, bool, error) {
	cur, err := store.GetApprovalRuleQ(ctx, q, name)
	if errors.Is(err, store.ErrNotFound) {
		return cur, false, nil
	}
	return cur, err == nil, err
}

// approvalRuleAuditData is the full text and version of a write, so every version a rule had is on record.
func approvalRuleAuditData(saved types.ApprovalRule, was int) map[string]any {
	return map[string]any{"name": saved.Name, "rule_id": saved.ID, "version": saved.Version, "was_version": was,
		"mode": saved.Mode, "spec": saved.ApprovalRuleSpec, "digest": approvalRuleDigest(saved)}
}

func (s *Server) handleListApprovalRules(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.approvalRuleStore(w)
	if !ok {
		return
	}
	rules, err := rs.ListApprovalRules(r.Context())
	if err != nil {
		writeServerError(w, r, "list approval rules", err)
		return
	}
	if rules == nil {
		rules = []types.ApprovalRule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approval_rules": rules, "switch": approvalRulesSwitch()})
}

// handlePutApprovalRule creates or replaces the rule named in the path, at its version plus one.
func (s *Server) handlePutApprovalRule(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.approvalRuleStore(w)
	if !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var body types.ApprovalRuleWrite
	if !decodeStrict(w, r, &body) {
		return
	}
	name := chi.URLParam(r, "name")
	if body.Name != "" && body.Name != name {
		writeErrorReason(w, http.StatusBadRequest, reasonApprovalRuleInvalid, "invalid approval rule: the body names another rule than the path")
		return
	}
	body.Name = name
	if err := validateApprovalRule(&body); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonApprovalRuleInvalid, "invalid approval rule: "+err.Error())
		return
	}
	if refusal, err := approvalRuleBoundaryRefusal(func() (types.Boundary, error) { return s.governanceBoundary(r.Context()) }, body); err != nil || refusal != nil {
		if err != nil {
			writeServerError(w, r, "read the Wardyn boundary", err)
		} else {
			writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
		}
		return
	}
	cur, err := rs.GetApprovalRule(r.Context(), name)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read approval rule", err)
		return
	}
	before := approvalRuleState(cur, found)
	if mode == govQueue {
		var beforeView any
		if before != nil {
			beforeView = before
		}
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindApprovalRule, op: "replace", key: name, payload: body,
			before: beforeView, after: body, changed: changedPaths(beforeView, body), baseState: before,
		})
		return
	}
	saved, err := rs.PutApprovalRule(r.Context(), body, principalFromRequest(r))
	if err != nil {
		writeServerError(w, r, "write approval rule", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		approvalRuleWriteAction, name, "success", mustJSON(approvalRuleAuditData(saved, cur.Version))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindApprovalRule, name, "success", nil)
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleDeleteApprovalRule removes a rule. Exempt from four-eyes: a rule only ever approves, so removing one narrows.
func (s *Server) handleDeleteApprovalRule(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.approvalRuleStore(w)
	if !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	removed, err := rs.DeleteApprovalRule(r.Context(), name)
	if err != nil {
		if notFoundIf(w, err, "approval rule", reasonApprovalRuleNotFound) {
			return
		}
		writeServerError(w, r, "delete approval rule", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		approvalRuleDeleteAction, name, "success", mustJSON(approvalRuleAuditData(removed, removed.Version))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindApprovalRule, name, "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyApprovalRuleChange applies a held PUT inside the decision transaction: the target lock, the rule as it now
// stands compared with what the proposal reviewed, then the write on q.
func applyApprovalRuleChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var held types.ApprovalRuleWrite
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := validateApprovalRule(&held); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonApprovalRuleInvalid, "invalid approval rule: %s", err.Error())
	}
	if refusal, err := approvalRuleBoundaryRefusal(func() (types.Boundary, error) { return store.GetBoundaryQ(ctx, q) }, held); err != nil || refusal != nil {
		if err != nil {
			return govApplied{}, err
		}
		return govApplied{}, refusal
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindApprovalRule, held.Name); err != nil {
		return govApplied{}, err
	}
	cur, found, err := currentApprovalRule(ctx, q, held.Name)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(approvalRuleState(cur, found)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	saved, err := store.PutApprovalRuleQ(ctx, q, held, principalFromRequest(r))
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: approvalRuleWriteAction, target: held.Name, data: approvalRuleAuditData(saved, cur.Version)}, nil
}

// approvalRuleReportRow is one approval the drafted rule matches. Outcome is would_approve or skip (with the gate's
// reason) for a PENDING row, which ran the dry-run decide(); predicate_match for a recent row, which only matched.
type approvalRuleReportRow struct {
	ApprovalID  uuid.UUID           `json:"approval_id"`
	RunID       uuid.UUID           `json:"run_id"`
	Kind        types.ApprovalKind  `json:"kind"`
	State       types.ApprovalState `json:"state"`
	RequestedAt time.Time           `json:"requested_at"`
	Outcome     string              `json:"outcome"`
	Reason      string              `json:"reason,omitempty"`
	Matched     map[string]string   `json:"matched"`
}

type approvalRuleReport struct {
	Rule    types.ApprovalRuleWrite `json:"rule"`
	Pending []approvalRuleReportRow `json:"pending"`
	Recent  []approvalRuleReportRow `json:"recent"`
	// RecentTruncated: the look back stopped at approvalRuleHistoryScan rows before it reached seven days.
	RecentTruncated bool `json:"recent_truncated"`
}

// handleApprovalRuleDryRun reports what a drafted rule (the body), or a stored one (a body naming only a rule),
// would do: every PENDING row through the same dry-run decide() the live switch runs, and the rows requested in the
// last seven days by predicate alone. It writes nothing: the report decider makes recordAudit drop every row.
func (s *Server) handleApprovalRuleDryRun(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.approvalRuleStore(w)
	if !ok {
		return
	}
	var body types.ApprovalRuleWrite
	if !decodeStrict(w, r, &body) {
		return
	}
	rule := types.ApprovalRule{ID: uuid.New(), Name: cmp.Or(body.Name, "draft"), Version: 1}
	if len(body.Kinds) == 0 && body.Name != "" {
		stored, err := rs.GetApprovalRule(r.Context(), body.Name)
		if err != nil {
			if notFoundIf(w, err, "approval rule", reasonApprovalRuleNotFound) {
				return
			}
			writeServerError(w, r, "read approval rule", err)
			return
		}
		rule = stored
	} else {
		body.Name = rule.Name
		if err := validateApprovalRule(&body); err != nil {
			writeErrorReason(w, http.StatusBadRequest, reasonApprovalRuleInvalid, "invalid approval rule: "+err.Error())
			return
		}
		rule.Mode, rule.ApprovalRuleSpec = body.Mode, body.ApprovalRuleSpec
	}
	report, err := s.approvalRuleDryRun(r.Context(), rule)
	if err != nil {
		writeServerError(w, r, "approval rule dry run", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) approvalRuleDryRun(ctx context.Context, rule types.ApprovalRule) (approvalRuleReport, error) {
	report := approvalRuleReport{Rule: types.ApprovalRuleWrite{Name: rule.Name, Mode: rule.Mode, ApprovalRuleSpec: rule.ApprovalRuleSpec},
		Pending: []approvalRuleReportRow{}, Recent: []approvalRuleReportRow{}}
	runs := map[uuid.UUID]*types.AgentRun{}
	match := func(ap types.ApprovalRequest) (types.ApprovalPolicyMatch, bool, error) {
		run, seen := runs[ap.RunID]
		if !seen {
			if got, err := s.cfg.Store.GetRun(ctx, ap.RunID); err == nil {
				run = &got
			} else if !errors.Is(err, store.ErrNotFound) {
				return types.ApprovalPolicyMatch{}, false, err
			}
			runs[ap.RunID] = run
		}
		if run == nil || !isServicePrincipal(run.CreatedBy) {
			return types.ApprovalPolicyMatch{}, false, nil
		}
		return s.matchApprovalRule(ctx, rule, ap, *run)
	}
	row := func(ap types.ApprovalRequest, m types.ApprovalPolicyMatch, outcome, reason string) approvalRuleReportRow {
		return approvalRuleReportRow{ApprovalID: ap.ID, RunID: ap.RunID, Kind: ap.Kind, State: ap.State,
			RequestedAt: ap.RequestedAt, Outcome: outcome, Reason: reason, Matched: m.Matched}
	}
	pending, err := s.cfg.Approvals.List(ctx, types.ApprovalPending)
	if err != nil {
		return report, err
	}
	for _, ap := range pending {
		m, ok, err := match(ap)
		if err != nil {
			return report, err
		}
		if !ok {
			continue
		}
		if isTerminalRunState(runs[ap.RunID].State) {
			// decide() would cancel an ended run's approvals (refuseIfRunEnded), a write the report drop does not
			// cover: answer its refusal here instead.
			report.Pending = append(report.Pending, row(ap, m, "skip", reasonApprovalRunEnded))
			continue
		}
		code, reason := s.policyDecide(ap.ID, &policyDecider{rule: rule, m: m, dryRun: true, report: true})
		if code == http.StatusOK {
			report.Pending = append(report.Pending, row(ap, m, "would_approve", ""))
		} else {
			report.Pending = append(report.Pending, row(ap, m, "skip", reason))
		}
	}
	pager, ok := s.cfg.Store.(store.Pager)
	if !ok {
		return report, nil
	}
	cutoff := s.cfg.Now().Add(-approvalRuleHistory)
	for scanned := 0; ; {
		page, err := pager.ListApprovalsPage(ctx, "", store.Page{Limit: 200, Offset: scanned})
		if err != nil {
			return report, err
		}
		for _, ap := range page {
			if ap.RequestedAt.Before(cutoff) {
				return report, nil
			}
			if scanned++; scanned > approvalRuleHistoryScan {
				report.RecentTruncated = true
				return report, nil
			}
			if ap.State == types.ApprovalPending {
				continue
			}
			m, ok, err := match(ap)
			if err != nil {
				return report, err
			}
			if ok {
				report.Recent = append(report.Recent, row(ap, m, "predicate_match", ""))
			}
		}
		if len(page) < 200 {
			return report, nil
		}
	}
}
