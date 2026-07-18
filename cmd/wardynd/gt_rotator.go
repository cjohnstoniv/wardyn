// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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
	MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string) (identity.RunIdentity, error)
}

// runGroundtruthTokenRotator keeps `path` populated with a FRESH host-sensor token
// (aud=wardyn-groundtruth). The eBPF/Tetragon ingest sidecar re-reads that file on a
// 401, so with a live producer it recovers when its ~1h-TTL token expires instead of
// going permanently blind — the static WARDYN_GROUNDTRUTH_TOKEN env deployment could
// NOT refresh (a process env is fixed after exec), which is why the shipped stack went
// blind ~1h into any run. A file on a shared volume is the only refreshable
// wiring. Seeds immediately, then re-mints at ~half the remaining TTL (clamped
// [1m,30m]) so a missed tick never leaves an expired token. Best-effort per tick.
func runGroundtruthTokenRotator(ctx context.Context, m gtMinter, path string) {
	for {
		next := 30 * time.Minute
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ri, err := m.MintRunIdentity(mctx, groundtruthSensorRunID, groundtruthSensorSub, groundtruthSensorSub, groundtruthAudience)
		cancel()
		switch {
		case err != nil:
			slog.ErrorContext(ctx, "wardynd: groundtruth token rotate: mint failed", slog.Any("err", err))
			next = time.Minute // retry soon
		default:
			if werr := writeTokenFileAtomic(path, ri.Token); werr != nil {
				slog.ErrorContext(ctx, "wardynd: groundtruth token rotate: write failed",
					slog.String("path", path),
					slog.Any("err", werr),
				)
				next = time.Minute
			} else if !ri.Expiry.IsZero() {
				if half := time.Until(ri.Expiry) / 2; half < time.Minute {
					next = time.Minute
				} else if half < 30*time.Minute {
					next = half
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
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
