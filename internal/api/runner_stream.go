// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

type runnerStreamState struct {
	once    sync.Once
	hub     *remote.Hub
	limiter principalLimiter
}

// This route has its own proof-of-possession handshake. Human sessions and
// admin bearer tokens never authorize a runner or replace its AUTH signature.
func (s *Server) mountRunnerStreamRoutes(r chi.Router) {
	s.runnerStreams.limiter = principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers}
	r.Get("/runners/{id}/stream", s.handleRunnerStream)
}

func (s *Server) handleRunnerStream(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	if federation.CheckOrgURL(s.cfg.RunnerOrgURL) != nil {
		http.Error(w, "runner stream unavailable: set WARDYN_RUNNER_ORG_URL to the public HTTPS organisation URL", http.StatusServiceUnavailable)
		return
	}
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		http.Error(w, "runner stream requires the Postgres runner store", http.StatusNotImplemented)
		return
	}
	if !s.runnerStreams.limiter.allow(peerKey(r.RemoteAddr), s.cfg.Now()) {
		http.Error(w, "runner stream connection rate exceeded", http.StatusTooManyRequests)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil || id.String() != chi.URLParam(r, "id") {
		http.Error(w, "invalid runner id", http.StatusBadRequest)
		return
	}
	s.runnerStreams.once.Do(func() {
		cfg := s.cfg.RunnerHub
		cfg.OrgURL, cfg.Lookup = s.cfg.RunnerOrgURL, st.GetRunner
		s.runnerStreams.hub = remote.NewHub(s.cfg.BaseCtx, cfg)
	})
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	// Serve owns the connection, always closes it, and cannot Attach a Link
	// until nonce, stored key, route id and the admitted Substrate all agree.
	_ = s.runnerStreams.hub.Serve(r.Context(), runnerwire.NewWebSocket(conn), id)
}

// RevokeRunnerSession notifies an already connected runner after the caller
// commits revocation to RunnerStore. H3/H10 own that transaction and any run
// lifecycle consequences; this closes only the authenticated transport.
func (s *Server) RevokeRunnerSession(ctx context.Context, id uuid.UUID) error {
	// Initializing the hub here avoids racing a first connection with revoke.
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return nil
	}
	s.runnerStreams.once.Do(func() {
		cfg := s.cfg.RunnerHub
		cfg.OrgURL, cfg.Lookup = s.cfg.RunnerOrgURL, st.GetRunner
		s.runnerStreams.hub = remote.NewHub(s.cfg.BaseCtx, cfg)
	})
	return s.runnerStreams.hub.Revoke(ctx, id)
}
