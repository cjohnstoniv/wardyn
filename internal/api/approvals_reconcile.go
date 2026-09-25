// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ReconcileWorkspaceEgressDecisions re-applies decided `always` egress verdicts
// to the run's primary workspace (persistWorkspaceEgressDecision is not atomic
// with Decide). AddWorkspaceEgressDecision is an idempotent upsert that also
// clears the mirror list. Returns the count re-applied. It runs once
// at boot (cmd/wardynd). ponytail: boot-only heals on the next restart; a periodic
// tick would heal sooner on a laptop that rarely reboots — add one if that window
// proves too wide.
// The operator's newest action must win: per (workspace, host) only the latest
// DecidedAt is kept, and a decision older than Workspace.EgressEditedAt (stamped
// by the egress-list PUTs) is skipped — an undone `always` still reads APPROVED,
// so a per-approval "applied" marker could not tell. Decided rows are never
// deleted: cap this with an incremental scan if the table grows large.
func (s *Server) ReconcileWorkspaceEgressDecisions(ctx context.Context) (int, error) {
	if s.cfg.Store == nil || s.cfg.Approvals == nil {
		return 0, nil
	}
	decisions, err := s.egressDecisionsToReconcile(ctx)
	if err != nil {
		return 0, err
	}
	reconciled := 0
	for _, d := range decisions {
		// Re-run the live path's direction-specific rejects against the CURRENT
		// workspace row: alwaysEgressDecision checks only the predicates that need the
		// approval. The heal replays a verdict recorded at t0 against the workspace as it
		// is at boot, and the two can diverge (`egress:<host>` marked REQUIRED after an
		// older deny-always, which the live API answers 400 for). EgressEditedAt does not
		// cover this: the requirements PUT does not stamp it. Skipped and audited, not
		// silently: "why did this host not get onto this list" needs an answer.
		ws, werr := s.cfg.Store.GetWorkspace(ctx, d.workspace)
		if werr != nil {
			continue // workspace gone; nothing to persist onto (as before)
		}
		if reason := s.healRejects(ctx, ws, d); reason != "" {
			s.auditHealDecision(ctx, d, ws.OwnedBy, "failure", reason)
			continue
		}
		// Best-effort, exactly like the live write-back: a deleted workspace or a
		// cap-reached list is skipped, not fatal to the rest of the reconcile.
		written, err := s.cfg.Store.AddWorkspaceEgressDecision(ctx, d.workspace, d.host, d.allow, maxApprovedEgress)
		if err != nil {
			s.auditHealDecision(ctx, d, ws.OwnedBy, "failure", err.Error())
			continue
		}
		s.auditHealDecision(ctx, d, written.OwnedBy, "success", "")
		reconciled++
	}
	return reconciled, nil
}

// egressDecision is one `always`-scoped verdict the boot heal may re-apply: the
// workspace it lands on, the host, the direction, and WHEN the human decided it.
// decidedAt is what makes two decisions on the same host comparable; it is the
// zero time for a row with no decided_at, which sorts oldest — a decided row
// always carries one in Postgres, so the zero value only ever means "as old as
// possible", never "recent".
type egressDecision struct {
	workspace uuid.UUID
	host      string
	allow     bool
	decidedAt time.Time
}

// egressDecisionsToReconcile is the boot heal's decision set: for each
// (workspace, host) the single NEWEST decided `always` egress verdict, minus any
// verdict the operator has since overruled by editing that workspace's egress
// lists directly. Ordered oldest-first so a boot log reads chronologically; the
// entries are independent, so the order is for humans, not correctness.
func (s *Server) egressDecisionsToReconcile(ctx context.Context) ([]egressDecision, error) {
	type listKey struct {
		workspace uuid.UUID
		host      string
	}
	newest := map[listKey]egressDecision{}
	edits := map[uuid.UUID]time.Time{}
	// APPROVED and DENIED only, and that is the whole list on purpose: a durable
	// `always` verdict is something a human decided. EXPIRED and CANCELLED carry
	// no decision at all (both write the zero scope — the sweeper because nobody
	// answered, the terminal-run cascade because the run ended), so there is
	// nothing to replay into a workspace's egress lists and a cancelled approval
	// must never widen one. Pinned by a test, since the omission is the behaviour.
	for _, state := range []types.ApprovalState{types.ApprovalApproved, types.ApprovalDenied} {
		aps, err := s.cfg.Approvals.List(ctx, state)
		if err != nil {
			return nil, err
		}
		for _, ap := range aps {
			d, ok := s.alwaysEgressDecision(ctx, ap, state == types.ApprovalApproved)
			if !ok {
				continue
			}
			if edited := s.lastEgressEdit(ctx, edits, d.workspace); !edited.IsZero() && d.decidedAt.Before(edited) {
				continue // the operator's own list edit is newer than this verdict
			}
			if prev, seen := newest[listKey{d.workspace, d.host}]; seen && !prev.decidedAt.Before(d.decidedAt) {
				continue
			}
			newest[listKey{d.workspace, d.host}] = d
		}
	}
	out := make([]egressDecision, 0, len(newest))
	for _, d := range newest {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b egressDecision) int {
		if c := a.decidedAt.Compare(b.decidedAt); c != 0 {
			return c
		}
		return strings.Compare(a.host, b.host)
	})
	return out, nil
}

// alwaysEgressDecision resolves one decided approval row into the workspace write
// it stands for, or reports false when it stands for none: a non-egress kind, a
// scope short of `always`, a host that would not survive the write-time shape
// check, a run that no longer exists, or a run with no recorded workspace link.
//
// These are the predicates that need only the APPROVAL. The live decide path
// applies two more that need the current workspace row — approveAlwaysRejects
// and denyAlwaysReject — and this function deliberately does not: it is called
// per approval, while the workspace is read once per decision in the caller's
// loop, which is where healRejects asks them. Skipping these two predicates
// here is deliberate, not an oversight: getting it wrong lets the heal
// re-write, on every boot, a deny the live API answers 400 for.
func (s *Server) alwaysEgressDecision(ctx context.Context, ap types.ApprovalRequest, allow bool) (egressDecision, bool) {
	if ap.Kind != types.ApprovalEgressDomain || ap.DecisionScope.Normalize() != types.ScopeAlways {
		return egressDecision{}, false
	}
	host := approvalHost(ap)
	if host == "" || !hostrules.ValidApprovedHost(host) {
		return egressDecision{}, false
	}
	run, err := s.cfg.Store.GetRun(ctx, ap.RunID)
	if err != nil {
		return egressDecision{}, false // run gone; nothing to persist onto
	}
	target := primaryWorkspace(run)
	if target == uuid.Nil {
		return egressDecision{}, false
	}
	d := egressDecision{workspace: target, host: host, allow: allow}
	if ap.DecidedAt != nil {
		d.decidedAt = *ap.DecidedAt
	}
	return d, true
}

// lastEgressEdit is when the operator last REPLACED one of this workspace's
// egress lists through the approved-egress/denied-egress PUTs, memoized in edits
// so the scan costs one GetWorkspace per workspace rather than per approval.
//
// Zero means "never edited that way" — nothing is skipped — and an unreadable
// workspace answers zero for the same reason it does not error: the
// AddWorkspaceEgressDecision that would follow fails on that row anyway, so
// guessing here would only trade one skip for another.
func (s *Server) lastEgressEdit(ctx context.Context, edits map[uuid.UUID]time.Time, id uuid.UUID) time.Time {
	if at, ok := edits[id]; ok {
		return at
	}
	var at time.Time
	if ws, err := s.cfg.Store.GetWorkspace(ctx, id); err == nil && ws.EgressEditedAt != nil {
		at = *ws.EgressEditedAt
	}
	edits[id] = at
	return at
}

// healRejects re-asks the LIVE decide path's direction-specific question about a
// verdict the heal is about to replay, returning the operator-facing reason it
// would be refused today, or "" to proceed.
//
// It is the live path's own two predicates, called on the CURRENT row —
// approveAlwaysRejects for an approve, denyAlwaysReject for a deny — rather than
// a reimplementation, so the heal cannot drift into writing something the API
// refuses. The asymmetry is theirs, not this function's: an approve is rejected
// for a host already routed by construction, a deny for a host the workspace's
// requirements contract marks required.
func (s *Server) healRejects(ctx context.Context, ws types.Workspace, d egressDecision) string {
	if d.allow {
		if _, dead := s.approveAlwaysRejects(ctx, ws)[d.host]; dead {
			return "host is already routed or wired in by construction; a permanent approved-egress entry for it is never consulted"
		}
		return ""
	}
	return s.denyAlwaysReject(ctx, ws, d.host)
}

// auditHealDecision records what the boot heal did to a workspace's egress list.
//
// The heal made DURABLE writes and recorded nothing at all, while its live twin
// persistWorkspaceEgressDecision audits even its give-up paths precisely so "one
// audit query answers 'how did this host get onto this workspace's list'". A
// host that appears only because a boot replayed a months-old verdict was
// exactly the case that query could not answer.
//
// Same action namespace and the same O5 cross-user marker as the live path, so
// the two are one queryable stream; the SOURCE distinguishes them ("boot-heal"
// rather than "approval:<id>"), because "an operator clicked this" and "a
// restart replayed this" are different facts about the same row.
func (s *Server) auditHealDecision(ctx context.Context, d egressDecision, owner, outcome, detail string) {
	action := "workspace.egress.deny"
	if d.allow {
		action = "workspace.egress.approve"
	}
	data := map[string]any{"domains": []string{d.host}, "source": "boot-heal"}
	if detail != "" {
		data["detail"] = detail
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", action, d.workspace.String(), outcome,
		auditWorkspaceDataFor("", owner, data)))
}
