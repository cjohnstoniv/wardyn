// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Interactive entry to a run — a terminal, a UI app, SSH, a take-over — needs the
// run's owner (#1476). A super admin keeps kill, approve, policy, grants, revoke,
// audit, revive, resume and end, and loses the shell: the workload keeps running
// with the owner's personal connections, and naming the actor in the audit does
// not give the owner a say. This file is the one rule and the one place a ticket
// is redeemed, so no entry point can drift from the others.

// mayEnterRun: the run's owner (non-empty and equal, ownsRun's guard,
// helpers.go), or a super admin on a run with no personal owner. OperatorOwned
// is the run's recorded authentication, never created_by.
func mayEnterRun(run types.AgentRun, principal string, superAdmin bool) bool {
	return (run.CreatedBy != "" && principal != "" && run.CreatedBy == principal) ||
		(superAdmin && run.OperatorOwned)
}

// sponsorEntry is the "entry" value of the audit rows (session.attach, ui.open) of a service identity's
// sponsor entering one of its runs.
const sponsorEntry = "sponsor"

// serviceOfRun is the name of the service identity that created run.
func serviceOfRun(run types.AgentRun) (string, bool) {
	return strings.CutPrefix(run.CreatedBy, types.ServicePrincipalPrefix)
}

// sponsoredService is the service identity a session that passed the entry gate on run entered as the sponsor of,
// or "": a service identity's run is entered only through the sponsor arm (mayEnterRunOrSponsor).
func sponsoredService(run types.AgentRun) string {
	name, _ := serviceOfRun(run)
	return name
}

// sponsorEntryData is the data a sponsor's entry rows add to their own: the service identity and
// entry=sponsor. A service identity's run is entered only through the sponsor arm (its owner is a
// principal no session or key holds), so the run alone says which entry this is; nil for any other run.
func sponsorEntryData(run types.AgentRun) map[string]any {
	name, ok := serviceOfRun(run)
	if !ok {
		return nil
	}
	return map[string]any{"service_identity": name, "entry": sponsorEntry}
}

// withSponsorEntry adds sponsorEntryData to data, in place, and returns it.
func withSponsorEntry(run types.AgentRun, data map[string]any) map[string]any {
	maps.Copy(data, sponsorEntryData(run))
	return data
}

// sponsorStillEntitled is the structural part of the sponsor arm of entry: principal is still the current
// sponsor of the service identity that created run, the identity is active, and it has not been
// re-sponsored since `since` (the ticket's or the session's authority time). Asked at ticket mint, ticket
// redemption and every UI re-check. The grant half (service_attach) is asked at the mint only: a withdrawn
// grant refuses the next ticket and ends nothing already open. A lookup that cannot answer is an error.
func (s *Server) sponsorStillEntitled(ctx context.Context, run types.AgentRun, principal string, since time.Time) (bool, error) {
	name, ok := serviceOfRun(run)
	if !ok || principal == "" || isServicePrincipal(principal) {
		return false, nil
	}
	ss, ok := s.cfg.Store.(store.ServiceIdentityStore)
	if !ok {
		return false, nil
	}
	still, err := ss.ServiceStillSponsored(ctx, name, principal, since)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return still, err
}

// mayEnterRunOrSponsor is mayEnterRun for a redeemed ticket or an open UI session, with the sponsor arm: a
// member-role principal who is still the sponsor of the service identity that owns run. A super admin's
// stamp never takes the arm (a sponsor's ticket is stamped member whatever the minter's role, so the run's
// own doors always run). A store failure refuses and is logged.
func (s *Server) mayEnterRunOrSponsor(ctx context.Context, run types.AgentRun, principal string, superAdmin bool, since time.Time) bool {
	if mayEnterRun(run, principal, superAdmin) {
		return true
	}
	if superAdmin {
		return false
	}
	ok, err := s.sponsorStillEntitled(ctx, run, principal, since)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: sponsor entry check failed; refusing", "run_id", run.ID, "err", err)
	}
	return ok && err == nil
}

// runOwnerOnlyReason is the audit reason an entry refusal carries on the lanes
// that keep their own trail beside authz.denied (session.attach, ssh.authenticate).
const runOwnerOnlyReason = string(authz.ReasonRunOwnerOnly)

// refuseRunOwnerOnly answers a super admin who may not enter run: a 403 naming
// why (not the 404 a member gets: the admin can already see the run), audited
// under run_owner_only.
func (s *Server) refuseRunOwnerOnly(w http.ResponseWriter, r *http.Request, run types.AgentRun) {
	s.refuse(w, r, authz.Deny(authz.ReasonRunOwnerOnly, run.ID.String(), "").OnRun(run.ID))
}

// getRunForEntry loads a run for a route that writes into a live terminal. The
// 404s are getRunAuthorizedBy's own (a security admin or a member cannot tell a
// foreign run from a missing one); a super admin who is not the owner of a
// personally-owned run gets the 403.
func (s *Server) getRunForEntry(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.AgentRun, bool) {
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return types.AgentRun{}, false
	}
	if !mayEnterRun(run, principalFromRequest(r), s.isOperator(r.Context())) {
		s.refuseRunOwnerOnly(w, r, run)
		return types.AgentRun{}, false
	}
	return run, true
}

// entryVerdict is why a redeemed ticket was refused; its string is the audit
// reason, and verdictOK is "".
type entryVerdict string

const (
	verdictOK entryVerdict = ""
	// verdictInvalid: no such ticket, expired, another run's, or one written
	// before the authority columns existed.
	verdictInvalid entryVerdict = "invalid"
	verdictRevoked entryVerdict = uiDeniedReasonRevoked
	// verdictRevocationUnavailable and verdictDelegationUnavailable: the check
	// could not be made. Retryable, never reported as a bad ticket.
	verdictRevocationUnavailable entryVerdict = uiDeniedReasonRevocationUnavailable
	verdictDelegationEnded       entryVerdict = uiDeniedReasonDelegationEnded
	verdictDelegationUnavailable entryVerdict = uiDeniedReasonDelegationUnavailable
)

// unavailable reports a verdict that is a failed lookup rather than a refusal.
func (v entryVerdict) unavailable() bool {
	return v == verdictRevocationUnavailable || v == verdictDelegationUnavailable
}

// redeemAttachTicket is the one redemption of an attach ticket, for the terminal
// WebSocket and the UI gateway alike (#1474, #1475). The ticket is burned first,
// before any check. It is then refused when it carries no authority time (a row
// from before the column: a zero time never means exempt), when the person's
// session was revoked since it was admitted, and when it came through a portal
// whose grant has ended. A non-nil error is a store failure on the consume
// itself; a lookup that could not answer is an unavailable verdict.
//
// The returned actor is empty for verdictInvalid and names the ticket's
// principal for every other verdict, so the caller audits who was refused.
func (s *Server) redeemAttachTicket(ctx context.Context, tok string, runID uuid.UUID) (ticketActor, entryVerdict, error) {
	ta, ok, err := consumeAttachTicket(ctx, s.cfg.Store, tok, runID, s.cfg.Now())
	if err != nil {
		return ticketActor{}, verdictOK, err
	}
	if !ok || ta.authorizedAt.IsZero() {
		return ticketActor{}, verdictInvalid, nil
	}
	if s.cfg.SessionRevocations != nil {
		revoked, err := s.cfg.SessionRevocations.IsSessionRevoked(ctx, ta.principal, ta.email, ta.authorizedAt)
		if err != nil {
			slog.ErrorContext(ctx, "wardynd: attach ticket revocation lookup failed", "run_id", runID, "err", err)
			return ta, verdictRevocationUnavailable, nil
		}
		if revoked {
			return ta, verdictRevoked, nil
		}
	}
	if ta.via != nil {
		expires, v := s.delegationLive(ctx, *ta.via)
		if v != verdictOK {
			return ta, v, nil
		}
		ta.grantExpires = expires
	}
	return ta, verdictOK, nil
}

// delegationLive asks whether the portal grant a session came through still
// stands: unexpired and its portal not revoked, resolved by id (#1475). It
// returns the grant's expiry. A store that is not a DelegateStore, and a lookup
// that fails, are unavailable and refuse: the delegated lane's own fall-through
// for a store without the capability (delegation.go) is right for a bearer that
// never was one, and wrong for a session that provably was. An absent grant, or
// one under another portal, has ended.
func (s *Server) delegationLive(ctx context.Context, via types.DelegationVia) (time.Time, entryVerdict) {
	ds, ok := s.cfg.Store.(store.DelegateStore)
	if !ok {
		slog.ErrorContext(ctx, "wardynd: a portal-derived session cannot be checked: the store has no portal capability")
		return time.Time{}, verdictDelegationUnavailable
	}
	t, err := ds.GetDelegatedTokenByID(ctx, via.Grant, s.cfg.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return time.Time{}, verdictDelegationEnded
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: portal grant lookup failed", "grant", via.Grant, "err", err)
		return time.Time{}, verdictDelegationUnavailable
	}
	if t.DelegateID != via.Delegate {
		return time.Time{}, verdictDelegationEnded
	}
	return t.ExpiresAt, verdictOK
}

// sessionExpiry is when a UI session opened from this ticket ends: the
// configured lifetime, or the portal grant's expiry if that comes first.
func (a ticketActor) sessionExpiry(now time.Time, ttl time.Duration) time.Time {
	exp := now.Add(ttl)
	if a.via != nil && a.grantExpires.Before(exp) {
		return a.grantExpires
	}
	return exp
}

// refuseTicketResponse writes the answer to a refused redemption. Every refusal
// but an unavailable lookup is the bad-ticket 403 byte for byte, so the response
// is no oracle for which check refused; a lookup that could not answer is a
// retryable 503.
func refuseTicketResponse(w http.ResponseWriter, v entryVerdict) {
	if v.unavailable() {
		writeErrorReason(w, http.StatusServiceUnavailable, string(v), uiSessionUnverifiableMsg)
		return
	}
	writeErrorReason(w, http.StatusForbidden, reasonUIGatewayTicketInvalid, "invalid, expired, or already-used attach ticket")
}

// refuseAttachTicket is the terminal WebSocket's refusal of a redemption: the
// audit row, then the response.
func (s *Server) refuseAttachTicket(w http.ResponseWriter, r *http.Request, runID uuid.UUID, ta ticketActor, v entryVerdict) {
	if v == verdictInvalid {
		if s.authFailedAllowed() {
			s.auditAttachDenied(r, runID, "unknown", "invalid, expired, or already-used attach ticket")
		}
	} else if s.authFailedLimiter.allow(s.cfg.Now()) {
		s.auditAttachRefused(r, runID, ta.principal, "ticket", string(v), ta.via)
	}
	refuseTicketResponse(w, v)
}

// refuseUITicket is the UI gateway's: the same, as ui.authorize/denied.
func (s *Server) refuseUITicket(w http.ResponseWriter, runID uuid.UUID, app string, ta ticketActor, v entryVerdict) {
	switch {
	case v == verdictInvalid:
		if s.authFailedAllowed() {
			s.auditUI(&runID, types.ActorHuman, "unknown", "ui.authorize", app, "denied",
				map[string]any{"reason": "invalid, expired, or already-used ticket"})
		}
	case s.authFailedLimiter.allow(s.cfg.Now()):
		s.auditUI(&runID, types.ActorHuman, ta.principal, "ui.authorize", app, "denied",
			ta.withVia(map[string]any{"reason": string(v)}))
	}
	refuseTicketResponse(w, v)
}
