// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// gtMinter is the slice of the identity provider the rotator needs (satisfied by
// *embedded.Provider / any identity.Provider).
type gtMinter interface {
	MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string, operatorOwned bool) (identity.RunIdentity, error)
}

// gtTokenWriter delivers one freshly minted sensor token: to the shared file on
// Compose (fileTokenWriter) or into the named Secret on Kubernetes
// (secretTokenWriter, gt_secret.go).
type gtTokenWriter func(ctx context.Context, token string) error

// fileTokenWriter writes the token to path atomically.
func fileTokenWriter(path string) gtTokenWriter {
	return func(_ context.Context, token string) error {
		if err := writeTokenFileAtomic(path, token); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	}
}

// gtRotateAfter is the rotator's wait, a seam so tests can see each delay.
var gtRotateAfter = time.After

// runGroundtruthTokenRotator keeps the sensor supplied with a FRESH host-sensor token
// (aud=wardyn-groundtruth). The eBPF/Tetragon ingest re-reads its token file on a
// 401, so with a live producer it recovers when its ~1h-TTL token expires instead of
// going permanently blind — the static WARDYN_GROUNDTRUTH_TOKEN env deployment could
// NOT refresh (a process env is fixed after exec), which is why the shipped stack went
// blind ~1h into any run. A file on a shared volume, or a Secret the sensor mounts,
// is the only refreshable wiring. Seeds immediately, then re-mints at ~half the
// remaining TTL (clamped [1m,30m]) so a missed tick never leaves an expired token.
// A failed mint or write retries in 1m. Best-effort per tick.
func runGroundtruthTokenRotator(ctx context.Context, m gtMinter, write gtTokenWriter) {
	// A write that keeps failing the same way (a Secret wardynd never creates and the chart left out) is
	// logged on the first failure and again when the error changes or the write recovers, not every minute.
	lastWriteErr := ""
	for {
		next := 30 * time.Minute
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ri, err := m.MintRunIdentity(mctx, groundtruthSensorRunID, groundtruthSensorSub, groundtruthSensorSub, groundtruthAudience, false)
		cancel()
		switch {
		case err != nil:
			slog.ErrorContext(ctx, "wardynd: groundtruth token rotate: mint failed", slog.Any("err", err))
			next = time.Minute // retry soon
		default:
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			werr := write(wctx, ri.Token)
			wcancel()
			if werr != nil {
				if werr.Error() != lastWriteErr {
					slog.ErrorContext(ctx, "wardynd: groundtruth token rotate: write failed (retrying every minute)", slog.Any("err", werr))
				}
				lastWriteErr = werr.Error()
				next = time.Minute
			} else {
				if lastWriteErr != "" {
					slog.InfoContext(ctx, "wardynd: groundtruth token rotate: write recovered")
					lastWriteErr = ""
				}
				if !ri.Expiry.IsZero() {
					if half := time.Until(ri.Expiry) / 2; half < time.Minute {
						next = time.Minute
					} else if half < 30*time.Minute {
						next = half
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-gtRotateAfter(next):
		}
	}
}

// groundtruthRotatorLockBackoff is how long a standby replica waits between
// attempts to take over the leader lock. Fixed, not configurable: the lock is
// uncontended almost always — a standby only wins when the leader's Postgres
// SESSION ends (usually its process exiting, but see below), which is not
// something an operator tunes per deployment.
const groundtruthRotatorLockBackoff = 30 * time.Second

// runGroundtruthTokenRotatorLeader gates runGroundtruthTokenRotator behind a
// cluster-wide leader election (S2). The token file lives on a volume SHARED
// across every wardynd replica (see deploy/compose's `groundtruth_token`
// volume), and the Secret is one object too, so every replica running the
// rotator unconditionally would all rewrite the same target — harmless (tokens are stateless ES256 JWT-SVIDs, so the damage
// is file thrash and duplicate mints, not invalidation) but wasteful. Only
// the lock holder mints/writes; every other replica parks on
// groundtruthRotatorLockBackoff and retries, taking over automatically once
// the holder's Postgres session ends.
//
// At most one steady-state leader — not exactly one, and NOT a fencing
// primitive. The advisory lock is SESSION-scoped and this loop never
// re-verifies it after the acquire, so any session loss short of process death
// (a Postgres restart, an RDS failover, pg_terminate_backend, an idle-session
// timeout, a network blip) silently releases it while the old leader keeps
// rotating; a standby then wins within one backoff. Two leaders, undetected.
// That is ACCEPTED, not overlooked: both write the identical harmless thing —
// mint a stateless token, atomically rename it into place — so a reader always
// sees one whole valid token, and the worst case is the same file thrash and
// duplicate mints as no leader election at all. Do NOT hang anything that needs
// real mutual exclusion on
// db.GroundTruthRotatorLockKey; it would inherit an exclusivity guarantee that
// is not there.
//
// tryLock is a seam so this stays unit-testable without Postgres, mirroring
// lifecycle.Config.TickLock's func-typed style; wardynd wires
// groundtruthRotatorLock(pool) (adapters.go), which calls
// db.TryAdvisoryLockDedicated(ctx, pool, db.GroundTruthRotatorLockKey). Unlike
// the reaper's TickLock (try/release every tick), this acquires ONCE and holds
// its own connection, outside the pool, for the process lifetime — the
// accepted cost of acquire-once.
func runGroundtruthTokenRotatorLeader(ctx context.Context, tryLock func(context.Context) (release func(), ok bool, err error), m gtMinter, write gtTokenWriter) {
	// Non-leader outcomes are logged on TRANSITION only: this loop retries every
	// 30s forever, so an unconditional line is 2,880/day/replica in the ordinary
	// standby steady state and the same again per replica during a Postgres
	// outage (the sibling reapTickLock drops its equivalent to Debug for exactly
	// this, adapters.go). Transition-logging keeps the ERROR loud the first time
	// — a rotator that never acquires leaves the ingest blind, which must not be
	// a Debug-level fact — without the repetition.
	logged := "" // last non-leader outcome already logged
	for {
		release, ok, err := tryLock(ctx)
		switch {
		case err != nil:
			if logged != "err" {
				logged = "err"
				slog.ErrorContext(ctx, "wardynd: groundtruth rotator: lock acquire failed", slog.Any("err", err))
			}
		case ok:
			slog.InfoContext(ctx, "wardynd: groundtruth rotator: acquired leader lock")
			runGroundtruthTokenRotator(ctx, m, write)
			release()
			return
		default:
			if logged != "standby" {
				logged = "standby"
				slog.InfoContext(ctx, "wardynd: groundtruth rotator: standby, another replica holds the lock")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(groundtruthRotatorLockBackoff):
		}
	}
}

// writeTokenFileAtomic writes the token 0600 via a temp file + rename so the ingest
// (a concurrent reader) never observes a half-written token.
func writeTokenFileAtomic(path, token string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gt-token-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
