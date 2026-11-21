// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The service grant refresher: its own goroutine on every replica, with or without a runner, so a token is
// warm before a run asks and a failure shows before the token it would have replaced expires. It shares
// nothing with the run watcher, so a token endpoint that hangs delays no run-lease or proxy-expiry pass.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	serviceGrantPassEvery   = time.Minute
	serviceGrantPassWorkers = 8
)

// StartServiceGrantRefresher starts the refresher and returns at once; it stops when ctx ends. cmd/wardynd
// starts it on every replica, never through the leader-only runner.
func (s *Server) StartServiceGrantRefresher(ctx context.Context) {
	s.goBackground(func() { s.runServiceGrantRefresher(ctx, serviceGrantPassEvery) })
	s.goBackground(func() { s.runServiceGrantHeartbeat(ctx, serviceGrantPassEvery) })
}

// runServiceGrantHeartbeat keeps this replica's seen_at fresh and sweeps old status rows. It is its own loop,
// so a pass held by a slow token endpoint or a blocked store cannot make this replica look departed.
func (s *Server) runServiceGrantHeartbeat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.serviceGrantHeartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) serviceGrantHeartbeat(ctx context.Context) {
	gs, ok := s.serviceGrantStore()
	if !ok {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, serviceGrantStatusWriteBound)
	defer cancel()
	if err := gs.TouchServiceGrantReplica(hctx, s.svcGrant.replica()); err != nil {
		slog.WarnContext(ctx, "wardynd: could not mark this replica seen for service grants", slog.Any("err", err))
	}
	if _, err := gs.SweepServiceGrantStatuses(hctx); err != nil {
		slog.WarnContext(ctx, "wardynd: could not sweep stale service grant statuses", slog.Any("err", err))
	}
}

func (s *Server) runServiceGrantRefresher(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.serviceGrantPassSafely(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) serviceGrantPassSafely(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "wardynd: the service grant refresher's pass panicked", slog.Any("panic", r))
		}
	}()
	s.serviceGrantPass(ctx)
}

// serviceGrantPass is one refresher pass: it retires masks no grant names and attempts the rows that are due, at
// most 8 at once and one per row. The replica heartbeat is its own loop (runServiceGrantHeartbeat).
func (s *Server) serviceGrantPass(ctx context.Context) {
	// No store, or one that keeps no grants (a bare api.New): nothing can be configured, activated or minted.
	if s.cfg.Store == nil {
		return
	}
	if _, ok := s.serviceGrantStore(); !ok {
		return
	}
	// The mask reconciliation runs beside the attempts, never ahead of them: a name another request holds is
	// left to the next pass. The pass's reads take at most 5 s, each attempt at most 25 s, and the mask
	// reconciliation beside them at most 25 s.
	reconciled := make(chan struct{})
	go func() {
		defer close(reconciled)
		mctx, mcancel := context.WithTimeout(ctx, serviceGrantAttemptBound)
		defer mcancel()
		s.reconcileServiceGrantMasks(mctx)
	}()
	defer func() { <-reconciled }()
	due, ok := s.serviceGrantDueRows(ctx)
	if !ok {
		return
	}
	sem := make(chan struct{}, serviceGrantPassWorkers)
	var wg sync.WaitGroup
	for _, p := range due {
		if !s.beginServiceGrantAttempt(p.UID) {
			continue // still running from the last pass
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(p types.ModelProvider) {
			defer func() { <-sem; wg.Done(); s.endServiceGrantAttempt(p.UID) }()
			_, _ = s.serviceGrantToken(ctx, p, "", serviceGrantTriggerRefresh)
		}(p)
	}
	wg.Wait()
}

// serviceGrantDueRows reads the configuration and picks the due rows under one 5 s bound; ok is false when the
// configuration cannot be read, holds no rows, or the switch-on has not been made.
func (s *Server) serviceGrantDueRows(ctx context.Context) ([]types.ModelProvider, bool) {
	rctx, cancel := context.WithTimeout(ctx, serviceGrantStatusWriteBound)
	defer cancel()
	sc, err := s.cfg.Store.GetSiteConfig(rctx)
	if err != nil || sc.ModelProviders == nil {
		return nil, false
	}
	if active, aerr := s.serviceGrantsActive(rctx); aerr != nil || !active {
		return nil, false
	}
	var due []types.ModelProvider
	for _, p := range sc.ModelProviders.Providers {
		if p.OIDC != nil && p.OIDC.ServiceGrant != nil && !p.Disabled && s.serviceGrantDue(rctx, p) {
			due = append(due, p)
		}
	}
	return due, true
}

func (s *Server) beginServiceGrantAttempt(uid string) bool {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	if s.svcGrant.running == nil {
		s.svcGrant.running = map[string]bool{}
	}
	if s.svcGrant.running[uid] {
		return false
	}
	s.svcGrant.running[uid] = true
	return true
}

func (s *Server) endServiceGrantAttempt(uid string) {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	delete(s.svcGrant.running, uid)
}

// serviceGrantDue: this replica has no cached token for the row, the cached token is inside the reuse margin,
// the secret's revision differs from the cached token's (a rotation on a warm, idle row, due whatever the
// pacing), or the row's last attempt failed (attempted once a pass: the pacing binds a resolve or a dispatch only).
func (s *Server) serviceGrantDue(ctx context.Context, p types.ModelProvider) bool {
	digest := serviceGrantDigest(p)
	s.svcGrant.mu.Lock()
	c, cached := s.svcGrant.cache[p.UID]
	f, failed := s.svcGrant.failed[p.UID]
	s.svcGrant.mu.Unlock()
	switch {
	case !cached || c.digest != digest:
		return true
	case failed && f.digest == digest:
		return true
	case c.expiresAt.Sub(s.cfg.Now()) <= serviceGrantReuseMargin:
		return true
	}
	rev, ok, err := s.serviceGrantRevision(ctx, serviceGrantSecretName(p.UID))
	return err != nil || (ok && rev != c.rev)
}
