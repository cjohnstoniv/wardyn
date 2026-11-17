// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runEffectivePolicy is the policy the run's proxy enforces now, and when it was recorded: the
// run.policy.resolve envelope dispatch recorded, with its policy replaced by the current generation's
// once a revive replaced the run's proxy. Every effective-policy reader takes it (the run policy view,
// UI apps, desktop rules, approval-rule capabilities); only the data_out readers tell its two ok=false
// cases apart (runEffectivePolicyGen).
func (s *Server) runEffectivePolicy(ctx context.Context, runID uuid.UUID) (envelope effectivePolicyDatum, at time.Time, ok bool, err error) {
	envelope, at, ok, _, err = s.runEffectivePolicyGen(ctx, runID)
	return envelope, at, ok, err
}

// runEffectivePolicyGen is runEffectivePolicy, and unread besides: the run's current generation when ok is
// false because that generation's row could not be read, else 0. ok is false in two cases the callers must
// not mix up: the run has no run.policy.resolve row (unread is 0; a pruned run), and the run's current
// generation is 2 or later and its run.policy.generation row cannot be read (unread is that number).
// Callers that fail closed on ok=false need not look at it. A caller that would read a missing envelope as
// the permissive default (runDataOutRule) must refuse on unread, since the dispatch envelope's policy is no
// longer the one enforced and its generation may only have tightened it.
//
// The current generation is the one agent_runs says is current (store.RunPolicyGenerations), never the
// newest run.policy.generation row: a revive records its generation before the new proxy starts, and one
// that leaves the old proxy running abandons it, but its row stays. While a revive is still replacing
// the proxy, its generation is already current here, as it is for the run's audit stamps. A revived
// generation's own row is looked up by its number. The row is written best-effort (advanceRunPolicyGeneration
// records it after the store advance and drops a failed write), so it may be missing. A store that keeps no
// generations has none, so the dispatch envelope stands.
//
// Both rows are asked for on their own, so a run's later rows cannot push them out of the read. A store
// without filtered reads (store.Pager; Postgres has them) is read as before, from the run's first
// effectivePolicyAuditScan rows.
func (s *Server) runEffectivePolicyGen(ctx context.Context, runID uuid.UUID) (envelope effectivePolicyDatum, at time.Time, ok bool, unread int, err error) {
	if s.cfg.Store == nil {
		return envelope, at, false, 0, nil
	}
	gen := 1
	if gs, isGen := s.cfg.Store.(store.RunPolicyGenerations); isGen {
		st, _, serr := gs.RunPolicyState(ctx, runID)
		if serr != nil {
			return envelope, at, false, 0, serr
		}
		gen = st.CurrentGeneration()
	}
	resolve, err := s.runPolicyRows(ctx, runID, spellingsOf("run.policy.resolve"), "")
	if err != nil {
		return envelope, at, false, 0, err
	}
	envelope, at, ok = latestPolicyResolve(resolve)
	if !ok || gen <= 1 {
		return envelope, at, ok, 0, nil
	}
	rows, err := s.runPolicyRows(ctx, runID, []string{"run.policy.generation"}, fmt.Sprintf(`{"generation":%d}`, gen))
	if err != nil {
		return effectivePolicyDatum{}, time.Time{}, false, 0, err
	}
	for _, ev := range slices.Backward(rows) {
		var g struct {
			Policy *types.RunPolicySpec `json:"policy"`
		}
		if json.Unmarshal(ev.Data, &g) == nil && g.Policy != nil {
			resolved := envelope.DataOut
			envelope.RunPolicySpec = *g.Policy
			// A generation only tightens data_out (the seal), and an in-place Kubernetes edit records its proxy's policy, which never carries the rule.
			if v := types.StricterTransfer(resolved.DataOut(), g.Policy.DataOut.DataOut()); v != envelope.DataOut.DataOut() {
				envelope.DataOut = v
			}
			return envelope, ev.Time, true, 0, nil
		}
	}
	return effectivePolicyDatum{}, time.Time{}, false, gen, nil
}

// runPolicyRows is the run's newest rows under the first of actions that has any, whose data contains
// dataContains ("" for any), oldest first: filtered reads through store.Pager. A store without them is
// read from the run's first effectivePolicyAuditScan rows, every spelling kept.
func (s *Server) runPolicyRows(ctx context.Context, runID uuid.UUID, actions []string, dataContains string) ([]types.AuditEvent, error) {
	p, ok := s.cfg.Store.(store.Pager)
	if !ok {
		events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, effectivePolicyAuditScan)
		f := store.AuditFilter{DataContains: dataContains}
		var out []types.AuditEvent
		for _, ev := range events {
			if slices.Contains(actions, ev.Action) && f.Matches(ev) {
				out = append(out, ev)
			}
		}
		return out, err
	}
	for _, action := range actions {
		rows, err := p.QueryAuditEventsFilteredPage(ctx, &runID, store.AuditFilter{Action: action, DataContains: dataContains},
			store.Page{Limit: effectivePolicyRows, Newest: true})
		if err != nil || len(rows) > 0 {
			slices.Reverse(rows)
			return rows, err
		}
	}
	return nil, nil
}

// spellingsOf is action and every older name a run's rows may hold it under (audit_legacy.go), newest
// first. Dispatch records a run's policy once, so a run has rows under one of them.
func spellingsOf(action string) []string {
	out := []string{action}
	for old, now := range legacyAuditActions {
		if now == action {
			out = append(out, old)
		}
	}
	return out
}

// effectivePolicyRows bounds one filtered read. Dispatch writes one resolve row per run, and each
// generation number is recorded once, so more than one row is a replayed write.
const effectivePolicyRows = 5
