// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// startApprovalNotify parses WARDYN_APPROVAL_NOTIFY, installs it for the two approval insertion seams
// and starts this replica's delivery worker. Unset or empty means off: no rows are written and no
// worker runs. A bad config refuses boot with an error that names a channel id and a rule, never a
// URL or secret; the config itself is never logged. It must run after installBootTransport, so the
// worker's client clones a transport that already carries the trusted CA.
func startApprovalNotify(ctx context.Context, raw string, pool *pgxpool.Pool, rec audit.Recorder, masks *secretmask.Registry, expiry time.Duration) error {
	cfg, err := notify.Parse(raw)
	if err != nil || cfg == nil {
		return err
	}
	for _, w := range cfg.ExpiryWarnings(expiry) {
		slog.Warn("wardynd: approval notify: " + w)
	}
	notify.SetActive(cfg, rec)
	go notify.NewWorker(notify.Deps{Pool: pool, Config: cfg, Masks: masks}).Run(ctx)
	slog.Info("wardynd: approval notifications enabled (WARDYN_APPROVAL_NOTIFY)", slog.Int("channels", len(cfg.Channels)))
	return nil
}
