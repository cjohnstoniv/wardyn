// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// awsSSORenewInterval is how often the AWS SSO renewer looks for a live run's session inside its renewal
// window (WARDYN_AWS_SSO_REFRESH_SKEW, at least five minutes), so a renewal that fails is retried several
// times while the token in hand is still valid. Not configurable, like the other housekeeping cadences.
const awsSSORenewInterval = time.Minute

// awsSSORenewer is the *api.Server surface the renewer needs, narrowed so a test can drive a tick
// without a store.
type awsSSORenewer interface {
	RenewAWSSSOCredentials(context.Context) (int, error)
}

// runAWSSSORenewer renews at once and then every interval until ctx ends. ticks records each pass as the
// aws_sso_renewal sweep (nil records nothing); an error is an attempt without a success.
func runAWSSSORenewer(ctx context.Context, srv awsSSORenewer, interval time.Duration, ticks *sweephealth.Tracker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		_ = ticks.Tick(ctx, sweephealth.AWSSSORenewal, func(ctx context.Context) error {
			n, err := srv.RenewAWSSSOCredentials(ctx)
			if n > 0 {
				slog.InfoContext(ctx, "wardynd: renewed AWS SSO sessions of live runs ahead of expiry", slog.Int("renewed", n))
			}
			if err != nil {
				slog.WarnContext(ctx, "wardynd: AWS SSO renewal pass error", slog.Any("err", err))
			}
			return err
		})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// startAWSSSORenewer runs the renewer on the elected sweeper leader, like the other leader sweeps. A
// second leader for a moment renews nothing twice: each renewal takes its credential's cross-replica
// owner lock and re-reads the credential under it.
func startAWSSSORenewer(ctx context.Context, leader *db.SweeperLeader, srv awsSSORenewer, ticks *sweephealth.Tracker) {
	leaderGo(ctx, leader, "aws_sso.renewer", func(ctx context.Context) {
		runAWSSSORenewer(ctx, srv, awsSSORenewInterval, ticks)
	})
	slog.Info("wardynd: AWS SSO renewer started", slog.Duration("interval", awsSSORenewInterval))
}

var _ awsSSORenewer = (*api.Server)(nil)
