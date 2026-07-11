// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

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

// attachTicket is one outstanding single-use WS credential.
type attachTicket struct {
	runID     uuid.UUID
	actorType types.ActorType
	principal string
	expiresAt time.Time
}

// attachTickets is the in-memory outstanding-ticket table. In-process state is
// correct here: a ticket is minted and redeemed against the same wardynd that
// holds the WS (there is exactly one control plane), and a restart merely
// invalidates outstanding tickets, which the UI handles by re-minting.
type attachTickets struct {
	mu sync.Mutex
	m  map[string]attachTicket
}

// mint sweeps expired entries and issues a fresh ticket for runID.
func (t *attachTickets) mint(runID uuid.UUID, actorType types.ActorType, principal string, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[string]attachTicket)
	}
	for k, v := range t.m {
		if now.After(v.expiresAt) {
			delete(t.m, k)
		}
	}
	t.m[tok] = attachTicket{runID: runID, actorType: actorType, principal: principal, expiresAt: now.Add(attachTicketTTL)}
	return tok, nil
}

// consume redeems tok for runID exactly once. A miss, an expired entry, or a
// run-id mismatch all fail identically (no oracle distinguishing "wrong run"
// from "no such ticket").
func (t *attachTickets) consume(tok string, runID uuid.UUID, now time.Time) (attachTicket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.m[tok]
	if !ok {
		return attachTicket{}, false
	}
	delete(t.m, tok) // single-use: burned on any redemption attempt
	if now.After(tk.expiresAt) || tk.runID != runID {
		return attachTicket{}, false
	}
	return tk, true
}

// ticketActorCtxKey carries the ticket's minting principal through to
// actorFromRequest so the session.attach/detach audit names the human who
// minted the ticket (invariant 4), not "admin-token".
type ticketActorCtxKey struct{}

type ticketActor struct {
	actorType types.ActorType
	principal string
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
// Mounted INSIDE the humanOrAdminAuth group — minting requires the same
// credential every other admin-gated action does; the ticket only ever
// DOWNGRADES that credential to "open this one run's PTY within 30s".
func (s *Server) handleAttachTicket(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), id)
	if notFoundIf(w, err, "run") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}
	// Same fail-closed gate as the WS itself: a ticket for a non-attachable run
	// is useless, so refuse to mint one (clean 409 now beats a WS error later).
	if run.State != types.RunRunning {
		writeError(w, http.StatusConflict, "run is not RUNNING; cannot attach (state="+string(run.State)+")")
		return
	}
	at, principal := actorFromRequest(r)
	tok, err := s.attachTix.mint(id, at, principal, s.cfg.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint attach ticket: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":             tok,
		"expires_in_seconds": int(attachTicketTTL / time.Second),
	})
}

// ticketOrHumanAuth guards the attach WS route: a valid ?ticket= (single-use,
// unexpired, bound to this run id) authenticates on its own and stamps the
// minting principal for attribution; anything else falls through to the
// standard humanOrAdminAuth middleware (OIDC session / admin token / local
// mode) exactly as before. A PRESENT-but-invalid ticket fails closed with 403
// rather than falling through — a caller that chose ticket auth gets a crisp
// answer, never a silent downgrade to cookie auth.
func (s *Server) ticketOrHumanAuth(next http.Handler) http.Handler {
	human := s.humanOrAdminAuth(next)
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
		tk, ok := s.attachTix.consume(tok, id, s.cfg.Now())
		if !ok {
			writeError(w, http.StatusForbidden, "invalid, expired, or already-used attach ticket")
			return
		}
		next.ServeHTTP(w, r.WithContext(withTicketActor(r.Context(), ticketActor{actorType: tk.actorType, principal: tk.principal})))
	})
}
