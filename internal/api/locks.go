// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Cross-replica locks (internal/db/locks.go). The six that used to be
// in-process mutexes (the per-run operation lock, the site-config and
// capability-enforcement writers, the audit chain verify sweep, and the AWS SSO
// and Azure DevOps sign-in refresh single-flights) are taken here, in the one
// order db.LockOrder documents.
//
// No caller proceeds unlocked. A lock that cannot be taken is a refusal: an
// HTTP door answers 503 reasonLockUnavailable (retry), a redemption answers as
// it does for any transient failure, and a sweep skips the item. The context a
// lock returns is the one the guarded work must use: it is cancelled if the
// lock's connection is lost.

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// lockState is the in-process locker a store with no database falls back to.
type lockState struct {
	once  sync.Once
	local *db.LocalLocker
	// override replaces the store's locker. Set by tests only, to hand a server
	// a locker that refuses or has a one-connection pool.
	override db.Locker
}

// locker is the store's own (Postgres, shared by every replica over one
// database) when it has one, else this process's.
func (s *Server) locker() db.Locker {
	if s.locks.override != nil {
		return s.locks.override
	}
	if p, ok := s.cfg.Store.(interface{ Locker() db.Locker }); ok {
		if l := p.Locker(); l != nil {
			return l
		}
	}
	s.locks.once.Do(func() { s.locks.local = db.NewLocalLocker() })
	return s.locks.local
}

// lock waits for class's lock on parts (db.NewLockKey). The returned context
// is the guarded work's; unlock is for defer.
func (s *Server) lock(ctx context.Context, class int32, parts ...string) (context.Context, func(), error) {
	return s.locker().Lock(ctx, db.NewLockKey(class, parts...), db.LockWait)
}

// tryLock is lock that does not wait: ok is false when the lock is held. A
// refusal (no capacity, the database not answering) is ok=false too, logged:
// every caller is a sweep that retries on its next pass.
func (s *Server) tryLock(ctx context.Context, class int32, parts ...string) (context.Context, func(), bool) {
	lctx, unlock, ok, err := s.locker().TryLock(ctx, db.NewLockKey(class, parts...))
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not take a lock; skipping until the next pass", slog.Any("err", err))
		return ctx, nil, false
	}
	return lctx, unlock, ok
}

// lockDoor takes a lock for an HTTP handler and rebinds r to the lock's
// context, so every r.Context() read after it belongs to the guarded work.
// ok=false: the 503 is written and the handler returns.
func (s *Server) lockDoor(w http.ResponseWriter, r *http.Request, class int32, parts ...string) (*http.Request, func(), bool) {
	lctx, unlock, err := s.lock(r.Context(), class, parts...)
	if err != nil {
		writeLockRefused(w, r, err)
		return r, nil, false
	}
	return r.WithContext(lctx), unlock, true
}

// writeLockRefused answers a lock that could not be taken: 503 with a
// Retry-After for a refusal, 500 for the programming error of an out-of-order
// take.
func writeLockRefused(w http.ResponseWriter, r *http.Request, err error) {
	if !db.LockRefused(err) {
		writeServerError(w, r, "take a lock", err)
		return
	}
	slog.WarnContext(r.Context(), "api: a lock could not be taken; refusing the request",
		slog.String("path", r.URL.Path), slog.Any("err", err))
	w.Header().Set("Retry-After", "5")
	writeErrorReason(w, http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg)
}

const lockUnavailableMsg = "another operation on this resource is in progress, or the server cannot take its lock right now; retry in a moment"

// The per-run operation lock (#1480). Two paths in this daemon act on one run's
// proxy and broker credentials from a row they read earlier: a revive replaces
// the proxy it just claimed the run for, and the lease sweep re-asserts the
// stop of a kept run it listed before that claim. Neither check-then-act is
// atomic on its own, so a stale sweep could stop the proxy a revive had just
// started. Both hold this run's lock across the read and the runner call, and
// the sweep re-reads the row once it holds it.
//
// The lock is a Postgres advisory lock, so it holds across replicas. It is per
// run, never global, so two runs never wait on each other. Only the revive
// (reviveRunProxy) and the two sweeps that re-assert a destructive stop
// (leaseRun, keepRebootedRun) take it. It is NOT taken inside loseRun,
// stopLostSandbox, endSandbox, reconcileFinalize or stopKeptRun: they run under
// it. The sweep side only ever tries: a run whose lock is held, or that the
// lock pool cannot take now, is skipped and retried next pass.

// lockRunOp takes run id's lock, waiting for it.
func (s *Server) lockRunOp(ctx context.Context, id uuid.UUID) (context.Context, func(), error) {
	return s.lock(ctx, db.RunOpLockClass, id.String())
}

// tryLockRunOp is lockRunOp that gives up instead of waiting.
func (s *Server) tryLockRunOp(ctx context.Context, id uuid.UUID) (context.Context, func(), bool) {
	return s.tryLock(ctx, db.RunOpLockClass, id.String())
}
