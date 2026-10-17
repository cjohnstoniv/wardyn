// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
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
	global  principalLimiter
	runners principalLimiter
	subsMu  sync.Mutex
	subs    map[uuid.UUID]runnerEndpoint
	// owners is each runner owner's slots, under subsMu.
	owners  map[string]ownerSlots
	offline runnerOfflineSweeps
	relay   relayListener

	// handshakes counts connections admitted and not yet proved or closed.
	handshakes    atomic.Int32
	maxHandshakes int32
}

// Anonymous admission, before a runner proves its key, is two buckets and a
// cap, all per replica (an HA install's limits are these times the replica
// count): the address bucket (enrolBurst, then enrolRatePerSec, per client
// address), one global bucket shared by every address, and at most
// runnerStreamMaxHandshakes connections between arrival and proof. A runner
// that proves its key is refunded both buckets and frees its slot, so only
// failed and unfinished handshakes spend them; runnerwire's handshake deadline
// bounds how long one slot is held.
//
// Memory, per replica: a keyed bucket entry is under 128 bytes (measured about
// 90 with map overhead), so each keyed limiter stays under 512 KiB at
// enrolLimiterMaxPeers. A slot pins one connection's goroutine, HTTP buffers
// and WebSocket reader, under 64 KiB (measured about 46 KiB for both ends of a
// loopback pair), so the cap bounds unproved connections under 16 MiB.
const (
	runnerStreamGlobalBurst      = 512.0
	runnerStreamGlobalRatePerSec = 10.0
	runnerStreamMaxHandshakes    = 256
)

// This route has its own proof-of-possession handshake. Human sessions and
// admin bearer tokens never authorize a runner or replace its AUTH signature.
// Every connection is charged before any read. The runner-id bucket is charged
// only after Verified: a route id is a claim until the key proves it, so a
// caller who cannot prove it never spends that runner's reconnect budget.
func (s *Server) mountRunnerStreamRoutes(r chi.Router) {
	s.runnerStreams.limiter = principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers}
	s.runnerStreams.global = principalLimiter{rate: runnerStreamGlobalRatePerSec, burst: runnerStreamGlobalBurst, max: 1}
	s.runnerStreams.maxHandshakes = runnerStreamMaxHandshakes
	s.runnerStreams.runners = principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers}
	r.Get("/runners/{id}/stream", s.handleRunnerStream)
}

// runnerAdmissionKey carries the request's runnerAdmission to Verified.
type runnerAdmissionKey struct{}

// runnerAdmission is what one connection holds until its runner proves its key.
type runnerAdmission struct {
	peer    string
	release func()
}

var errRunnerStreamRateLimited = errors.New("runner stream connection rate exceeded")

// admitRunnerHandshake charges the anonymous budgets. ok false names the refusal.
func (s *Server) admitRunnerHandshake(peer string) (release func(), refusal string, ok bool) {
	rs, now := &s.runnerStreams, s.cfg.Now()
	if !rs.limiter.allow(peer, now) {
		return nil, errRunnerStreamRateLimited.Error(), false
	}
	// A refusal past the address bucket hands its token back, so a runner
	// retrying through a global or cap refusal keeps its own burst.
	if !rs.global.allow("", now) {
		rs.limiter.refund(peer, now)
		return nil, errRunnerStreamRateLimited.Error(), false
	}
	if rs.handshakes.Add(1) > rs.maxHandshakes {
		rs.handshakes.Add(-1)
		rs.limiter.refund(peer, now)
		return nil, "too many runner stream handshakes in progress", false
	}
	return sync.OnceFunc(func() { rs.handshakes.Add(-1) }), "", true
}

func (s *Server) handleRunnerStream(w http.ResponseWriter, r *http.Request) {
	peer := peerKey(r.RemoteAddr)
	release, refusal, ok := s.admitRunnerHandshake(peer)
	if !ok {
		writeErrorReason(w, http.StatusTooManyRequests, reasonRunnerStreamRateLimited, refusal)
		return
	}
	defer release()
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
	// A refused authentication (a lapsed, swept or rebound registration reads as one) is logged with its
	// cause: the runner is told nothing but a close, so this line is the only place an operator sees why.
	if err := hub.Serve(context.WithValue(r.Context(), runnerAdmissionKey{}, runnerAdmission{peer: peer, release: release}), runnerwire.NewWebSocket(conn), id); errors.Is(err, runnerwire.ErrAuth) {
		slog.WarnContext(r.Context(), "wardynd: runner authentication refused", slog.String("runner", id.String()), slog.Any("cause", err))
	}
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
// readmits it with no re-claim. An organization's runner names no person, so its standing is its own
// claim and revocation: inactive unless claimed and unrevoked.
func (s *Server) runnerOwnerInactive(ctx context.Context, rn types.Runner) (bool, error) {
	if rn.OwnerKind == types.RunnerOwnerOrg {
		return rn.Owner != "" || rn.State != types.RunnerClaimed || rn.RevokedAt != nil, nil
	}
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return false, nil
	}
	return st.IdentityBlocked(ctx, rn.Owner, -1)
}

// runnerProxyImage is the proxy image this organisation runs for rn's architecture, the same answer
// registration gave: the per-architecture image, else WARDYN_PROXY_IMAGE. "" when it names none.
func (s *Server) runnerProxyImage(rn types.Runner) string {
	return cmp.Or(s.cfg.ProxyImageByArch[rn.Arch], s.cfg.ProxyImage)
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
			if a, ok := ctx.Value(runnerAdmissionKey{}).(runnerAdmission); ok {
				s.runnerStreams.limiter.refund(a.peer, s.cfg.Now())
				s.runnerStreams.global.refund("", s.cfg.Now())
				a.release()
			}
			return nil
		}
		if cfg.Admit == nil {
			cfg.Admit = s.admitRunner
		}
		if cfg.Inactive == nil {
			cfg.Inactive = s.runnerOwnerInactive
		}
		if cfg.ProxyImage == nil {
			cfg.ProxyImage = s.runnerProxyImage
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
