// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
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
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// loadTLSConfig reads the operator-supplied WARDYN_TLS_CERT/WARDYN_TLS_KEY pair
// once at boot and builds the *tls.Config both HTTP(S) listeners are derived
// from (serveAndShutdown's console listener and startUISandboxGateway's second
// one — "one certificate, two names", per that function's own doc comment).
// The RETURNED value must never be handed to an http.Server directly: each
// caller goes through tlsConfigForListener instead (below), which Clone()s it
// — net/http's HTTP/2 setup (onceSetNextProtoDefaults, h2_bundle.go)
// mutates Server.TLSConfig IN PLACE (appending to NextProtos, setting
// PreferServerCipherSuites) before its own later ServeTLS clone runs, so two
// *http.Server instances sharing ONE *tls.Config would race on that mutation —
// the exact shape of the 0.7.9 regression (a shared *tls.Config mutated by
// HTTP/2 broke corporate-CA installs). TestListenersDoNotShareOneTLSConfig
// pins the clone; TestEveryTLSListenerGetsItsOwnConfig pins that every
// http.Server goes through it.
//
// The key goes through the shared _FILE mode rule (cliutil.ReadSecretFile),
// the same one every other secret-file setting is checked against (#1116,
// #1293): unlike those, WARDYN_TLS_KEY is already a path rather than a value
// with a _FILE twin, but it is exactly as sensitive, and until now it was
// read straight off disk by net/http's ListenAndServeTLS with no mode check
// at all — a group- or world-readable key was accepted silently. The
// certificate is public, so it is read with a plain os.ReadFile.
func loadTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("refusing to start: WARDYN_TLS_CERT=%q is unreadable: %w", certPath, err)
	}
	keyPEM, err := cliutil.ReadSecretFile("WARDYN_TLS_KEY", keyPath)
	if err != nil {
		return nil, fmt.Errorf("refusing to start: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("refusing to start: WARDYN_TLS_CERT/WARDYN_TLS_KEY do not form a valid certificate and key pair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// tlsConfigForListener returns the *tls.Config ONE HTTP(S) listener should
// use: a Clone() of posture's shared, loaded keypair (posture.tlsConfig), or
// nil when TLS is not enabled. Both httpSrv constructions below (this file's
// serveAndShutdown and startUISandboxGateway) call this rather than reading
// posture.tlsConfig directly — see loadTLSConfig's doc comment for why
// handing the SAME *tls.Config to more than one *http.Server races.
// TestListenersDoNotShareOneTLSConfig pins the clone and
// TestEveryTLSListenerGetsItsOwnConfig pins the two call sites.
func tlsConfigForListener(posture tlsPosture) *tls.Config {
	return posture.tlsConfig.Clone()
}

// resolveTLSPosture derives the TLS/cookie posture (validateConfig) and, when
// TLS is enabled, loads the keypair (loadTLSConfig) — combined into one call
// so run() keeps a single err-check here instead of gaining a branch of its
// own; see the call site's comment (main.go) for why that matters.
func resolveTLSPosture(dsn, tlsCert, tlsKey, listen string, tlsTerminated, allowPlaintextListen bool) (tlsPosture, error) {
	posture, err := validateConfig(dsn, tlsCert, tlsKey, listen, tlsTerminated, allowPlaintextListen)
	if err != nil {
		return tlsPosture{}, err
	}
	if !posture.tlsEnabled {
		return posture, nil
	}
	posture.tlsConfig, err = loadTLSConfig(tlsCert, tlsKey)
	if err != nil {
		return tlsPosture{}, err
	}
	return posture, nil
}

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
//   - Sweeper leader: the sweepers that must run once (approval expiry,
//     recording retention, credential expiry, the always-egress reconcile and
//     the run pause) run on one replica elected by a Postgres advisory lock
//     (db.SweeperLeader), each started and stopped with that replica's term.
//     The run-secret sweeper is NOT among them: it drops this replica's own
//     in-memory cache, so every replica runs it. A nil leader (a test, or a
//     deployment that never built one) runs every sweeper unconditionally.
//   - Approval expiry sweeper: transition PENDING approvals older than the
//     cutoff to EXPIRED so the queue does not grow unbounded.
//   - Recording retention sweeper: delete stored session recordings past the
//     operator's retention window. Off unless WARDYN_RECORDING_RETENTION_DAYS
//     is set, and only for stores that implement retention (fs and pg today,
//     via the recordingSweepable interface in adapters.go; a future
//     object-storage backend would use its own bucket lifecycle rules
//     instead).
//   - Credential expiry sweeper: delete stored sign-ins past their expires_at,
//     daily (api.Server.SweepExpiredCredentials).
//   - Boot-time reconciliation (C3): re-derive the state of any run left
//     non-terminal by a previous process (crash/restart) so it is not stranded
//     RUNNING forever with a live sandbox and un-revoked credentials.
//     Best-effort; a reconciliation error never blocks startup.
func startBackgroundWorkers(rootCtx context.Context, f *bootFlags, srv *api.Server, run runner.Runner, pool *pgxpool.Pool, st store.PG, idp identity.Provider, brk *broker.Broker, maskedRec audit.Recorder, recStore recording.Store, leader *db.SweeperLeader, ticks *sweephealth.Tracker) {
	if leader != nil {
		go goSafe("sweeper.leader", func() { leader.Run(rootCtx) })
	}
	// Every replica registers every sweep this install runs, whether or not it
	// ever holds the sweeper lock, so a follower can see a stopped leader.
	_, recSweepable := recStore.(recordingSweepable)
	registerSweepHealth(ticks, sweepInstall{
		runner: run != nil, autoStop: *f.autoStopInterval, approvalExpiry: *f.approvalExpiryInterval,
		recordingSweepable: recSweepable, recordingRetentionDays: *f.recordingRetention,
		runOutputPersist: *f.runOutputPersist,
		api:              srv.HealthSweeps(),
	})
	// The audit retention policy and the daily partition sweep (a nil pool is a unit test).
	if pool != nil {
		startAuditRetention(rootCtx, f, st, leader)
		warnSmallPool(pool.Config().MaxConns)
	}

	if run != nil && *f.autoStopInterval > 0 {
		reaper := lifecycle.New(
			lifecycleStore{pool: pool},
			lifecycleStopper{
				pool: pool, runner: run, identity: idp, broker: brk,
				// The third terminal writer's approval cascade (V1-D1). srv owns the
				// approvals service; the reaper owns the STOPPED transition. Threading
				// the one method is smaller than giving the reaper its own approval
				// store and its own copy of the reason derivation.
				cancelApprovals: srv.CancelTerminalRunApprovals,
				finishOutput:    srv.FinishRunOutput,
				snapshotPane:    srv.SnapshotRunPane,
			},
			maskedRec,
			lifecycle.Config{Interval: *f.autoStopInterval, MaxAge: cliutil.EnvDuration("WARDYN_RUN_MAX_AGE", 0), TickLock: reapTickLock(pool), Sweeps: ticks},
		)
		go goSafe("lifecycle.reaper", func() { reaper.Run(rootCtx) })
		slog.Info("wardynd: lifecycle reaper started", slog.Duration("interval", *f.autoStopInterval))
	}

	if gtFile := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE")); gtFile != "" {
		go goSafe("groundtruth.rotator", func() { runGroundtruthTokenRotatorLeader(rootCtx, groundtruthRotatorLock(pool), idp, gtFile) })
		slog.Info("wardynd: groundtruth token rotator started", slog.String("file", gtFile))
	}

	if *f.approvalExpiryInterval > 0 {
		leaderGo(rootCtx, leader, "approval.sweeper", func(ctx context.Context) {
			// FIX #5: sweeper shares maskedRec so approval.expire events fan out to SIEM.
			runApprovalSweeper(ctx, approvalStore{PG: store.NewPG(pool), rec: maskedRec}, *f.approvalExpiryInterval, *f.approvalExpiryAfter, srv, ticks)
		})
		slog.Info("wardynd: approval expiry sweeper started",
			slog.Duration("interval", *f.approvalExpiryInterval),
			slog.Duration("after", *f.approvalExpiryAfter),
		)
	}

	if rs, ok := recStore.(recordingSweepable); ok && *f.recordingRetention > 0 {
		after := time.Duration(*f.recordingRetention) * 24 * time.Hour
		leaderGo(rootCtx, leader, "recording.sweeper", func(ctx context.Context) {
			runRecordingSweeper(ctx, rs, maskedRec, recordingSweepInterval, after, ticks)
		})
		slog.Info("wardynd: recording retention sweeper started", slog.Duration("after", after))
	}

	// Run-secret eviction lane: drop the in-memory plaintext masking corpus of
	// runs terminal past api.RunSecretGrace, so a long-lived daemon stops
	// holding credentials for every run it ever dispatched. Unconditional — a
	// no-op without a mask registry, and there is nothing to configure.
	go goSafe("secret.sweeper", func() { runSecretSweeper(rootCtx, srv, runSecretSweepInterval, ticks) })
	startRunOutputSweeper(rootCtx, leader, srv, runOutputSweepInterval, ticks)
	startSCIMPurgeSweeper(rootCtx, f, leader, srv, scimPurgeSweepInterval)
	leaderGo(rootCtx, leader, "credential.sweeper", func(ctx context.Context) { runCredentialSweeper(ctx, srv, credentialSweepInterval, ticks) })

	// NOT gated on run != nil, unlike the lifecycle reaper above: ReconcileOnBoot
	// is independent of s.cfg.Runner (its own doc comment, internal/api/reconcile.go)
	// — the envbuild orphan-build sweep needs only an ImageBuilder, which a
	// `-runner none -envbuild` headless-API deployment still configures. Gating
	// this call on run left that deployment's build containers never swept.
	if rerr := srv.ReconcileOnBoot(rootCtx); rerr != nil {
		slog.WarnContext(rootCtx, "wardynd: boot reconciliation", slog.Any("err", rerr))
	}

	// Terminal sandbox sweep ticker (#710): started AFTER ReconcileOnBoot
	// returns, so the ordering is structural rather than resting only on the
	// ticker's own interval being far slower (terminal_sandbox_sweeper.go's
	// doc comment has the timing argument too, for the case this call is ever
	// moved back above). Gated the same as the lifecycle reaper above (nothing
	// to probe with no Runner), independent of autoStopInterval.
	if run != nil {
		startTerminalSandboxSweeper(rootCtx, srv, terminalSandboxSweepTickLock(pool), terminalSandboxSweepInterval, ticks)
	}

	// D28: re-apply decided `always`-scoped egress decisions onto their
	// workspaces, healing any allow/deny the non-atomic post-Decide write-back
	// dropped on a PG blip. In a goroutine — it reads all decided egress
	// approvals, which need not gate serving.
	leaderGo(rootCtx, leader, "egress.reconcile", func(ctx context.Context) {
		if n, rerr := srv.ReconcileWorkspaceEgressDecisions(ctx); rerr != nil {
			slog.WarnContext(ctx, "wardynd: always-egress reconcile deferred", slog.Any("err", rerr))
		} else if n > 0 {
			slog.InfoContext(ctx, "wardynd: reconciled always-egress decisions onto workspaces", slog.Int("decisions", n))
		}
	})
}

// smallPoolWarnBelow is the pool the two tick locks need: the lifecycle reaper's
// tick and the terminal-sandbox sweep's can overlap, and each holds one
// connection for its lock while its queries take another.
const smallPoolWarnBelow = 4

// warnSmallPool says at boot that pool_max_conns is too small for the ticks.
// pgxpool.Acquire BLOCKS on an empty pool rather than erroring, so the failure
// looks like a hang, not a misconfiguration (docs/ENV.md, WARDYN_PG_DSN).
func warnSmallPool(maxConns int32) {
	if maxConns >= smallPoolWarnBelow {
		return
	}
	slog.Warn("wardynd: pool_max_conns below 4 — the lifecycle reaper's tick and the terminal-sandbox sweep's tick can overlap, each holding one connection for its lock while its queries take another; requests can block waiting for a connection. Below 6, a sign-in during a tick is not serialized (auth.signin_unserialized)",
		slog.Int("pool_max_conns", int(maxConns)))
}

// startSSHGateway launches the SSH gateway's accept loop in its own goroutine
// (like the periodic workers above), extracted verbatim from run() to hold
// run() under the lint gate's funlen limit. A no-op inside ServeSSHGateway
// when -ssh-listen is empty, so this is safe to call unconditionally; goSafe
// contains a panic the same as every other background goroutine here.
func startSSHGateway(rootCtx context.Context, f *bootFlags, srv *api.Server) {
	if *f.sshListen == "" {
		return
	}
	go goSafe("ssh.gateway", func() {
		if serr := srv.ServeSSHGateway(rootCtx); serr != nil {
			slog.Error("wardynd: ssh gateway stopped", slog.Any("err", serr))
		}
	})
}

// startUISandboxGateway launches the UI-sandbox gateway's own HTTP(S) listener
// in its own goroutine, like startSSHGateway above. A no-op when
// -ui-sandbox-listen is empty (off = no listener, no new surface) or when the
// server built no handler for it.
//
// It is a SECOND http.Server, not a route on the console's: that separation is
// the security control (see internal/api/uigateway.go's header), and boot has
// already refused a UI address equal to the console's. It reuses the SAME TLS
// cert/key — one certificate, two names is a deployment detail, and a
// deployment that terminates TLS upstream terminates both the same way. It
// gets its OWN Clone() of posture.tlsConfig, not the shared value itself: see
// loadTLSConfig's doc comment for why sharing one *tls.Config between the two
// listeners would race.
//
// The timeouts mirror serveAndShutdown's for the same reasons: no whole-request
// deadline (a relayed editor holds a long-lived streaming connection), a
// header-read deadline, and a capped header size. Shutdown rides rootCtx: the
// listener closes when the daemon does.
func startUISandboxGateway(rootCtx context.Context, f *bootFlags, posture tlsPosture, srv *api.Server) {
	handler := srv.UIGatewayHandler()
	if *f.uiListen == "" || handler == nil {
		return
	}
	httpSrv := &http.Server{
		Addr:              *f.uiListen,
		Handler:           handler,
		TLSConfig:         tlsConfigForListener(posture),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go goSafe("ui.gateway", func() {
		<-rootCtx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	})
	go goSafe("ui.gateway.serve", func() {
		slog.Info("wardynd: ui-sandbox gateway listening", slog.String("listen", *f.uiListen), slog.Bool("tls", posture.tlsEnabled))
		var err error
		if posture.tlsEnabled {
			err = httpSrv.ListenAndServeTLS("", "")
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("wardynd: ui-sandbox gateway stopped", slog.Any("err", err))
		}
	})
}

// forwarderJoinWait bounds serveAndShutdown's wait for the org federation
// forwarder (issue #1131): it observes the same rootCtx.Done() this function's
// select does, so it is already stopping by the time we wait on it, and only a
// wedged store or client call would ever make this matter.
const forwarderJoinWait = 5 * time.Second

// serveAndShutdown runs the HTTP(S) server until a shutdown signal or a serve
// error, then drains: graceful HTTP shutdown first, audit sinks last (after the
// server has stopped accepting requests, so no further audit events are
// produced). Every exit path must Close the sinks, or the final batch is
// abandoned. Extracted verbatim from run(); fan may be nil. The proxy-facing
// TLS listener (hop, internal_tls.go) shares this lifecycle: its serve error
// ends the daemon like the console's, and it drains in the same Shutdown pass.
// orgFederation is nil unless hybrid enrolment (issue #103) is on; when set,
// it is joined (not merely cancelled) before this returns, so the process
// never exits while its goroutine might still be logging or touching the
// store.
func serveAndShutdown(rootCtx context.Context, f *bootFlags, posture tlsPosture, srv *api.Server, idpName string, fan *sinks.Fanout, denials *audit.DenialCoalescer, hop *hopTLS, orgFederation *federation.Forwarder) error {
	httpSrv := &http.Server{
		Addr:              *f.listen,
		Handler:           srv.Handler(),
		TLSConfig:         tlsConfigForListener(posture),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: long-lived streaming endpoints (the attach
		// WebSocket and the fleet SSE stream) must not be killed by a whole-request
		// deadline. IdleTimeout bounds idle keep-alive connections and MaxHeaderBytes
		// caps header size (slowloris/abuse) without affecting request bodies.
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	errCh := make(chan error, 2)
	internalSrv := startInternalListener(hop, f, srv.InternalHandler(), errCh)
	go func() {
		switch {
		case posture.tlsEnabled:
			slog.Info("wardynd: listening with built-in TLS",
				slog.String("version", version.String()),
				slog.String("listen", *f.listen),
				slog.String("identity", idpName),
				slog.String("trust_domain", *f.trustDomain),
			)
			if err := httpSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		default:
			slog.Info("wardynd: listening",
				slog.String("version", version.String()),
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

	// EVERY exit path from here on drains the audit sinks: a ListenAndServe or
	// Shutdown error must close the fanout too, or whatever the webhook batcher
	// still holds goes to the garbage collector instead of the SIEM — on
	// precisely the exit an operator is most likely to be investigating.
	// Deferred rather than repeated at three returns, and it still runs AFTER
	// FlushAuthFailedStreak on the normal path (defers run last).
	defer func() {
		if fan == nil {
			return
		}
		if cerr := fan.Close(); cerr != nil {
			slog.Error("wardynd: audit sink shutdown", slog.Any("err", cerr))
		}
	}()

	// Declared after the fanout drain, so it runs BEFORE it: the open dry-run
	// denial windows write their summaries while the sinks and the pool are
	// still open.
	defer denials.Flush(context.Background())

	select {
	case <-rootCtx.Done():
		slog.Info("wardynd: shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), api.HTTPShutdownTimeout)
	defer shutCancel()
	// fan.Close() is the deferred drain above — reached from here and from the
	// serve-error return alike.
	return runShutdownSequence(shutCtx, internalSrv, httpSrv, srv, orgFederation)
}

// httpShutdowner is the *http.Server surface runShutdownSequence drives,
// narrowed so a test can inject a Shutdown that fails without standing up a
// real listener.
type httpShutdowner interface {
	Shutdown(ctx context.Context) error
}

// backgroundServer is the *api.Server surface runShutdownSequence needs after
// the HTTP drain, narrowed for the same reason as httpShutdowner.
type backgroundServer interface {
	WaitBackground()
	FlushAuthFailedStreak()
}

// runShutdownSequence drains the internal listener, then the public HTTP
// server, then any goBackground work still in flight, then flushes the
// auth-failure streak — in that order, unconditionally, EVEN when the HTTP
// drain itself fails or hits its budget: a timed-out or errored Shutdown
// (shutErr != nil) must not skip what follows. An early return here would
// answer the "shutdown is done" question honestly for HTTP, but still drop
// the run.kill row and both revocations the same way a SIGKILL would (see
// WaitBackground below), on precisely the slow shutdown where they matter
// most. Extracted from serveAndShutdown so a test can inject a Shutdown that
// fails and prove WaitBackground/FlushAuthFailedStreak still ran
// (TestRunShutdownSequence*) — TestServeShutdownOrder used to pin the same
// invariant by walking serveAndShutdown's AST for call order.
func runShutdownSequence(shutCtx context.Context, internalSrv *http.Server, httpSrv httpShutdowner, srv backgroundServer, orgFederation *federation.Forwarder) error {
	if internalSrv != nil {
		_ = internalSrv.Shutdown(shutCtx)
	}
	// A timed-out HTTP drain (shutErr != nil) must not skip what follows: an
	// early return here would answer the "shutdown is done" question honestly
	// for HTTP, but still drop the run.kill row and both revocations the same
	// way a SIGKILL would (see WaitBackground below), on precisely the slow
	// shutdown where they matter most.
	shutErr := httpSrv.Shutdown(shutCtx)
	if shutErr != nil {
		slog.Warn("wardynd: HTTP drain hit its budget", slog.Any("err", shutErr))
	}

	// httpSrv.Shutdown only waits for in-flight HANDLERS to return — it knows
	// nothing about work a handler deliberately detached from itself (a sign-in
	// launch's dispatch, a superseded sandbox's teardown: internal/api's
	// goBackground/WaitBackground). Without this, a SIGTERM landing between a
	// supersede's KILLED claim and its teardown would answer the sign-in POST
	// (already done) but drop the run.kill row and both revocations on the
	// floor — a run left KILLED with its sandbox still up and its credentials
	// still live. WaitBackground carries its own bound (killCascadeTimeout plus
	// a margin) and logs if it hits it, so this cannot turn an orderly stop
	// into a hang.
	srv.WaitBackground()

	// The forwarder goroutine bootHybrid started on rootCtx is already
	// stopping (it selects on the same rootCtx.Done() this function's own
	// select did, above), so this only waits for it to actually finish rather
	// than abandoning it mid-run — the gap that left one still logging past a
	// test's end in issue #1131. Bounded so a wedged store or client call
	// cannot turn an orderly stop into a hang.
	if orgFederation != nil {
		select {
		case <-orgFederation.Done():
		case <-time.After(forwarderJoinWait):
			slog.Warn("wardynd: shutdown budget hit with the org federation forwarder still running; abandoning it",
				slog.Duration("budget", forwarderJoinWait))
		}
	}

	// BETWEEN the two, deliberately: the server has stopped accepting requests
	// (so no new auth.fail can open a streak) and the sinks are still open (so
	// the summary row this emits is actually delivered). Same slot the proxy
	// flushes its private-IP memo in.
	srv.FlushAuthFailedStreak()

	if shutErr != nil {
		return fmt.Errorf("shutdown: %w", shutErr)
	}
	return nil
}
