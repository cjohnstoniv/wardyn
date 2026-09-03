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

// ReconcileWorkspaceEgressDecisions re-applies decided `always`-scoped egress
// decisions to their run's primary workspace, healing D28: the post-Decide
// write-back (persistWorkspaceEgressDecision) is not atomic with Decide, so a PG
// blip there dropped a permanent allow/deny behind a 200 with only a failure
// audit row — future runs then never inherited the operator's decision.
// AddWorkspaceEgressDecision is idempotent (an upsert that also clears the mirror
// list), so re-applying an already-persisted decision is a no-op and a dropped one
// is recreated. Returns the count re-applied, for the boot log.
//
// This is the SMALLER of the two options the finding names (a boot/periodic
// reconcile vs threading one tx through Decide + the workspace write, which spans
// two service interfaces the api layer does not share a tx across). It runs once
// at boot (cmd/wardynd). ponytail: boot-only heals on the next restart; a periodic
// tick would heal sooner on a laptop that rarely reboots — add one if that window
// proves too wide.
//
// A HEAL MUST NOT OUTRANK THE OPERATOR, and two ordering bugs made it do exactly
// that. Both are about the same question — whose word is NEWEST — so both are
// answered here rather than at the write:
//
//   - It walked states in the fixed order [APPROVED, DENIED], never by
//     decided_at. An operator who denied a host and then changed their mind and
//     approved it got the OLDER deny re-applied last on every restart, silently
//     reversing their newest verdict with no audit event. egressDecisionsToReconcile
//     now folds both states into ONE pass and keeps, per (workspace, host), only
//     the decision with the latest DecidedAt — so the reversal is not merely
//     ordered correctly, it is never written at all.
//   - It re-applied a decision the operator had already UNDONE. resolveAlwaysTarget
//     promises an `always` is reversible through the approved-egress/denied-egress
//     PUTs, but the approval row still reads APPROVED/always afterwards, so the
//     next boot put the host back — a durable, fail-OPEN re-widening of a list the
//     operator explicitly narrowed. Those PUTs now stamp Workspace.EgressEditedAt,
//     and a decision older than that stamp is skipped: the operator's most recent
//     action wins whether it was a verdict or a list edit, which is one rule, not
//     two.
//
// The stamp lives on the WORKSPACE rather than as a per-approval "already
// applied" marker because the marker cannot answer this question. The undone
// decision was applied successfully — marking it would not stop the resurrection;
// only knowing that something NEWER happened to the list does. It also keeps the
// heal intact: a decision made after the last manual edit is still re-applied,
// which is the whole point of D28.
//
// The scan reads all decided egress approvals; decided rows are never deleted, so
// on a very long-lived deployment cap this with an incremental scan keyed off the
// newest DecidedAt already reconciled.
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
		// RE-RUN THE LIVE PATH'S DIRECTION-SPECIFIC REJECTS against the CURRENT
		// workspace row, which alwaysEgressDecision's "same predicates, in the
		// same order, the live write-back applies" claimed and did not do. The
		// heal replays a verdict recorded at t0 against a workspace as it is at
		// boot, so the two can have diverged: mark `egress:<host>` REQUIRED after
		// an older deny-always on that host and every restart re-wrote a deny the
		// live API answers 400 for — "the workspace declares a need it can never
		// satisfy" in every confined replay, re-broken on each boot with nothing
		// saying why. The heal's only newer-action guard is EgressEditedAt, which
		// the requirements PUT does not stamp.
		//
		// SKIPPED AND AUDITED, not skipped silently: "how did this host get onto
		// this workspace's list" has to have an answer, and so does "why did it
		// not".
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
// applies two more that need the CURRENT WORKSPACE ROW — approveAlwaysRejects
// and denyAlwaysReject — and this function deliberately does not: it is called
// per approval, while the workspace is read once per decision in the caller's
// loop, which is where healRejects asks them. This comment used to claim "same
// predicates, in the same order, the live write-back applies"; it was false in
// exactly that gap, which is what let the heal re-write, on every boot, a deny
// the live API answers 400 for.
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
