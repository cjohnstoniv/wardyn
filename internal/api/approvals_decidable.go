// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197: the hold projection (projectHolds) and the pure "could THIS
// caller decide THIS approval" predicate (mayDecide) the attention rule's row
// 8 needs. New file: approvals.go is at 985/1000 lines
// (scripts/check-file-size.sh's cap), and both of these are read-only
// mirrors of gates decide() itself owns — keeping them beside decide() would
// invite editing one copy and not the other.
package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// projectHolds sets Held/HeldUntil on every row in place, via approval.Hold —
// the ONE projection point for both GET /approvals (handleListApprovals's
// page functions, wrapped in approvals.go) and the attention rule below
// (which needs a run's own PENDING rows' Held/HeldUntil to pick row 8's
// kind/by). A decided row (Hold's own state gate) leaves both fields at
// their zero value, which is exactly "no hold" — nothing to clear here.
func (s *Server) projectHolds(rows []types.ApprovalRequest) {
	now := s.cfg.Now()
	for i := range rows {
		held, until := approval.Hold(rows[i], now)
		rows[i].Held = held
		rows[i].HeldUntil = nil
		if held && !until.IsZero() {
			u := until
			rows[i].HeldUntil = &u
		}
	}
}

// withHoldProjection and withHoldProjectionAll wrap handleListApprovals' own
// page/fetch-all closures so every PENDING row they return carries
// Held/HeldUntil before servePage serializes it — the ONE projection point
// for every public GET /approvals read (handleListApprovals's four
// pageFn/allFn call sites, approvals.go). A nil pageFn (no DB-paged
// capability on this backend) stays nil, matching servePage's own fallback.
func (s *Server) withHoldProjection(fn func(store.Page) ([]types.ApprovalRequest, error)) func(store.Page) ([]types.ApprovalRequest, error) {
	if fn == nil {
		return nil
	}
	return func(p store.Page) ([]types.ApprovalRequest, error) {
		rows, err := fn(p)
		if err != nil {
			return nil, err
		}
		s.projectHolds(rows)
		return rows, nil
	}
}

func (s *Server) withHoldProjectionAll(fn func() ([]types.ApprovalRequest, error)) func() ([]types.ApprovalRequest, error) {
	return func() ([]types.ApprovalRequest, error) {
		rows, err := fn()
		if err != nil {
			return nil, err
		}
		s.projectHolds(rows)
		return rows, nil
	}
}

// mayDecide reports whether the caller may ACT on ap — approve or at least
// deny it — WITHOUT loading either row again (both are already in hand from
// the attention projection's own read) and WITHOUT writing a response. It
// mirrors decide()'s own gates, in the same order decide() runs them,
// restricted to the gates that depend only on WHO is asking and WHAT the
// row is — never on the request BODY (decide's scope rules 1-7 all read the
// POST body, which mayDecide has none of; a bodyless decide is exactly what
// every attention-driven Approve/Deny button sends by default).
//
// One known divergence: an Azure DevOps escalation outside the
// administrator's ceiling. decide()'s own ADO rule (adoDecisionRule,
// injection_ado_capability.go) refuses an APPROVE above the ceiling with a
// 403 before Decide() is ever reached, but mayDecide does not repeat that
// check (decidableKindAndOwner's own doc explains why) — so a member sees
// "you" on a row they could still deny (deny is never ceiling-gated), just
// not approve. This is why the doc above says "act (approve or at least
// deny)" rather than "approve would succeed": the weaker claim is the one
// that stays true.
//
// Pinned by TestMayDecideAgreesWithDecide, which drives the SAME cases
// through a real POST .../approve on the fake harness and asserts the two
// never disagree over that verb — see that test's own comment for why a
// hand-matched mirror of authorizeUserDecision/requireSecondHuman alone is
// not enough: decide() can grow a gate neither of those two functions names,
// and only replaying the real path catches it.
func (s *Server) mayDecide(r *http.Request, ap types.ApprovalRequest, run types.AgentRun) bool {
	// decide()'s rule 3b: nobody decides a credential_reauth, security
	// operator included — 409 on every tier, unconditionally.
	if ap.Kind == types.ApprovalCredentialReauth {
		return false
	}
	if !s.decidableKindAndOwner(r, ap, run) {
		return false
	}
	return s.secondHumanAllows(r, ap, run)
}

// decidableKindAndOwner mirrors authorizeUserDecision's verdict (the 404/403
// gates), given ap/run already in hand: a security operator decides any kind
// (isSecurityOperator); otherwise only an egress_domain approval or a
// structural Azure DevOps escalation (adoEscalationScope) on a run the
// caller owns (ownsRunOrAdmin) — and, for the plain egress_domain case only,
// the caller must additionally hold the row's own host as an egress_host
// capability grant (capSeamAllowed). An ADO escalation needs no capability
// check here: ownership is authorizeUserDecision's whole member rule for
// that shape.
//
// Known gap, NOT reproduced here on purpose: decide()'s own ADO rule
// (adoDecisionRule, injection_ado_capability.go) refuses an APPROVE above
// the administrator's ceiling with a 403, checked before Decide() runs. This
// function says nothing about the ceiling, so a member's ADO escalation
// outside it still passes here. See mayDecide's own doc for why that is an
// acceptable weaker claim (the row can still be denied) rather than a bug.
func (s *Server) decidableKindAndOwner(r *http.Request, ap types.ApprovalRequest, run types.AgentRun) bool {
	if s.isSecurityOperator(r.Context()) {
		return true
	}
	_, isADO := adoEscalationScope(ap)
	if ap.Kind != types.ApprovalEgressDomain && !isADO {
		return false
	}
	if !s.ownsRunOrAdmin(r, run) {
		return false
	}
	if isADO {
		return true
	}
	allowed, err := s.capSeamAllowed(r.Context(), capEgressHost, approvalHost(ap))
	return err == nil && allowed
}

// secondHumanAllows mirrors requireSecondHuman's verdict, given ap/run
// already in hand (no re-fetch, no response write): only egress_domain is
// governed, off by default (envEgressSecondHuman), the admin-token
// break-glass always passes, LocalMode can never satisfy it (both operands
// of "a second human decided" are client-supplied there — see
// requireSecondHuman's own doc), and otherwise it refuses only when the
// asking principal IS the run's own creator.
func (s *Server) secondHumanAllows(r *http.Request, ap types.ApprovalRequest, run types.AgentRun) bool {
	if !envEnabled(envEgressSecondHuman) || ap.Kind != types.ApprovalEgressDomain {
		return true
	}
	actorType, principal := actorFromRequest(r)
	if actorType == types.ActorSystem && principal == adminTokenPrincipal {
		return true // the same break-glass requireSecondHuman reports (and decide() then audits)
	}
	if s.cfg.LocalMode {
		return false // requireSecondHuman's unconditional 503: unenforceable here
	}
	return run.CreatedBy == "" || run.CreatedBy != principal
}
