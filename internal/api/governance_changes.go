// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The /governance/changes routes: list and read the changes held for a second human, and approve or
// reject one. Registered on securityOps; the per-target approver table (governanceChangeKinds)
// decides inside each handler who may act on which kind, so a kind that only a super admin may write
// is never shown to, or decided by, a security admin.
//
// A decision is ONE transaction (store.DecideGovernanceChange): lock the change row; require it
// pending and unexpired; re-check that the approver is still who they were (a revoked token, a
// session cut off, a role that no longer passes); compare the target and the deployment default with
// what the proposal reviewed (409 governance_change_stale on a mismatch); apply the write through the
// store's Querier forms; move the state; commit. The audit rows follow the commit. A failure anywhere
// before the commit leaves the target unwritten and the change pending.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// governanceChangeStates is the closed set a list may be narrowed to.
var governanceChangeStates = []string{
	types.GovernanceChangePending, types.GovernanceChangeApplied, types.GovernanceChangeRejected,
	types.GovernanceChangeExpired, types.GovernanceChangeStale,
}

// mountGovernanceChangeRoutes registers the four /governance/changes routes on the SECURITY tier. The
// group a mount function receives is decided at the call site (routes.go), and authz_test.go's
// chi.Walk matrix is what enforces it.
func (s *Server) mountGovernanceChangeRoutes(securityOps chi.Router) {
	securityOps.Get("/governance/changes", s.handleListGovernanceChanges)
	securityOps.Get("/governance/changes/{id}", s.handleGetGovernanceChange)
	securityOps.Post("/governance/changes/{id}/approve", s.handleApproveGovernanceChange)
	securityOps.Post("/governance/changes/{id}/reject", s.handleRejectGovernanceChange)
}

// handleListGovernanceChanges lists changes newest first. ?state= narrows to one state and defaults to
// pending, the queue an approver works. A change of a kind the caller may not approve is not listed.
func (s *Server) handleListGovernanceChanges(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = types.GovernanceChangePending
	}
	if !slices.Contains(governanceChangeStates, state) {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceChangeStateInvalid,
			"state must be one of "+strings.Join(governanceChangeStates, ", "))
		return
	}
	all, err := s.cfg.Store.ListGovernanceChanges(r.Context(), state)
	if err != nil {
		writeServerError(w, r, "list governance changes", err)
		return
	}
	out := make([]types.GovernanceChange, 0, len(all))
	for _, ch := range all {
		if s.canSeeGovernanceKind(r, ch.TargetKind) {
			out = append(out, ch)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetGovernanceChange returns one change with its diff. A change the caller may not approve
// answers 404, the hidden-404 parity the rest of the API keeps.
func (s *Server) handleGetGovernanceChange(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "governance change")
	if !ok {
		return
	}
	ch, err := s.cfg.Store.GetGovernanceChange(r.Context(), id)
	if err == nil && !s.canSeeGovernanceKind(r, ch.TargetKind) {
		err = store.ErrNotFound
	}
	if notFoundIf(w, err, "governance change", reasonGovernanceChangeNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read governance change", err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) handleApproveGovernanceChange(w http.ResponseWriter, r *http.Request) {
	s.decideGovernanceChange(w, r, true)
}

func (s *Server) handleRejectGovernanceChange(w http.ResponseWriter, r *http.Request) {
	s.decideGovernanceChange(w, r, false)
}

// govRefusal is a refusal decided inside the decision transaction, carried out of it so the
// transaction rolls back (the change stays pending) and the response is written after.
type govRefusal struct {
	write func(w http.ResponseWriter, r *http.Request)
	// why is a short label for the error text.
	why string
}

func (e *govRefusal) Error() string { return "governance change refused: " + e.why }

// readRejectReason reads the optional {reason} body of a reject: trimmed, at most 512 characters, and
// free of control characters. A reason that fails either is a 400 that leaves the change pending.
func (s *Server) readRejectReason(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, ok := readCappedBody(w, r, 16<<10, "request body")
	if !ok {
		return "", false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return "", true
	}
	var req struct {
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid JSON body: "+err.Error())
		return "", false
	}
	reason := strings.TrimSpace(req.Reason)
	switch {
	case utf8.RuneCountInString(reason) > maxGovernanceRejectReasonLen:
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "reason: at most 512 characters")
		return "", false
	case !controlCharFree(reason):
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "reason: must not contain control characters")
		return "", false
	}
	return reason, true
}

// sameGovernanceHuman reports whether the approver is the person who proposed the change: the same
// principal, or the same non-empty email once case-folded. requireSecondHuman compares the principal
// only; here one person holding two principals (an SSO session and an API token, or two identity
// provider subjects with one mailbox) must not count as two humans.
func sameGovernanceHuman(ch types.GovernanceChange, principal, email string) bool {
	if principal != "" && principal == ch.ProposedBy {
		return true
	}
	return email != "" && ch.ProposedByEmail != "" && strings.EqualFold(email, ch.ProposedByEmail)
}

// recheckApprover re-establishes, inside the decision transaction, that the approver still has the
// authority they had when the request was authenticated. A session or the admin token carries nothing
// to re-read. An API token's row is read again by id (revoked or expired is refused), the
// session-revocation cutoff is asked again, and the approver predicate is re-evaluated against the
// role the row holds now. Each failure is the auth lane's own answer, never a pass.
func (s *Server) recheckApprover(ctx context.Context, r *http.Request, q store.Querier, kind govKind) error {
	tokenID := apiTokenIDFromContext(ctx)
	if tokenID == uuid.Nil {
		return nil
	}
	t, err := store.GetLiveAPITokenQ(ctx, q, tokenID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &govRefusal{why: "approver token revoked or expired", write: func(w http.ResponseWriter, r *http.Request) {
			s.auditAuthFailed(r, "invalid_admin_token")
			writeErrorReason(w, http.StatusUnauthorized, reasonInvalidAdminToken, "invalid admin token")
		}}
	case err != nil:
		return err
	}
	if s.cfg.SessionRevocations != nil {
		revoked, rerr := s.cfg.SessionRevocations.IsSessionRevoked(ctx, t.Principal, t.Email, t.CreatedAt)
		if rerr != nil {
			return &govRefusal{why: "session revocation unreadable", write: func(w http.ResponseWriter, r *http.Request) {
				s.metrics.authStoreErrorInc()
				writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "api token lookup failed")
			}}
		}
		if revoked {
			return &govRefusal{why: "approver session revoked", write: func(w http.ResponseWriter, r *http.Request) {
				s.auditAuthFailed(r, "invalid_admin_token")
				writeErrorReason(w, http.StatusUnauthorized, reasonInvalidAdminToken, "invalid admin token")
			}}
		}
	}
	if s.cfg.RoleStampTTL > 0 && !sshRoleFresh(t.IdentityStampedAt, s.cfg.Now().UTC(), s.cfg.RoleStampTTL) {
		return &govRefusal{why: "approver role stamp stale", write: func(w http.ResponseWriter, r *http.Request) {
			s.refuseStaleRoleStamp(w, r, t)
		}}
	}
	fresh := withHumanIdentity(ctx, t.Principal, t.Email, t.Role, t.UserType, t.Groups, t.GroupsTruncated == nil || *t.GroupsTruncated)
	if !kind.approver.allowed(s, r.WithContext(fresh)) {
		return s.denyApprover(kind)
	}
	return nil
}

// denyApprover is the refusal a caller whose tier does not pass the kind's approver predicate gets.
func (s *Server) denyApprover(kind govKind) *govRefusal {
	return &govRefusal{why: "approver tier", write: func(w http.ResponseWriter, r *http.Request) {
		s.refuse(w, r, authz.Deny(kind.approver.reason, govChangeTarget, ""))
	}}
}

// decideGovernanceChange is approve (apply the held write) and reject (apply nothing) in one body,
// since the two differ only in the apply and the distinct-human rule: the proposer may reject their
// own change, which applies nothing.
func (s *Server) decideGovernanceChange(w http.ResponseWriter, r *http.Request, approve bool) {
	id, ok := parseIDParam(w, r, "id", "governance change")
	if !ok {
		return
	}
	bypass := isAdminTokenCaller(r)
	if envEnabled(envGovernanceSecondHuman) && s.cfg.LocalMode && !bypass {
		s.refuseGovernanceLocalMode(w)
		return
	}
	var reason string
	if !approve {
		if reason, ok = s.readRejectReason(w, r); !ok {
			return
		}
	}
	ctx := r.Context()
	head, err := s.cfg.Store.GetGovernanceChange(ctx, id)
	if notFoundIf(w, err, "governance change", reasonGovernanceChangeNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read governance change", err)
		return
	}
	kind, known := governanceChangeKinds[head.TargetKind]
	if !known {
		writeServerError(w, r, "decide governance change", errors.New("unknown target kind "+head.TargetKind))
		return
	}
	if !bypass && !kind.approver.allowed(s, r) {
		s.denyApprover(kind).write(w, r)
		return
	}
	actorType, principal := actorFromRequest(r)
	email := oidcEmailFromContext(ctx)

	decision := store.GovernanceDecision{To: types.GovernanceChangeApplied, By: principal, ByEmail: email}
	if !approve {
		decision.To, decision.Reason = types.GovernanceChangeRejected, reason
	}
	var applied govApplied
	out, err := s.cfg.Store.DecideGovernanceChange(ctx, id, decision, func(q store.Querier, ch types.GovernanceChange) error {
		if !bypass {
			if err := s.recheckApprover(ctx, r, q, kind); err != nil {
				return err
			}
			if approve && sameGovernanceHuman(ch, principal, email) {
				return &govRefusal{why: "approver proposed the change", write: func(w http.ResponseWriter, r *http.Request) {
					s.refuse(w, r, authz.Deny(authz.ReasonSecondHumanRequired, govChangeTarget, envGovernanceSecondHuman+
						" is set: a second human must approve this — you proposed it, so someone else approves it (you may reject it)"))
				}}
			}
		}
		if !approve {
			return nil
		}
		if ch.DeploymentHash != computeETag(s.cfg.DefaultPolicy) {
			return store.ErrGovernanceChangeStale
		}
		var err error
		applied, err = kind.apply(s, r, q, ch)
		return err
	})
	if err != nil {
		s.answerFailedDecision(w, r, head, approve, bypass, actorType, principal, err)
		return
	}

	data := map[string]any{"target_kind": out.TargetKind, "target_key": out.TargetKey, "proposed_by": out.ProposedBy}
	if approve {
		// The target's own row, as a direct write would have written it, with the actor the approver
		// and the two facts that tie it to the proposal.
		tdata := map[string]any{}
		for k, v := range applied.data {
			tdata[k] = v
		}
		if applied.afterCommit != nil {
			for k, v := range applied.afterCommit() {
				tdata[k] = v
			}
		}
		tdata["change_id"], tdata["proposed_by"] = out.ID, out.ProposedBy
		s.recordAudit(ctx, s.auditEvent(nil, actorType, principal, applied.action, applied.target, "success", mustJSON(tdata)))
		s.recordAudit(ctx, s.auditEvent(nil, actorType, principal, "governance.change.approve", out.ID.String(), "success", mustJSON(data)))
		if bypass {
			s.recordGovernanceBypass(r, out.TargetKind, out.ID.String(), "success", map[string]any{"change_id": out.ID, "proposed_by": out.ProposedBy})
		}
	} else {
		if reason != "" {
			data["reason"] = reason
		}
		s.recordAudit(ctx, s.auditEvent(nil, actorType, principal, "governance.change.reject", out.ID.String(), "success", mustJSON(data)))
	}
	writeJSON(w, http.StatusOK, out)
}

// answerFailedDecision writes the response for a decision that did not commit as asked, and the audit
// rows that record it: a lapsed change is moved to expired (governance.change.expire), and an approval
// that failed for a stale target, a change no longer pending or an error is recorded as
// governance.change.approve with outcome failure and that class, beside a failed bypass row when the
// admin token spent one.
func (s *Server) answerFailedDecision(w http.ResponseWriter, r *http.Request, head types.GovernanceChange, approve, bypass bool,
	actorType types.ActorType, principal string, err error) {
	ctx := r.Context()
	failed := func(class string) {
		if !approve {
			return
		}
		data := map[string]any{"target_kind": head.TargetKind, "target_key": head.TargetKey, "proposed_by": head.ProposedBy, "error": class}
		s.recordAudit(ctx, s.auditEvent(nil, actorType, principal, "governance.change.approve", head.ID.String(), "failure", mustJSON(data)))
		if bypass {
			s.recordGovernanceBypass(r, head.TargetKind, head.ID.String(), "failure", map[string]any{"change_id": head.ID, "error": class})
		}
	}
	var refusal *govRefusal
	var notPending *store.ErrGovernanceChangeNotPending
	var write *profileWriteError
	switch {
	case errors.As(err, &refusal):
		refusal.write(w, r)
	case errors.As(err, &notPending):
		if notPending.Lapsed {
			s.recordAudit(ctx, s.auditEvent(nil, actorType, principal, "governance.change.expire", head.ID.String(), "success",
				mustJSON(map[string]any{"target_kind": head.TargetKind, "target_key": head.TargetKey})))
		}
		failed("not_pending")
		writeErrorReason(w, http.StatusConflict, reasonGovernanceChangeNotPending,
			"this change is no longer pending ("+notPending.State+")")
	case errors.Is(err, store.ErrGovernanceChangeStale):
		failed("stale")
		writeErrorReason(w, http.StatusConflict, reasonGovernanceChangeStale,
			"the target, or the deployment default, has changed since this change was proposed: it was not applied and is now stale; propose it again")
	case errors.As(err, &write):
		failed("error")
		writeErrorReason(w, write.status, write.reason, write.msg)
	case errors.Is(err, store.ErrNotFound):
		failed("error")
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceChangeNotFound, "governance change not found")
	default:
		failed("error")
		writeServerError(w, r, "decide governance change", err)
	}
}
