// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197: the attention rule (attention/heldCandidates), its projection
// onto a page of runs (projectAttention), and GET /me/attention
// (handleMeAttention). New file, beside approvals_decidable.go, for the same
// reason: approvals.go and runs_policy.go are both close to
// scripts/check-file-size.sh's 1000-line cap.
package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attentionCandidate is one held row's contribution to a run's attention:
// what kind of thing it is, and who (in the caller's own view) can clear it.
type attentionCandidate struct {
	kind types.AttentionKind
	by   types.AttentionBy
}

// isADOReauth reports whether ap is an Azure DevOps sign-in or consent
// request — TallyKey is the SAME structural test CancelForRun/Hold already
// use, reused so this can never classify a row differently than they do.
func isADOReauth(ap types.ApprovalRequest) bool {
	k := approval.TallyKey(ap)
	return k == approval.TallyReauthADOSignIn || k == approval.TallyReauthADOConsent
}

// heldCandidates lists every held row on the run (pending must already be
// projectHolds'd), each classified into rule 6 (ado_consent), 7 (reauth) or 8
// (approval) — in that order, rule 6's rows first, then 7's, then 8's, which
// is the precedence attention() walks to pick one. Rule 6/7's `by` depends
// only on which view is asking (view=user forces owner=me, so "you" IS the
// run's owner there; the Admin view is never forced to it, so a
// credential_reauth/lost run's clearer is always the run's owner, "owner" —
// only the actor who can actually act is ever shown "you"). Rule 8's `by`
// depends on THIS caller's own mayDecide verdict, since
// unlike 6/7 a member CAN sometimes clear one of these.
func (s *Server) heldCandidates(r *http.Request, run types.AgentRun, pending []types.ApprovalRequest, adminView bool) []attentionCandidate {
	ownerBy := types.AttentionOwner
	if !adminView {
		ownerBy = types.AttentionYou
	}
	var rule6, rule7, rule8 []attentionCandidate
	for _, ap := range pending {
		if !ap.Held {
			continue
		}
		switch {
		case ap.Kind == types.ApprovalCredentialReauth && isADOReauth(ap):
			rule6 = append(rule6, attentionCandidate{types.AttentionADOConsent, ownerBy})
		case ap.Kind == types.ApprovalCredentialReauth:
			rule7 = append(rule7, attentionCandidate{types.AttentionReauth, ownerBy})
		default:
			by := types.AttentionAdmin
			if s.mayDecide(r, ap, run) {
				by = types.AttentionYou
			}
			rule8 = append(rule8, attentionCandidate{types.AttentionApproval, by})
		}
	}
	out := make([]attentionCandidate, 0, len(rule6)+len(rule7)+len(rule8))
	out = append(out, rule6...)
	out = append(out, rule7...)
	return append(out, rule8...)
}

// attention is #1197's rule table (rows 1-8), given run and every PENDING
// approval it has raised (already projectHolds'd — see projectAttention).
// Nil means the run needs nobody's attention right now.
func (s *Server) attention(r *http.Request, run types.AgentRun, pending []types.ApprovalRequest, adminView bool) *types.RunAttention {
	// Row 1: a terminal run needs nobody — attentionFor's own "terminal
	// outranks hold" rule, restated here.
	if run.State.IsTerminal() {
		return nil
	}
	// Row 2: a lease-ended run reads as "Ended at its end time", grey, not a
	// hold — even though it is technically still RUNNING until the ended-run
	// grace stops it.
	if run.LostReason == types.LostEnded {
		return nil
	}
	// Row 3: lost for any OTHER reason (reboot, outage) needs a revive —
	// owner-or-super-admin only (run_revive.go), hence `owner` in the
	// Admin view: reviving is only ever offered on the User view's own page.
	if run.LostAt != nil {
		by := types.AttentionOwner
		if !adminView {
			by = types.AttentionYou
		}
		return &types.RunAttention{Kind: types.AttentionLost, By: by, Pending: len(pending)}
	}
	// Row 4: reserved, no producer today (types.RunWaiting's own doc) — kept
	// so a future producer needs no change here.
	if run.State == types.RunWaiting {
		return &types.RunAttention{Kind: types.AttentionApproval, By: types.AttentionYou, Pending: len(pending)}
	}
	candidates := s.heldCandidates(r, run, pending, adminView)
	if len(candidates) == 0 {
		return nil // row 5: nothing PENDING is actually held
	}
	pick := candidates[0]
	for _, c := range candidates {
		if c.by == types.AttentionYou {
			pick = c
			break
		}
	}
	return &types.RunAttention{Kind: pick.kind, By: pick.by, Pending: len(pending)}
}

// errNoAttentionCapability is the fail-closed answer when the wired
// ApprovalService cannot serve the one extra read attention projection needs
// (a test double, never wardynd's own wiring — see cmd/wardynd/adapters.go).
var errNoAttentionCapability = errors.New("attention projection is not scoped for this request on this backend")

// pendingApprovalsForRuns loads every PENDING approval on runIDs and
// projects Held/HeldUntil onto each (projectHolds) — the ONE extra read
// projectAttention needs, batched over the whole page rather than once per
// run.
func (s *Server) pendingApprovalsForRuns(r *http.Request, runIDs []uuid.UUID) (map[uuid.UUID][]types.ApprovalRequest, error) {
	pager, ok := s.cfg.Approvals.(store.ApprovalsForRunsPager)
	if !ok {
		return nil, errNoAttentionCapability
	}
	rows, err := pager.ListPendingApprovalsForRuns(r.Context(), runIDs)
	if err != nil {
		return nil, err
	}
	s.projectHolds(rows)
	out := map[uuid.UUID][]types.ApprovalRequest{}
	for _, ap := range rows {
		out[ap.RunID] = append(out[ap.RunID], ap)
	}
	return out, nil
}

// projectAttention sets Attention on every row of runs — read-only rows this
// lane's callers already scoped and paged; nothing here changes which rows
// are returned or their order. adminView is the SAME view= the caller
// resolved for the runs list itself (parsedRunsListParams.view == "admin").
func (s *Server) projectAttention(r *http.Request, runs []types.AgentRun, adminView bool) error {
	ids := make([]uuid.UUID, len(runs))
	for i, run := range runs {
		ids[i] = run.ID
	}
	byRun, err := s.pendingApprovalsForRuns(r, ids)
	if err != nil {
		return err
	}
	// One capability batch for the WHOLE page (#1197): mayDecide's
	// egress_host check would otherwise cost one grant+subject read per held
	// egress row per poll. Installed once here, at the top of the one
	// resolution every held row in this page is projected under.
	ctx := withCapBatch(r.Context())
	req := r.WithContext(ctx)
	for i := range runs {
		runs[i].Attention = s.attention(req, runs[i], byRun[runs[i].ID], adminView)
	}
	return nil
}

// meAttention is GET /me/attention's response body.
type meAttention struct {
	NeedsYou         int `json:"needs_you"`
	PendingApprovals int `json:"pending_approvals"`
}

// handleMeAttention answers the shell's nav badges: needs_you (live runs in
// this view's own default scope whose attention.by=="you") and
// pending_approvals (the PENDING count GET /approvals?state=PENDING scopes
// today — unchanged so the badge matches the page it links to; the owner's
// own ruling on this is that the badge counts only what the viewer could act
// on, which is exactly what attention.by=="you" already answers).
//
// ?view=user|admin, same coercion GET /runs?view= applies: absent means
// "user", and "admin" from a non-security-operator is coerced to "user"
// (fail closed) — never a 403, since a member simply has no admin view to
// ask about.
func (s *Server) handleMeAttention(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	switch view {
	case "", "user":
		view = "user"
	case "admin":
		if !s.isSecurityOperator(r.Context()) {
			view = "user"
		}
	default:
		// The SAME cause GET /runs' own ?view= check answers
		// (reasonInvalidViewParam, #656 slice 1).
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidViewParam, "invalid view")
		return
	}
	adminView := view == "admin"
	principal := principalFromRequest(r)

	runsPager, ok := s.cfg.Store.(store.RunsFilteredPager)
	if !ok {
		// The SAME cause GET /runs' own scoping check answers
		// (reasonListingUnscopedBackend, #656 slice 1).
		writeErrorReason(w, http.StatusInternalServerError, reasonListingUnscopedBackend, "run listing is not scoped for this request on this store backend")
		return
	}
	owner := principal
	if adminView {
		owner = ""
	}
	runs, err := runsPager.ListRunsFiltered(r.Context(), store.RunFilter{Owner: owner, Statuses: []string{"active"}}, store.Page{})
	if err != nil {
		writeServerError(w, r, "list", err)
		return
	}
	if err := s.projectAttention(r, runs, adminView); err != nil {
		writeServerError(w, r, "list", err)
		return
	}
	needsYou := 0
	for _, run := range runs {
		if run.Attention != nil && run.Attention.By == types.AttentionYou {
			needsYou++
		}
	}

	pendingApprovals, err := s.scopedPendingApprovalsCount(r, adminView, principal)
	if err != nil {
		writeServerError(w, r, "list", err)
		return
	}

	writeJSON(w, http.StatusOK, meAttention{NeedsYou: needsYou, PendingApprovals: pendingApprovals})
}

// scopedPendingApprovalsCount mirrors handleListApprovals' own scoping
// EXACTLY (approvals.go's scopeToOwner branch): an admin view counts every
// PENDING approval in the deployment; the user view counts only PENDING
// approvals on runs this principal created — fail-closed (500) without
// ApprovalsForRunsPager, the same posture handleListApprovals takes for an
// unscoped member list. COUNTED in the database (CountPendingApprovals /
// CountPendingApprovalsByRunCreator), never listed: a poll every 5 s from
// every open console must not read every PENDING row's full columns just to
// answer "how many" — the same rule CountApprovalsForRun's own doc states.
func (s *Server) scopedPendingApprovalsCount(r *http.Request, adminView bool, principal string) (int, error) {
	counter, ok := s.cfg.Approvals.(store.ApprovalsForRunsPager)
	if !ok {
		return 0, errNoAttentionCapability
	}
	if adminView {
		return counter.CountPendingApprovals(r.Context())
	}
	return counter.CountPendingApprovalsByRunCreator(r.Context(), principal)
}
