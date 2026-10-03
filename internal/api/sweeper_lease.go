// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"
)

// SweeperLease is the leader election the sweeps that must run once share
// (db.SweeperLeader). Join joins the current term; ok is false on a follower.
// The returned context ends when the lease is lost, and end must be called when
// the work is done, because the leader waits for every end before it releases
// its lock. Current reports whether epoch is still the durable one: false once
// a newer leader has acquired the lease.
type SweeperLease interface {
	Join() (ctx context.Context, epoch int64, end func(), ok bool)
	Current(ctx context.Context, epoch int64) (bool, error)
}

// pauseCompensateTimeout bounds the re-read and thaw that undo a freeze whose
// mark failed.
const pauseCompensateTimeout = 15 * time.Second

type leaseEpochKey struct{}

// beginLeaderSweep gates one pass of a sweep that must run on one replica. With
// no SweeperLease the pass runs as it always did. With one, a follower skips
// the pass, and the leader's pass gets a context that ends when the lease is
// lost and carries the epoch its writes are fenced by (leaseCurrent).
func (s *Server) beginLeaderSweep(ctx context.Context) (context.Context, func(), bool) {
	if s.cfg.SweeperLease == nil {
		return ctx, func() {}, true
	}
	lctx, epoch, end, ok := s.cfg.SweeperLease.Join()
	if !ok {
		return ctx, nil, false
	}
	ctx, cancel := context.WithCancel(context.WithValue(ctx, leaseEpochKey{}, epoch))
	stop := context.AfterFunc(lctx, cancel)
	return ctx, func() { stop(); cancel(); end() }, true
}

// leaseCurrent reports whether the pass in ctx still holds the newest lease
// epoch. A pass with no epoch (no SweeperLease) is always current. It fails
// closed: an epoch that cannot be read is not current, so a stale leader
// cannot start a pause through a database blip.
func (s *Server) leaseCurrent(ctx context.Context) bool {
	epoch, ok := ctx.Value(leaseEpochKey{}).(int64)
	if !ok || s.cfg.SweeperLease == nil {
		return true
	}
	cur, err := s.cfg.SweeperLease.Current(ctx, epoch)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: sweeper lease epoch unreadable; not writing", slog.Any("err", err))
		return false
	}
	return cur
}
