// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Attach tickets: browsers cannot set an Authorization header on a WebSocket
// handshake, so in admin-token auth mode (no OIDC session cookie) the attach
// WS was unreachable from the UI. The standard fix: the UI first POSTs
// /runs/{id}/attach/ticket through the NORMAL authenticated surface, receives
// a single-use, 30s-TTL random ticket bound to that run and to the minting
// principal, and presents it as ?ticket= on the WS handshake. The ticket is
// consumed on first use (a reconnect mints a fresh one), so a leaked ticket is
// worthless after connect and worthless everywhere after 30s. Attribution is
// preserved: the session.attach audit names the MINTING principal, never an
// anonymous ticket.

// attachTicketTTL bounds how long a minted ticket is redeemable. Long enough
// for the immediate connect that follows the mint; short enough that a ticket
// captured from a log or referrer is stale by the time anyone reads it.
const attachTicketTTL = 30 * time.Second

// The outstanding-ticket table is a Postgres row per ticket (migration 0026),
// not process memory: a ticket minted on one control plane is redeemable on
// another, and a restart no longer invalidates every outstanding ticket.

// mintAttachTicket issues a fresh single-use ticket bound to runID, to the
// minting principal, and to that principal's role (admin/member) at mint time
// — the WS attach route's ?ticket= lane bypasses humanOrAdminAuth entirely, so
// this stamped role is the only signal available to re-check the owner rule
// when the ticket is consumed (see attach.go's handleAttachWS).
//
// now is the time the minting request was ADMITTED, and it is stamped on the
// ticket as the authority time (#1474): redemption asks whether a revoke landed
// since. The email is the verified identity's, from ctx — "" on the admin-token
// and local lanes — because a revoke may name the email instead of the subject.
func mintAttachTicket(ctx context.Context, st store.Store, runID uuid.UUID, actorType types.ActorType, principal, role string, now time.Time) (string, error) {
	var via *types.DelegationVia
	if v, ok := audit.DelegationFrom(ctx); ok {
		via = &v
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	t := store.AttachTicket{RunID: runID, ActorType: actorType, Principal: principal, Role: role, Via: via,
		AuthorizedAt: now.UTC(), Email: oidcEmailFromContext(ctx)}
	if err := st.MintAttachTicket(ctx, tok, t, now, now.Add(attachTicketTTL)); err != nil {
		return "", err
	}
	return tok, nil
}

// consumeAttachTicket redeems tok for runID exactly once, returning the minting
// principal for attribution. The store's DELETE ... RETURNING burns the row on
// ANY redemption attempt, so a probe against a guessed run id spends the ticket
// (what the in-memory map did). A miss, an expired row, and a run-id mismatch
// all fail identically — no oracle distinguishing "wrong run" from "no such
// ticket". A non-nil error is a store failure, NOT a rejection: the caller must
// not report it as a bad ticket.
func consumeAttachTicket(ctx context.Context, st store.Store, tok string, runID uuid.UUID, now time.Time) (ticketActor, bool, error) {
	t, ok, err := st.ConsumeAttachTicket(ctx, tok, now)
	if err != nil {
		return ticketActor{}, false, err
	}
	if !ok || t.RunID != runID {
		return ticketActor{}, false, nil
	}
	return ticketActor{actorType: t.ActorType, principal: t.Principal, role: t.Role, via: t.Via,
		authorizedAt: t.AuthorizedAt, email: t.Email}, true, nil
}

// ticketActorCtxKey carries the ticket's minting principal through to
// actorFromRequest so the session.attach/detach audit names the human who
// minted the ticket (invariant 4), not "admin-token".
type ticketActorCtxKey struct{}

type ticketActor struct {
	actorType types.ActorType
	principal string
	// role is the minting principal's role (oidc.RoleAdmin / oidc.RoleUser) at
	// mint time, stamped by handleAttachTicket. It is the ONLY role source
	// available in the ?ticket= WS lane (ticketOrHumanAuth bypasses
	// humanOrAdminAuth for it entirely) — see handleAttachWS's owner
	// re-check.
	role string
	// via is the portal and delegated token the ticket was minted through
	// (#1142), nil otherwise. withTicketActor replays it as the delegated
	// context, so recordAudit stamps data.via on the ticket lane's rows and
	// isOperator refuses it there as it did at mint.
	via *types.DelegationVia
	// authorizedAt and email are the authority the ticket was minted under
	// (#1474), and grantExpires the portal grant's expiry on a delegated ticket
	// (#1475, set by redeemAttachTicket). authorizedAt is zero on a row written
	// before the column existed, which redemption refuses.
	authorizedAt time.Time
	email        string
	grantExpires time.Time
}

func withTicketActor(ctx context.Context, a ticketActor) context.Context {
	if a.via != nil {
		ctx = audit.WithDelegation(ctx, *a.via)
	}
	return context.WithValue(ctx, ticketActorCtxKey{}, a)
}

// withVia adds the ticket's via to an audit datum written outside the
// request context (the UI gateway audits on BaseCtx), nil-safe.
func (a ticketActor) withVia(data map[string]any) map[string]any {
	if a.via != nil {
		data["via"] = a.via
	}
	return data
}

func ticketActorFromContext(ctx context.Context) (ticketActor, bool) {
	a, ok := ctx.Value(ticketActorCtxKey{}).(ticketActor)
	return a, ok
}

// handleAttachTicket mints a single-use WS ticket:
//
//	POST /api/v1/runs/{id}/attach/ticket
//
// Mounted INSIDE the humanOrAdminAuth group (getRunAuthorized), then narrowed to
// the owner (mayEnterRun): a run's OWNER may mint a ticket for their own run; a
// super admin only for a run with no personal owner, and otherwise gets a 403
// run_owner_only. A member who did not create this run gets the byte-identical
// 404 a missing run would (no existence oracle). The minted ticket carries the minter's OWN
// role, which is the ONLY authorization signal the WS handler has left to
// check at consume time, since the ?ticket= lane bypasses humanOrAdminAuth
// entirely.
func (s *Server) handleAttachTicket(w http.ResponseWriter, r *http.Request) {
	// The ticket's authority time, taken before any store call so nothing the
	// client can stretch falls before it (the same convention as apitokens.go).
	authorizedAt := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	// Interactive entry needs the run's owner (mayEnterRun, run_entry.go), on top
	// of getRunAuthorized: its ownsRunOrAdmin passes a SECURITY ADMIN on any run,
	// deliberately, for incident response, and a super admin too. Minting is the
	// one route under that predicate that is not inspect-or-stop: this ticket
	// becomes a live interactive PTY or UI session inside the owner's sandbox,
	// holding the owner's personal connections.
	//
	// A super admin on a run that is not theirs, and not an operator-owned one,
	// gets a 403 naming why (#1476): they can already see the run, so there is
	// no existence to hide. Everyone else keeps the byte-identical 404 the
	// foreign-member deny writes — no existence oracle, including its wire
	// reason (AsIf, #656 slice 3) — audited under its OWN reason, so an auditor
	// can see a security admin refused a foreign PTY without inferring it from
	// the path. A delegated token is never a super admin here (isOperator is
	// false under one), so a portal cannot mint for a run its person may not enter.
	if !mayEnterRun(run, principalFromRequest(r), s.isOperator(r.Context())) {
		if s.isOperator(r.Context()) {
			s.refuseRunOwnerOnly(w, r, run)
			return
		}
		s.refuse(w, r, authz.Deny(authz.ReasonAttachTicketForeignRun, run.ID.String(), "run not found").OnRun(run.ID).AsIf(authz.Reason(reasonRunNotFound)))
		return
	}
	// Same fail-closed gate as the WS itself: a ticket for a non-attachable run
	// is useless, so refuse to mint one (clean 409 now beats a WS error later).
	if run.State != types.RunRunning {
		writeErrorReason(w, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; cannot attach (state="+string(run.State)+")")
		return
	}
	if runIsKept(run) {
		writeErrorReason(w, http.StatusConflict, reasonAttachRunKept, "run has ended; cannot attach")
		return
	}
	at, principal := actorFromRequest(r)
	// DELIBERATELY isOperator — this role field means exactly "reaches runs its
	// holder does not own" (attach.go's == oidc.RoleAdmin consume check), never
	// the minter's session tier, so a security admin's ticket stamps member.
	// See the three-tier doctrine on internal/auth/oidc's RoleSecurityAdmin;
	// the API-token stamp (apitokens.go) is the ONE snapshot site that moved,
	// because a token carries a whole session identity rather than run reach.
	role := oidc.RoleUser
	if s.isOperator(r.Context()) {
		role = oidc.RoleAdmin
	}
	// A revoke that landed while this request was in flight must not leave a
	// ticket behind: the same late re-check as handleCreateAPIToken, and the same
	// fail-closed answer when it cannot be made.
	if s.cfg.SessionRevocations != nil {
		revoked, rerr := s.cfg.SessionRevocations.IsSessionRevoked(r.Context(), principal, oidcEmailFromContext(r.Context()), authorizedAt)
		if rerr != nil {
			writeServerError(w, r, "mint attach ticket", rerr)
			return
		}
		if revoked {
			writeErrorReason(w, http.StatusForbidden, reasonAttachTicketNotYourRun, "this session is no longer authorized")
			return
		}
	}
	tok, err := mintAttachTicket(r.Context(), s.cfg.Store, id, at, principal, role, authorizedAt)
	if err != nil {
		slog.ErrorContext(r.Context(), "wardynd: mint attach ticket failed", "run_id", id, "err", err)
		writeErrorReason(w, http.StatusInternalServerError, reasonAttachTicketMintFailed, "mint attach ticket failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":             tok,
		"expires_in_seconds": int(attachTicketTTL / time.Second),
	})
}

// ticketOrHumanAuth guards the attach WS route: a valid ?ticket= (single-use,
// unexpired, bound to this run id) authenticates on its own and stamps the
// minting principal (AND role) for attribution; anything else falls
// through to the standard humanOrAdminAuth middleware (OIDC session / admin
// token / local mode). A PRESENT-but-invalid ticket fails closed with 403
// rather than falling through — a caller that chose ticket auth gets a crisp
// answer, never a silent downgrade to cookie auth.
//
// The two lanes are NOT symmetric, and get there differently.
// The FALL-THROUGH lane stays admin-only (s.requireOperator): humanOrAdminAuth
// only AUTHENTICATES, so a member who never minted a ticket must not simply
// omit ?ticket= and get the same live PTY straight from their session cookie
// (which browsers attach to a same-origin WebSocket handshake automatically).
// The TICKET lane is owner-only (a super admin only on a run with no personal
// owner): minting is itself gated on that rule (POST /runs/{id}/attach/ticket,
// mayEnterRun), so holding a ticket at all already proves that much — but the
// handler (handleAttachWS, attach.go) ALSO re-checks the ticket's own stamped
// role/principal against the run it names, since this lane never runs
// humanOrAdminAuth/requireOperator at all and the ticket's stamped data is the
// only signal left to check. Redemption (redeemAttachTicket, run_entry.go) also
// refuses a ticket admitted before a revoke or minted through a portal whose
// grant has since ended.
func (s *Server) ticketOrHumanAuth(next http.Handler) http.Handler {
	human := s.humanOrAdminAuth(s.requireOperator(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("ticket")
		if tok == "" {
			human.ServeHTTP(w, r)
			return
		}
		id, ok := parseIDParam(w, r, "id", "run")
		if !ok {
			return
		}
		ta, v, err := s.redeemAttachTicket(r.Context(), tok, id)
		if err != nil {
			// A store failure is not a bad ticket: say so, and never leak the
			// database error to an as-yet-unauthenticated caller — but DO log it,
			// or the operator sees a bare 500 with no cause anywhere.
			slog.ErrorContext(r.Context(), "wardynd: attach ticket lookup failed", "run_id", id, "err", err)
			writeErrorReason(w, http.StatusInternalServerError, reasonUIGatewayTicketLookupFailed, "attach ticket lookup failed")
			return
		}
		if v != verdictOK {
			s.refuseAttachTicket(w, r, id, ta, v)
			return
		}
		next.ServeHTTP(w, r.WithContext(withTicketActor(r.Context(), ta)))
	})
}

// auditAttachDenied records an authorization REFUSAL in the ?ticket= attach
// lane. The sibling SSH gateway audits every one of its rejections (ssh.authenticate
// failure, sshgateway.go) precisely so a scan against it leaves a trail; this
// lane audited none of its own, so ticket-probing the WebSocket route was
// invisible in the system of record — the one lane where that matters most,
// since it is the only route reaching a live PTY without ever running
// humanOrAdminAuth.
//
// actor is the ticket's stamped principal where the ticket resolved and only
// AUTHORIZATION failed, and "unknown" where the ticket itself did not — the
// same distinction sshAuditAuthFailure draws, for the same reason: naming a
// principal the caller never proved is worse than naming none.
func (s *Server) auditAttachDenied(r *http.Request, runID uuid.UUID, actor, reason string) {
	s.auditAttachRefused(r, runID, actor, "ticket", reason, nil)
}

// auditAttachRefused is auditAttachDenied for either lane, with the portal the
// refused credential came through when it came through one.
func (s *Server) auditAttachRefused(r *http.Request, runID uuid.UUID, actor, lane, reason string, via *types.DelegationVia) {
	data := map[string]any{"lane": lane, "reason": reason}
	if via != nil {
		data["via"] = via
	}
	ev := s.auditEvent(&runID, types.ActorHuman, actor, "session.attach", runID.String(), "failure", mustJSON(data))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}
