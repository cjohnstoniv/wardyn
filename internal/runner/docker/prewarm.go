// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"log/slog"
	"time"
)

// imagePrewarmTimeout bounds the background pull PrewarmImages fires at boot —
// generous (a cold registry pull of a small image, on a slow link) but not
// unbounded, so a stuck registry cannot leak the goroutine forever.
const imagePrewarmTimeout = 5 * time.Minute

// PrewarmImages best-effort pulls the proxy sidecar image and the drive-probe
// image in the background, so neither is resolved for the first time on a
// real request. Fire-and-forget (SF-14): without this, a fresh daemon's FIRST
// host_path drive create/preflight paid a live registry pull inside
// driveShareProbeTimeout's 5s budget, and driveHomeReadableByAgent reads any
// probe error as "unknown" — fail open — so that first request's honest
// answer was "pass", not "wait". A failed prewarm of the PROXY image is
// retried inline, synchronously, the next time it is needed (CreateSandbox
// still calls ensureImage). The drive-probe image differs: ProbeDrive only
// checks presence and errors if it is absent, so a failed prewarm of THAT
// image leaves every probe failing open until a later prewarm, a daemon
// restart, or an operator pull succeeds.
func (d *Driver) PrewarmImages() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), imagePrewarmTimeout)
		defer cancel()
		for _, ref := range []string{d.cfg.ProxyImage, d.driveProbeImage()} {
			if ref == "" {
				continue
			}
			if err := d.ensureImage(ctx, ref, nil); err != nil {
				slog.Warn("wardynd: docker: background image prewarm failed; the first request needing it will pull it inline instead",
					slog.String("image", ref), slog.Any("err", err))
			}
		}
	}()
}
