// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// startBackgroundWorkers launches the daemon's periodic goroutines and runs the
// boot-time reconciliation pass. Extracted verbatim from run():
//
//   - Lifecycle reaper: stop idle RUNNING sandboxes past their policy threshold.
//     Disabled when no runner is wired (nothing to stop) or interval <= 0.
//   - Groundtruth token rotator: keep a shared token file fresh so the
//     eBPF/Tetragon ingest sidecar — which re-reads the file on a 401 — recovers
//     when its ~1h token expires instead of going permanently blind (U009). Off
//     unless WARDYN_GROUNDTRUTH_TOKEN_FILE is configured.
//   - Approval expiry sweeper: transition PENDING approvals older than the
//     cutoff to EXPIRED so the queue does not grow unbounded.
//   - Boot-time reconciliation (C3): re-derive the state of any run left
//     non-terminal by a previous process (crash/restart) so it is not stranded
//     RUNNING forever with a live sandbox and un-revoked credentials.
//     Best-effort; a reconciliation error never blocks startup.
func startBackgroundWorkers(rootCtx context.Context, f *bootFlags, srv *api.Server, run runner.Runner, pool *pgxpool.Pool, idp identity.Provider, brk *broker.Broker, maskedRec audit.Recorder) {
	if run != nil && *f.autoStopInterval > 0 {
		reaper := lifecycle.New(
			lifecycleStore{pool: pool},
			lifecycleStopper{pool: pool, runner: run, identity: idp, broker: brk},
			maskedRec,
			lifecycle.Config{Interval: *f.autoStopInterval},
		)
		go goSafe("lifecycle.reaper", func() { reaper.Run(rootCtx) })
		slog.Info("wardynd: lifecycle reaper started", slog.Duration("interval", *f.autoStopInterval))
	}

	if gtFile := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE")); gtFile != "" {
		go goSafe("groundtruth.rotator", func() { runGroundtruthTokenRotator(rootCtx, idp, gtFile) })
		slog.Info("wardynd: groundtruth token rotator started", slog.String("file", gtFile))
	}

	if *f.approvalExpiryInterval > 0 {
		go goSafe("approval.sweeper", func() {
			// FIX #5: sweeper shares maskedRec so approval.expire events fan out to SIEM.
			runApprovalSweeper(rootCtx, approvalStore{pool: pool, rec: maskedRec}, *f.approvalExpiryInterval, *f.approvalExpiryAfter)
		})
		slog.Info("wardynd: approval expiry sweeper started",
			slog.Duration("interval", *f.approvalExpiryInterval),
			slog.Duration("after", *f.approvalExpiryAfter),
		)
	}

	if run != nil {
		if rerr := srv.ReconcileOnBoot(rootCtx); rerr != nil {
			slog.WarnContext(rootCtx, "wardynd: boot reconciliation", slog.Any("err", rerr))
		}
	}
}

// serveAndShutdown runs the HTTP(S) server until a shutdown signal or a serve
// error, then drains: graceful HTTP shutdown first, audit sinks last (after the
// server has stopped accepting requests, so no further audit events are
// produced — previously sinks were never Closed on shutdown, abandoning the
// final batch). Extracted verbatim from run(); fan may be nil.
func serveAndShutdown(rootCtx context.Context, f *bootFlags, posture tlsPosture, handler http.Handler, idpName string, fan *sinks.Fanout) error {
	httpSrv := &http.Server{
		Addr:              *f.listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: long-lived streaming endpoints (the attach
		// WebSocket and the fleet SSE stream) must not be killed by a whole-request
		// deadline. IdleTimeout bounds idle keep-alive connections and MaxHeaderBytes
		// caps header size (slowloris/abuse) without affecting request bodies.
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		switch {
		case posture.tlsEnabled:
			slog.Info("wardynd: listening with built-in TLS",
				slog.String("listen", *f.listen),
				slog.String("identity", idpName),
				slog.String("trust_domain", *f.trustDomain),
			)
			if err := httpSrv.ListenAndServeTLS(*f.tlsCert, *f.tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		default:
			slog.Info("wardynd: listening",
				slog.String("listen", *f.listen),
				slog.String("identity", idpName),
				slog.String("trust_domain", *f.trustDomain),
			)
			if *f.tlsTerminated {
				slog.Info("wardynd: serving plain HTTP behind a TLS-terminating reverse proxy (WARDYN_TLS_TERMINATED=true); cookies marked Secure")
			} else {
				slog.Warn("wardynd: serving PLAIN HTTP with no TLS — the control plane MUST be fronted by TLS for any non-localhost deployment (set WARDYN_TLS_CERT/WARDYN_TLS_KEY for built-in TLS, or WARDYN_TLS_TERMINATED=true behind a TLS-terminating reverse proxy)")
			}
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}
	}()

	select {
	case <-rootCtx.Done():
		slog.Info("wardynd: shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	if fan != nil {
		if cerr := fan.Close(); cerr != nil {
			slog.Error("wardynd: audit sink shutdown", slog.Any("err", cerr))
		}
	}
	return nil
}
