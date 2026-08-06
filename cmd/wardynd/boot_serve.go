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
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// startBackgroundWorkers launches the daemon's periodic goroutines and runs the
// boot-time reconciliation pass. Extracted verbatim from run():
//
//   - Lifecycle reaper: stop idle RUNNING sandboxes past their policy threshold.
//     Disabled when no runner is wired (nothing to stop) or interval <= 0.
//   - Groundtruth token rotator: keep a shared token file fresh so the
//     eBPF/Tetragon ingest sidecar — which re-reads the file on a 401 — recovers
//     when its ~1h token expires instead of going permanently blind. Off
//     unless WARDYN_GROUNDTRUTH_TOKEN_FILE is configured. Leader-elected
//     across replicas (S2): a Postgres advisory lock acquired once, so at most
//     one replica rewrites the shared file in the steady state; standbys retry
//     on a backoff and take over automatically when the leader's session ends.
//     Not fencing — a lost session can leave two writers briefly, which is
//     harmless because each write is an atomic rename of a stateless token.
//   - Approval expiry sweeper: transition PENDING approvals older than the
//     cutoff to EXPIRED so the queue does not grow unbounded.
//   - Recording retention sweeper: delete stored session recordings past the
//     operator's retention window. Off unless WARDYN_RECORDING_RETENTION_DAYS
//     is set, and only for stores that implement retention (fs and pg today,
//     via the recordingSweepable interface in adapters.go; a future
//     object-storage backend would use its own bucket lifecycle rules
//     instead).
//   - Boot-time reconciliation (C3): re-derive the state of any run left
//     non-terminal by a previous process (crash/restart) so it is not stranded
//     RUNNING forever with a live sandbox and un-revoked credentials.
//     Best-effort; a reconciliation error never blocks startup.
func startBackgroundWorkers(rootCtx context.Context, f *bootFlags, srv *api.Server, run runner.Runner, pool *pgxpool.Pool, idp identity.Provider, brk *broker.Broker, maskedRec audit.Recorder, recStore recording.Store) {
	if run != nil && *f.autoStopInterval > 0 {
		reaper := lifecycle.New(
			lifecycleStore{pool: pool},
			lifecycleStopper{pool: pool, runner: run, identity: idp, broker: brk},
			maskedRec,
			lifecycle.Config{Interval: *f.autoStopInterval, TickLock: reapTickLock(pool)},
		)
		go goSafe("lifecycle.reaper", func() { reaper.Run(rootCtx) })
		slog.Info("wardynd: lifecycle reaper started", slog.Duration("interval", *f.autoStopInterval))
	}

	if gtFile := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE")); gtFile != "" {
		// The rotator's leader lock holds ONE pooled connection for the whole
		// process lifetime, and the reaper borrows another for the length of each
		// tick — so a pool sized below 3 can leave request-serving queries with
		// none, and pgxpool.Acquire BLOCKS until its context is done rather than
		// erroring. That failure looks like a hang, not a misconfiguration, so
		// say so at boot (docs/ENV.md, WARDYN_PG_DSN's pool_max_conns note).
		if mc := pool.Config().MaxConns; mc < 3 {
			slog.Warn("wardynd: pool_max_conns below the documented minimum of 3 while the groundtruth rotator is enabled — the rotator holds one connection for the process lifetime and the lifecycle reaper borrows one per tick; requests can block waiting for a connection",
				slog.Int("pool_max_conns", int(mc)))
		}
		go goSafe("groundtruth.rotator", func() { runGroundtruthTokenRotatorLeader(rootCtx, groundtruthRotatorLock(pool), idp, gtFile) })
		slog.Info("wardynd: groundtruth token rotator started", slog.String("file", gtFile))
	}

	if *f.approvalExpiryInterval > 0 {
		go goSafe("approval.sweeper", func() {
			// FIX #5: sweeper shares maskedRec so approval.expire events fan out to SIEM.
			runApprovalSweeper(rootCtx, approvalStore{PG: store.NewPG(pool), rec: maskedRec}, *f.approvalExpiryInterval, *f.approvalExpiryAfter)
		})
		slog.Info("wardynd: approval expiry sweeper started",
			slog.Duration("interval", *f.approvalExpiryInterval),
			slog.Duration("after", *f.approvalExpiryAfter),
		)
	}

	if rs, ok := recStore.(recordingSweepable); ok && *f.recordingRetention > 0 {
		after := time.Duration(*f.recordingRetention) * 24 * time.Hour
		go goSafe("recording.sweeper", func() {
			runRecordingSweeper(rootCtx, rs, maskedRec, time.Hour, after)
		})
		slog.Info("wardynd: recording retention sweeper started", slog.Duration("after", after))
	}

	// NOT gated on run != nil, unlike the lifecycle reaper above: ReconcileOnBoot
	// is independent of s.cfg.Runner (its own doc comment, internal/api/reconcile.go)
	// — the envbuild orphan-build sweep needs only an ImageBuilder, which a
	// `-runner none -envbuild` headless-API deployment still configures. Gating
	// this call on run left that deployment's build containers never swept.
	if rerr := srv.ReconcileOnBoot(rootCtx); rerr != nil {
		slog.WarnContext(rootCtx, "wardynd: boot reconciliation", slog.Any("err", rerr))
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
				slog.String("version", version.Version),
				slog.String("listen", *f.listen),
				slog.String("identity", idpName),
				slog.String("trust_domain", *f.trustDomain),
			)
			if err := httpSrv.ListenAndServeTLS(*f.tlsCert, *f.tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		default:
			slog.Info("wardynd: listening",
				slog.String("version", version.Version),
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
