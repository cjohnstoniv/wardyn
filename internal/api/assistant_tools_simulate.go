// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantReplayBasis is said on every replay answer: one set of proposed rules judged every counted run.
const assistantReplayBasis = "proposed_rules_only"

// assistantAllowedUnder is the host verdict of one evaluator for one attempt, host and port: allowed only when
// it answers allow. Unknown, deny and an error all count as not allowed, and so does an attempt with no port.
func assistantAllowedUnder(ctx context.Context, ev egress.Evaluator) func(host string, port int) bool {
	return func(host string, port int) bool {
		if port <= 0 {
			return false
		}
		v, err := ev.EvaluateHost(ctx, egress.Request{Host: host, Port: port})
		return err == nil && v == egress.VerdictAllow
	}
}

// assistantSimulateSpec is the one policy every counted run is judged by: the draft in place of the target in
// the target's own chain. A unit draft is the unit save's build, composed with its parents
// (assistantUnitBuild). A run or template draft is the run create's composition, resolveRunPolicyFacts, over a
// request whose inline policy is the draft's, as a dry run with no credentials: the person's ceiling, walls and
// the boundary, exactly as the policy preview composes it. ok is false for a kind whose draft sets no host rules.
func (s *Server) assistantSimulateSpec(r *http.Request, sub assistantSubject, caller reviewCaller) (types.RunPolicySpec, bool, *assistantRefusal) {
	draft, ok, err := s.assistantDraftSpec(r.Context(), sub, caller)
	if err != nil {
		return draft, false, assistantCheckFailed(err)
	}
	if !ok || sub.govKind != "" {
		return draft, ok, nil
	}
	req := createRunRequest{InlinePolicy: &draft}
	spec, _, _, _, refusal := s.resolveRunPolicyFacts(r.Context(), r, &req, true, false)
	if refusal != nil {
		return draft, false, &assistantRefusal{custom: func(w http.ResponseWriter, rr *http.Request) bool { return refusal.write(s, w, rr) }}
	}
	return spec, true, nil
}

// assistantReplayScope is the runs a draft's counts read: a unit's runs and its descendants', else the
// person's own. owner is the caller's own principal, never a body field.
func (s *Server) assistantReplayScope(ctx context.Context, sub assistantSubject, owner string) (store.ReplayScope, error) {
	if sub.govKind != govKindUnit {
		return store.ReplayScope{Owner: owner}, nil
	}
	id, err := uuid.Parse(sub.key)
	if err != nil {
		return store.ReplayScope{Owner: owner}, nil
	}
	rows, err := s.unitTreeRows(ctx)
	if err != nil {
		return store.ReplayScope{}, err
	}
	units := []uuid.UUID{id}
	for _, d := range descendantsOf(rows, id) {
		units = append(units, d.ID)
	}
	return store.ReplayScope{Units: units}, nil
}

// assistantReplay is REPLAY's read and its pure counts over the last 30 days of scope, leaving out every run an
// assistant session served, with allowed as the draft's verdict.
func (s *Server) assistantReplay(ctx context.Context, scope store.ReplayScope, allowed func(string, int) bool) (replayResult, *assistantRefusal) {
	rd, ok := s.cfg.Store.(store.RunHostDecisionReader)
	ss, ok2 := s.cfg.Store.(store.AssistantSessionStore)
	if !ok || !ok2 {
		return replayResult{}, assistantRefusalFor(http.StatusNotImplemented, reasonAssistantSessionsUnavailable, "Run history is not available on this server.") // PROPOSED
	}
	now := s.cfg.Now().UTC()
	since, until := now.Add(-assistantHistoryWindow), now.Add(time.Minute)
	exclude, err := ss.AssistantRunIDsCreated(ctx, since, until)
	if err != nil {
		return replayResult{}, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the run history.") // PROPOSED
	}
	rows, totals, err := rd.RunHostDecisions(ctx, scope, since, until, assistantHistoryRuns, exclude)
	if err != nil {
		return replayResult{}, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the run history.") // PROPOSED
	}
	return replayCounts(rows, totals, allowed), nil
}

// assistantDraftReplay is the counts of sub under its own proposed rules, read as the person on r.
func (s *Server) assistantDraftReplay(r *http.Request, sub assistantSubject) (replayResult, bool, *assistantRefusal) {
	ctx := r.Context()
	caller := s.assistantReviewCaller(ctx)
	spec, ok, ref := s.assistantSimulateSpec(r, sub, caller)
	if ref != nil || !ok {
		return replayResult{}, false, ref
	}
	scope, err := s.assistantReplayScope(ctx, sub, oidcHumanFromContext(ctx))
	if err != nil {
		return replayResult{}, false, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the units.") // PROPOSED
	}
	res, ref := s.assistantReplay(ctx, scope, assistantAllowedUnder(ctx, proxy.NewBuiltinEvaluator(spec)))
	return res, ref == nil, ref
}

// assistantSimulateHost is one host of simulate's answer: counts and decisions only.
type assistantSimulateHost struct {
	Host        string `json:"host"`
	Runs        int    `json:"runs"`
	Today       string `json:"today"`
	Under       string `json:"under"`
	RunsChanged int    `json:"runs_changed"`
}

// assistantSimulateResult is simulate's answer: totals and per-host counts, never a run.
type assistantSimulateResult struct {
	Computed      bool                    `json:"computed"`
	RunsInScope   int                     `json:"runs_in_scope"`
	RunsCounted   int                     `json:"runs_counted"`
	Truncated     bool                    `json:"truncated"`
	WouldRefuse   int                     `json:"would_refuse"`
	WouldAllow    int                     `json:"would_allow"`
	NotComparable int                     `json:"not_comparable"`
	Hosts         []assistantSimulateHost `json:"hosts"`
	Basis         string                  `json:"basis"`
}

// assistantSimulateOf is res without its run list, at most assistantHistoryHosts hosts.
func assistantSimulateOf(res replayResult) assistantSimulateResult {
	out := assistantSimulateResult{Computed: true, RunsInScope: res.RunsInScope, RunsCounted: res.RunsCounted, Truncated: res.Truncated,
		WouldRefuse: res.WouldRefuse, WouldAllow: res.WouldAllow, NotComparable: res.NotComparable,
		Hosts: []assistantSimulateHost{}, Basis: assistantReplayBasis}
	for _, h := range res.Hosts {
		if len(out.Hosts) == assistantHistoryHosts {
			break
		}
		out.Hosts = append(out.Hosts, assistantSimulateHost{Host: h.Host, Runs: h.Runs, Today: h.Today, Under: h.Under, RunsChanged: h.RunsChanged})
	}
	return out
}

// assistantToolSimulate is simulate. Its counts come from run history, so it answers only while the
// organisation lets run history reach the provider.
func (s *Server) assistantToolSimulate(e *assistantToolEnv) (any, *assistantRefusal) {
	a, ref := assistantDraftArgsOf(e)
	if ref != nil {
		return nil, ref
	}
	if e.sc.Assistant == nil || !e.sc.Assistant.RunHistoryCounts {
		return nil, assistantRunHistoryOff()
	}
	sub, ref := s.assistantSubjectOf(e, a)
	if ref != nil {
		return nil, ref
	}
	res, ok, ref := s.assistantDraftReplay(e.r, sub)
	if ref != nil {
		return nil, ref
	}
	if !ok {
		return assistantSimulateResult{Hosts: []assistantSimulateHost{}, Basis: assistantReplayBasis}, nil
	}
	return assistantSimulateOf(res), nil
}

// assistantRunHistoryOff refuses a tool whose answer is run history while the organisation keeps it back.
func assistantRunHistoryOff() *assistantRefusal {
	return assistantRefusalFor(http.StatusForbidden, reasonAssistantToolRefused, "Your organisation keeps run history away from the assistant.") // PROPOSED
}
