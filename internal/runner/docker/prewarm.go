// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"log/slog"
	"time"
)

// imagePrewarmTimeout bounds PrewarmImages' background pull: generous enough for
// a cold registry pull on a slow link, but bounded so a stuck registry can't
// leak the goroutine forever.
const imagePrewarmTimeout = 5 * time.Minute

// PrewarmImages best-effort pulls the proxy sidecar image and the drive-probe
// image in the background, so neither is resolved for the first time on a real
// request. Fire-and-forget (SF-14): a failed PROXY prewarm is retried inline by
// CreateSandbox's ensureImage, but a failed drive-probe prewarm leaves every
// probe failing open (ProbeDrive errors on absence) until a later prewarm, a
// daemon restart, or an operator pull succeeds.
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
