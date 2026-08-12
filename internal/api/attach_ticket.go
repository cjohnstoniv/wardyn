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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Attach tickets: browsers cannot set an Authorization header on a WebSocket
// handshake, so in admin-token auth mode (no OIDC session cookie) the attach
// WS was unreachable from the UI. The standard fix: the UI first POSTs
// /runs/{id}/attach-ticket through the NORMAL authenticated surface, receives
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
// minting principal, and to that principal's role (admin/member) AT MINT TIME
// — the WS attach route's ?ticket= lane bypasses humanOrAdminAuth entirely, so
// this stamped role is the only signal available to re-check owner-or-admin
// when the ticket is consumed (see attach.go's handleAttachWS).
func mintAttachTicket(ctx context.Context, st store.Store, runID uuid.UUID, actorType types.ActorType, principal, role string, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	t := store.AttachTicket{RunID: runID, ActorType: actorType, Principal: principal, Role: role}
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
	return ticketActor{actorType: t.ActorType, principal: t.Principal, role: t.Role}, true, nil
}

// ticketActorCtxKey carries the ticket's minting principal through to
// actorFromRequest so the session.attach/detach audit names the human who
// minted the ticket (invariant 4), not "admin-token".
type ticketActorCtxKey struct{}

type ticketActor struct {
	actorType types.ActorType
	principal string
	// role is the minting principal's role (oidc.RoleAdmin / oidc.RoleMember) AT
	// MINT TIME, stamped by handleAttachTicket. It is the ONLY role source
	// available in the ?ticket= WS lane (ticketOrHumanAuth bypasses
	// humanOrAdminAuth for it entirely) — see handleAttachWS's owner-or-admin
	// re-check.
	role string
}

func withTicketActor(ctx context.Context, a ticketActor) context.Context {
	return context.WithValue(ctx, ticketActorCtxKey{}, a)
}

func ticketActorFromContext(ctx context.Context) (ticketActor, bool) {
	a, ok := ctx.Value(ticketActorCtxKey{}).(ticketActor)
	return a, ok
}

// handleAttachTicket mints a single-use WS ticket:
//
//	POST /api/v1/runs/{id}/attach-ticket
//
// Mounted INSIDE the humanOrAdminAuth group, owner-or-admin (getRunAuthorized):
// a run's OWNER may mint a ticket for their own run, same as an admin — a
// member who did not create this run gets the byte-identical 404 a missing run
// would (no existence oracle). The minted ticket carries the minter's OWN role
// (item 3), which is the ONLY authorization signal the WS handler has left to
// check at consume time, since the ?ticket= lane bypasses humanOrAdminAuth
// entirely.
func (s *Server) handleAttachTicket(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	// Same fail-closed gate as the WS itself: a ticket for a non-attachable run
	// is useless, so refuse to mint one (clean 409 now beats a WS error later).
	if run.State != types.RunRunning {
		writeError(w, http.StatusConflict, "run is not RUNNING; cannot attach (state="+string(run.State)+")")
		return
	}
	at, principal := actorFromRequest(r)
	role := oidc.RoleMember
	if s.isOperator(r.Context()) {
		role = oidc.RoleAdmin
	}
	tok, err := mintAttachTicket(r.Context(), s.cfg.Store, id, at, principal, role, s.cfg.Now())
	if err != nil {
		slog.ErrorContext(r.Context(), "wardynd: mint attach ticket failed", "run_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "mint attach ticket failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":             tok,
		"expires_in_seconds": int(attachTicketTTL / time.Second),
	})
}

// ticketOrHumanAuth guards the attach WS route: a valid ?ticket= (single-use,
// unexpired, bound to this run id) authenticates on its own and stamps the
// minting principal (AND role — item 3) for attribution; anything else falls
// through to the standard humanOrAdminAuth middleware (OIDC session / admin
// token / local mode). A PRESENT-but-invalid ticket fails closed with 403
// rather than falling through — a caller that chose ticket auth gets a crisp
// answer, never a silent downgrade to cookie auth.
//
// The two lanes are NOT symmetric since item 3, and get there differently.
// The FALL-THROUGH lane stays admin-only (s.requireOperator): humanOrAdminAuth
// only AUTHENTICATES, so a member who never minted a ticket must not simply
// omit ?ticket= and get the same live PTY straight from their session cookie
// (which browsers attach to a same-origin WebSocket handshake automatically).
// The TICKET lane is owner-or-admin: minting is itself owner-or-admin-gated
// (POST /runs/{id}/attach-ticket, getRunAuthorized), so holding a ticket at
// all already proves that much — but the handler (handleAttachWS, attach.go)
// ALSO re-checks the ticket's own stamped role/principal against the run it
// names, since this lane never runs humanOrAdminAuth/requireOperator at all
// and the ticket's stamped data is the only signal left to check.
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
		ta, ok, err := consumeAttachTicket(r.Context(), s.cfg.Store, tok, id, s.cfg.Now())
		if err != nil {
			// A store failure is not a bad ticket: say so, and never leak the
			// database error to an as-yet-unauthenticated caller — but DO log it,
			// or the operator sees a bare 500 with no cause anywhere.
			slog.ErrorContext(r.Context(), "wardynd: attach ticket lookup failed", "run_id", id, "err", err)
			writeError(w, http.StatusInternalServerError, "attach ticket lookup failed")
			return
		}
		if !ok {
			writeError(w, http.StatusForbidden, "invalid, expired, or already-used attach ticket")
			return
		}
		next.ServeHTTP(w, r.WithContext(withTicketActor(r.Context(), ta)))
	})
}
