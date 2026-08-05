// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// decisionRequest is the approve/deny body.
type decisionRequest struct {
	Reason string `json:"reason"`
}

// handleListApprovals returns approvals filtered by ?state= (empty = all) and
// ?run_id= (empty = every run), paginated by ?limit=&offset= (see parseListPage).
//
// When the lister also implements the OPTIONAL approvalPageLister (the wardynd
// adapter does, promoting store.PG's ListApprovalsPage), the un-filtered list
// applies LIMIT/OFFSET at the DB — the console polls four single-state lists
// every 10s and decided rows are never deleted, so the unpaged read grew with
// deployment age. A lister without it (test fakes) keeps servePage's fetch-all
// fallback.
//
// ?run_id= stays on the fetch-all path and filters INSIDE the closure, i.e.
// before servePage windows the result. That ordering is the whole point: the
// list is requested_at DESC and capped at maxListLimit, so filtering after the
// window would drop a run older than the newest 1000 approvals from its own
// detail page — silently, with a PENDING badge of 0.
//
// Ownership scoping (item 2): a member's ?run_id= must name an owned run —
// checked via the SAME getRunAuthorized gate GET/kill/profile/grants use, so a
// foreign or unknown run_id answers with the byte-identical 404 (no existence
// oracle). A member's UNSCOPED list (no run_id) is narrowed to approvals on
// runs they created (store.ApprovalsByRunCreatorPager) — fail CLOSED, never an
// unscoped fallback, when the backend does not implement it.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	state := types.ApprovalState(r.URL.Query().Get("state"))
	switch state {
	case "", types.ApprovalPending, types.ApprovalApproved, types.ApprovalDenied, types.ApprovalExpired:
	default:
		writeError(w, http.StatusBadRequest, "invalid state filter")
		return
	}
	var runID uuid.UUID
	if raw := r.URL.Query().Get("run_id"); raw != "" {
		var err error
		if runID, err = uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid run_id")
			return
		}
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}

	if !s.isOperator(r.Context()) {
		if runID == uuid.Nil {
			pager, capable := s.cfg.Approvals.(store.ApprovalsByRunCreatorPager)
			if !capable {
				writeError(w, http.StatusInternalServerError, "approval listing is not scoped for members on this backend")
				return
			}
			principal := principalFromRequest(r)
			servePage(w, page, func(p store.Page) ([]types.ApprovalRequest, error) {
				return pager.ListApprovalsPageByRunCreator(r.Context(), principal, state, p)
			}, nil)
			return
		}
		// ?run_id= given: prove ownership up front. Once proven, the fetch-all +
		// filter-by-runID path below is exactly as scoped as the admin path — it
		// can only ever surface THIS one, now-owned run's approvals.
		if _, ok := s.getRunAuthorized(w, r, runID); !ok {
			return
		}
	}

	var pageFn func(store.Page) ([]types.ApprovalRequest, error)
	if pl, ok := s.cfg.Approvals.(approvalPageLister); ok && runID == uuid.Nil {
		pageFn = func(p store.Page) ([]types.ApprovalRequest, error) {
			return pl.ListApprovalsPage(r.Context(), state, p)
		}
	}
	servePage(w, page, pageFn, func() ([]types.ApprovalRequest, error) {
		all, err := s.cfg.Approvals.List(r.Context(), state)
		if err != nil || runID == uuid.Nil {
			return all, err
		}
		out := make([]types.ApprovalRequest, 0, len(all))
		for _, ap := range all {
			if ap.RunID == runID {
				out = append(out, ap)
			}
		}
		return out, nil
	})
}

// approvalPageLister is the optional DB-paged read surface an ApprovalService
// may additionally implement (see handleListApprovals). Deliberately NOT part
// of ApprovalService: test doubles embedding the interface keep compiling, the
// exact rationale store.Pager documents.
type approvalPageLister interface {
	ListApprovalsPage(ctx context.Context, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error)
}

// handleApproveApproval transitions an approval to APPROVED. For credential
// approvals the broker mints inside the same transaction that observes the
// APPROVED state (handled by the broker on the next mint call); here we only
// record the human decision via the approval FSM. Owner-or-admin (item 3): a
// member may decide an approval raised by a run THEY own.
func (s *Server) handleApproveApproval(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, true)
}

// handleDenyApproval transitions an approval to DENIED (fail closed).
// Owner-or-admin (item 3): a member may decide an approval raised by a run
// THEY own.
func (s *Server) handleDenyApproval(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, false)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	id, ok := parseIDParam(w, r, "id", "approval")
	if !ok {
		return
	}
	if !s.isOperator(r.Context()) {
		// Member: may decide only an approval raised by a run THEY own (item 3).
		// 404-shaped — byte-identical to "approval not found" — for a foreign or
		// unknown approval id (no existence oracle), same philosophy as
		// getRunAuthorized. Get() is a plain read (no state change), so probing
		// it costs nothing an admin's own decide attempt wouldn't have anyway.
		ap, err := s.cfg.Approvals.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, "approval not found")
			return
		}
		run, rerr := s.cfg.Store.GetRun(r.Context(), ap.RunID)
		if rerr != nil || !s.ownsRunOrAdmin(r, run) {
			writeError(w, http.StatusNotFound, "approval not found")
			return
		}
	}
	var body decisionRequest
	if r.Body != nil {
		// Reason is optional; ignore a decode error on an empty body.
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body)
	}
	decidedByType, decidedBy := actorFromRequest(r)

	result, err := s.cfg.Approvals.Decide(r.Context(), id, approve, decidedByType, decidedBy, body.Reason)
	if err != nil {
		switch {
		// One sentinel: approval.ErrAlreadyDecided IS store.ErrAlreadyDecided.
		case errors.Is(err, approval.ErrAlreadyDecided):
			writeError(w, http.StatusConflict, "approval already decided")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "approval not found")
		default:
			writeError(w, http.StatusInternalServerError, "decide approval: "+err.Error())
		}
		return
	}
	// Counted HERE, at the one service call both handlers (and every non-human
	// decision path) funnel through — never in handleApprove/handleDeny, which
	// would each need their own increment. The counter cannot live in
	// approval.Decide itself: internal/api imports internal/approval, so the
	// dependency only runs this way.
	s.metrics.approvalDecided(approve)
	if approve {
		s.learnVerifyEgress(r.Context(), result, decidedByType, decidedBy)
	}
	writeJSON(w, http.StatusOK, result)
}

// learnVerifyEgress is the verify loop's write-back, hooked at the ONE
// chokepoint every approval decision funnels through: approving an
// egress_domain request raised DURING a workspace verify/record session lands
// the host as an `egress:<host>` row in THAT workspace's own requirements
// contract (required, operator_set — a human just clicked) the moment the
// decision is made. The gates are the trusted linkages, never sandbox input:
// the run row's WorkspaceID and its "workspace record" task discriminator — a
// PLAIN run's approval widens only its own run and writes NOTHING durable.
// The row lands on the WORKSPACE overlay, never a shared library source:
// approving a host for this aggregate must not leak the approval into every
// other workspace attaching the same source. Every guard fails silent — the
// approval itself already stands; this is the durable echo, not the decision.
func (s *Server) learnVerifyEgress(ctx context.Context, ap types.ApprovalRequest, byType types.ActorType, by string) {
	if ap.Kind != types.ApprovalEgressDomain || s.cfg.Store == nil {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, ap.RunID)
	if err != nil || run.WorkspaceID == nil || run.Task != "workspace record" {
		return
	}
	var scope struct {
		Host string `json:"host"`
	}
	if json.Unmarshal(ap.RequestedScope, &scope) != nil {
		return
	}
	host := strings.ToLower(strings.TrimSpace(scope.Host))
	if host == "" || !workspacescan.ValidApprovedHost(host) {
		return
	}
	key := "egress:" + host
	if _, err := s.cfg.Store.MergeWorkspaceRequirements(ctx, *run.WorkspaceID, map[string]types.WorkspaceRequirement{
		key: {Level: "required", Provenance: "operator_set"},
	}); err != nil {
		// The approval stands either way; the contract write is audited as the
		// miss it is (cap hit / workspace gone) so the operator can add the row
		// on Reach instead of wondering why the replay still denies the host.
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
			run.WorkspaceID.String(), "failure", mustJSON(map[string]any{
				"key": key, "source": "verify:" + ap.RunID.String(), "detail": err.Error(),
			})))
		return
	}
	s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
		run.WorkspaceID.String(), "success", mustJSON(map[string]any{
			"key": key, "source": "verify:" + ap.RunID.String(),
		})))
}
