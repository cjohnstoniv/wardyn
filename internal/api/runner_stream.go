// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type runnerStreamState struct {
	once    sync.Once
	hub     *remote.Hub
	limiter principalLimiter
	runners principalLimiter
	subsMu  sync.Mutex
	subs    map[uuid.UUID]runnerEndpoint
	// owners is each runner owner's slots, under subsMu.
	owners  map[string]ownerSlots
	offline runnerOfflineSweeps
	relay   relayListener
}

// This route has its own proof-of-possession handshake. Human sessions and
// admin bearer tokens never authorize a runner or replace its AUTH signature.
// The address limiter charges every connection before any read and refunds a
// runner that proves its key: behind a proxy every runner shares one address.
// Each verified runner's reconnects are limited by its own id.
func (s *Server) mountRunnerStreamRoutes(r chi.Router) {
	s.runnerStreams.limiter = principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers}
	s.runnerStreams.runners = principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers}
	r.Get("/runners/{id}/stream", s.handleRunnerStream)
}

// runnerPeerKey carries the request's address key, which Verified refunds.
type runnerPeerKey struct{}

var errRunnerStreamRateLimited = errors.New("runner stream connection rate exceeded")

func (s *Server) handleRunnerStream(w http.ResponseWriter, r *http.Request) {
	peer := peerKey(r.RemoteAddr)
	if !s.runnerStreams.limiter.allow(peer, s.cfg.Now()) {
		writeErrorReason(w, http.StatusTooManyRequests, reasonRunnerStreamRateLimited, errRunnerStreamRateLimited.Error())
		return
	}
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	if federation.CheckOrgURL(s.cfg.RunnerOrgURL) != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunnerStreamUnavailable, "runner stream unavailable: set WARDYN_RUNNER_ORG_URL to the public HTTPS organisation URL")
		return
	}
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerStreamUnavailable, "runner stream requires the Postgres runner store")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil || id.String() != chi.URLParam(r, "id") {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidIDParam, "invalid runner id")
		return
	}
	hub := s.runnerHub(st)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	// Serve owns the connection, always closes it, and cannot Attach a Link
	// until nonce, stored key, route id and the admitted Substrate all agree.
	_ = hub.Serve(context.WithValue(r.Context(), runnerPeerKey{}, peer), runnerwire.NewWebSocket(conn), id)
}

// RevokeRunnerSession notifies an already connected runner after the caller
// commits revocation to RunnerStore. H3/H10 own that transaction and any run
// lifecycle consequences; this closes only the authenticated transport.
func (s *Server) RevokeRunnerSession(ctx context.Context, id uuid.UUID) error {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return nil
	}
	// Initializing the hub here avoids racing a first connection with revoke.
	return s.runnerHub(st).Revoke(ctx, id)
}

// RunnerOnline reports whether this process holds the runner's live session,
// admitted to READY.
func (s *Server) RunnerOnline(id uuid.UUID) bool {
	st, ok := s.cfg.Store.(store.RunnerStore)
	return ok && s.runnerHub(st).Online(id)
}

// RunnersDisabled ends every runner session this process holds once runners
// are switched off. Each reconnect then meets the stream route's switch check,
// and a create already admitted meets the one on every dispatch.
func (s *Server) RunnersDisabled(ctx context.Context) {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return
	}
	if err := s.runnerHub(st).GoawayAll(ctx); err != nil {
		slog.WarnContext(ctx, "wardynd: ending runner sessions after runners were switched off", slog.Any("err", err))
	}
}

// defaultRunnerHooks wires the runners routes' online read and switch-off hook
// to this process's Hub unless the caller injected its own.
func (s *Server) defaultRunnerHooks() {
	if s.cfg.RunnerOnline == nil {
		s.cfg.RunnerOnline = s.RunnerOnline
	}
	if s.cfg.RunnersDisabled == nil {
		s.cfg.RunnersDisabled = s.RunnersDisabled
	}
}

// runnerOwnerInactive says the runner's owner is deactivated or purged (a SCIM suspension): the
// identity read the token and SSH-key lanes make. The runner stays claimed, so a reactivation
// readmits it with no re-claim.
func (s *Server) runnerOwnerInactive(ctx context.Context, rn types.Runner) (bool, error) {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return false, nil
	}
	return st.IdentityBlocked(ctx, rn.Owner, -1)
}

// runnerHub builds the one Hub lazily. Callbacks not injected are the durable
// org composition: admitRunner and the asynchronous offline sweep.
func (s *Server) runnerHub(st store.RunnerStore) *remote.Hub {
	s.runnerStreams.once.Do(func() {
		cfg := s.cfg.RunnerHub
		cfg.OrgURL, cfg.Lookup = s.cfg.RunnerOrgURL, st.GetRunner
		cfg.Verified = func(ctx context.Context, id uuid.UUID) error {
			if !s.runnerStreams.runners.allow(id.String(), s.cfg.Now()) {
				// The runner sees only a closed socket; this line names the cause.
				slog.WarnContext(ctx, "wardynd: runner stream reconnect refused by the per-runner rate limit", slog.String("runner_id", id.String()))
				return errRunnerStreamRateLimited
			}
			if peer, ok := ctx.Value(runnerPeerKey{}).(string); ok {
				s.runnerStreams.limiter.refund(peer, s.cfg.Now())
			}
			return nil
		}
		if cfg.Admit == nil {
			cfg.Admit = s.admitRunner
		}
		if cfg.Inactive == nil {
			cfg.Inactive = s.runnerOwnerInactive
		}
		// The Hub reports Disconnected only for a session it reported Connected,
		// and calls both under the runner's session slot, Disconnected even inside
		// a fresh admission. Each writes its audit row there, so a new session's
		// runner.connect never precedes the last one's runner.disconnect. Both read
		// the runner row and write the audit row on that slot; the offline sweep
		// is only scheduled there and never runs on it.
		if cfg.Connected == nil && cfg.Disconnected == nil {
			cfg.Connected = func(ctx context.Context, id uuid.UUID) error {
				rn, err := st.GetRunner(ctx, id)
				if err != nil {
					return err
				}
				s.RecordRunnerConnected(ctx, id, rn.Version, false)
				return nil
			}
			cfg.Disconnected = func(ctx context.Context, id uuid.UUID) error {
				var lastSeen *time.Time
				if rn, err := st.GetRunner(ctx, id); err == nil {
					lastSeen = rn.LastSeenAt
				}
				s.RecordRunnerDisconnected(ctx, id, lastSeen)
				s.runnerStreams.offline.schedule(s.cfg.BaseCtx, id, s.OfflineFromRunner)
				return nil
			}
		}
		s.runnerStreams.hub = remote.NewHub(s.cfg.BaseCtx, cfg)
	})
	return s.runnerStreams.hub
}
