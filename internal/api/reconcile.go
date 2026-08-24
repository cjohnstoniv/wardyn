// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Watcher-lease timings (agent_runs.watcher_heartbeat, migration 0027).
// A live watcher refreshes its lease every watcherHeartbeatInterval; a lease
// silent for watcherLeaseStaleAfter (three missed heartbeats) is presumed dead
// and claimable; every replica looks for those every watcherSweepInterval. So a
// run orphaned by a pod that never comes back is adopted within stale+sweep
// (~2.5 min) by whichever replica is alive — during which its sandbox simply
// keeps running, which is why the window is generous rather than tight.
const (
	watcherHeartbeatInterval = 30 * time.Second
	watcherLeaseStaleAfter   = 90 * time.Second
	watcherSweepInterval     = 60 * time.Second
)

// watcherOwner identifies THIS control-plane process in agent_runs.watcher_owner.
// Forensics only — which replica adopted a run. The mutual exclusion is the
// claim UPDATE itself, never this value, so it needs no coordination: hostname
// (the pod name under k8s) plus a per-process uuid, because two processes can
// share a hostname.
var watcherOwner = func() string {
	host, _ := os.Hostname()
	return host + "/" + uuid.NewString()
}()

// ImageBuildSweeper is an OPTIONAL capability an ImageBuilder may additionally
// implement: a boot-time sweep that force-removes build containers orphaned
// by a crashed or restarted wardynd process. It is a separate interface
// (rather than a fourth ImageBuilder method) because api must stay
// target-agnostic — the concrete envbuild package is docker-tagged, so this
// is checked via type assertion in sweepOrphanedBuilds below, the same
// optional-capability pattern as store.RunWatcherLeaser. An ImageBuilder that
// doesn't implement it (nil, a test fake, or a future non-docker target) is
// simply never swept.
type ImageBuildSweeper interface {
	SweepOrphanedBuilds(ctx context.Context) error
}

// SandboxOrphanSweeper is an OPTIONAL capability a Runner may implement: a
// label-keyed sweep of sandbox containers whose run row no longer owns them.
// A crash BETWEEN CreateSandbox and SetSandboxRef (runs_dispatch.go) leaves the
// per-run agent/proxy containers + network running under a run row with an EMPTY
// sandbox_ref — and every ref-keyed teardown (reconcileOrphanedSandbox,
// SweepTerminalSandboxes) skips a ref-empty row, so those containers leak across
// every reboot, untracked (D13). This sweep finds them by the wardyn.run-id
// label the substrate stamps and tears down any whose run isOrphan, past the
// minAge dispatch grace. Checked by type assertion (like ImageBuildSweeper) so
// api stays target-agnostic; a Runner without it (nil, a test fake, a future
// non-docker target) is simply never swept. isOrphan is called by the substrate
// with each labeled run id; only api can read run rows to answer it.
type SandboxOrphanSweeper interface {
	SweepOrphanedSandboxes(ctx context.Context, minAge time.Duration, isOrphan func(runID uuid.UUID) bool) (int, error)
}

// ReconcileOnBoot rebuilds the safety net that keeps a run from stranding
// non-terminal forever with a live sandbox and un-revoked credentials (C3), then
// keeps rebuilding it for the life of the daemon. Three parts, because they are
// safe under different conditions:
//
//  1. finalizeUndispatchedRuns — at boot AND on a slow cadence after it. A
//     non-terminal run older than undispatchedGrace with no sandbox ref cannot
//     still be provisioning anywhere, so the process that was provisioning it is
//     gone. It is the only reaper of that state, and the process that could
//     reach it may never boot again, so boot-only would strand it forever.
//  2. sweepRunWatchers — at boot AND every watcherSweepInterval after. It claims
//     runs whose watcher lease has gone stale, so a run orphaned by a pod that
//     never comes back is adopted by ANY live replica instead of waiting for that
//     pod's own boot, which may never happen. This is what the per-run watcher
//     goroutine (an in-process, un-persisted thing) could never provide alone.
//  3. sweepOrphanedBuilds — at boot AND every buildSweepInterval after,
//     independent of s.cfg.Runner: reaps envbuild build containers left behind
//     by a crashed/restarted process (see ImageBuildSweeper). The boot pass
//     alone is NOT enough, because the sweep's own safety gate is an age gate
//     (2*BuildTimeout, envbuild/reaper.go): under systemd Restart=always or a
//     k8s pod restart the process is back within seconds, so at the one moment
//     anything looked, the orphan was too young to reap — and its writable
//     layer would then leak until the next boot, which may be weeks away.
//
// Best-effort: errors are logged, never fatal. Adopted watchers run on the
// daemon base context so they outlive this call, and observe the AGENT via the
// persisted agent_exec_id rather than the container — an idle-container exec run
// whose agent already exited is finalized instead of stranded.
func (s *Server) ReconcileOnBoot(ctx context.Context) error {
	buildErr := s.sweepOrphanedBuilds(ctx)
	if _, ok := s.cfg.ImageBuilder.(ImageBuildSweeper); ok && s.watcherBaseCtx() != nil {
		go s.orphanedBuildSweeper(s.watcherBaseCtx(), buildSweepInterval)
	}
	if s.cfg.Runner == nil {
		return buildErr
	}
	// Start the periodic sweep FIRST, before anything that can fail: a transient
	// store error in the boot pass must not disable adoption for the process's
	// whole lifetime, which is exactly what returning early ahead of this line
	// did. Both boot passes then run regardless of the other's error.
	go s.runWatcherSweeper(s.watcherBaseCtx(), watcherSweepInterval)
	// sweepOrphanedSandboxes runs LAST: finalizeUndispatchedRuns above has by now
	// flipped every ref-empty abandoned run terminal, so those runs' leaked
	// containers are visible to the label sweep as "row terminal, containers still
	// alive" — the exact leak (D13). It repeats on the same slow cadence in
	// runWatcherSweeper.
	return errors.Join(buildErr, s.finalizeUndispatchedRuns(ctx), s.sweepRunWatchers(ctx), s.reconcileOrphanedSandbox(ctx), s.sweepOrphanedSandboxes(ctx))
}

// sweepOrphanedSandboxes tears down the per-run containers of runs whose row no
// longer owns them (see SandboxOrphanSweeper): a run row that is terminal,
// missing, or non-terminal-but-ref-empty-past-grace with no live watcher. The
// store-state verdict lives HERE (only api reads run rows); the label listing +
// deterministic-name teardown + age gate live in the substrate. Best-effort:
// a Runner without the optional capability is a no-op.
func (s *Server) sweepOrphanedSandboxes(ctx context.Context) error {
	sweeper, ok := s.cfg.Runner.(SandboxOrphanSweeper)
	if !ok {
		return nil
	}
	runs, err := s.cfg.Store.ListRuns(ctx)
	if err != nil {
		return fmt.Errorf("sweep orphaned sandboxes: list runs: %w", err)
	}
	byID := make(map[uuid.UUID]types.AgentRun, len(runs))
	for _, run := range runs {
		byID[run.ID] = run
	}
	leaser, hasLease := s.cfg.Store.(store.RunWatcherLeaser)
	isOrphan := func(runID uuid.UUID) bool {
		run, known := byID[runID]
		if !known {
			return true // no run row owns these containers — pure leak
		}
		if isTerminalRunState(run.State) {
			return true // the run ended; its containers should already be gone
		}
		if run.SandboxRef != "" {
			return false // live and tracked — its own lifecycle owns teardown
		}
		// Non-terminal with NO sandbox_ref: abandoned mid-dispatch UNLESS a live
		// watcher still owns it (the SetSandboxRef write was merely lost to a
		// transient store error while the run reached RUNNING with a heartbeating
		// watcher — mirrors finalizeUndispatchedRuns' RunWatcherFresh guard). The
		// substrate's minAge gate is the second guard for a young in-flight dispatch.
		if hasLease {
			if fresh, ferr := leaser.RunWatcherFresh(ctx, runID, watcherLeaseStaleAfter); ferr == nil && fresh {
				return false
			}
		}
		return true
	}
	swept, err := sweeper.SweepOrphanedSandboxes(ctx, undispatchedGrace, isOrphan)
	if swept > 0 {
		slog.InfoContext(ctx, "wardynd: orphaned sandbox sweep", slog.Int("swept", swept))
	}
	if err != nil {
		return fmt.Errorf("sweep orphaned sandboxes: %w", err)
	}
	return nil
}

// reconcileOrphanedSandbox is ReconcileOnBoot's fourth pass, closing
// W15-W15c-terminal-lifecycle-4: on the finalizeRunTail path a terminal run's
// SandboxRef only survives non-empty when THAT run's own StopSandbox call
// failed (the tail clears it on success). Kill and idle-stop tear down on
// their own paths and never clear, so their runs also reach this sweep — a
// safe no-op retry against an already-gone sandbox — and this file's other
// three passes
// deliberately skip terminal runs (they exist to finish an INCOMPLETE run,
// not to retry a completed one's cleanup; isTerminalRunState guards below).
// Nothing else ever revisits the abandoned container (and its proxy sidecar,
// which resolved injected credential VALUES into memory at startup) after
// the one failure audit at finalize time. Retry the teardown, every boot,
// for every terminal run still carrying a ref — StopSandbox is idempotent on
// an already-gone sandbox (Runner's own contract, handleKillRun's doc
// comment), so retrying against a sandbox that in fact came down fine is a
// safe no-op. Clears the ref on success so the run drops out of future
// sweeps; a still-failing teardown leaves it set (stopSandboxOrAudit already
// audits it with teardown_error) for the NEXT boot to retry.
//
// It also re-runs revokeRunCascade (bug-lifecycle-1 / W22-S1-7): a run that
// reaches this pass may have had its ORIGINAL finalize/kill/idle-stop
// teardown fail after its revoke ran — or its revoke itself fail — so it may
// carry an un-revoked identity/broker credential regardless of the teardown
// outcome. Retrying is
// safe: revokeRunCascade's own doc marks it idempotent (a deny of an
// already-denied token/credential is a no-op), so re-running it on an
// already-revoked run costs one wasted call, never a double-effect.
func (s *Server) reconcileOrphanedSandbox(ctx context.Context) error {
	runs, err := s.cfg.Store.ListRuns(ctx)
	if err != nil {
		return fmt.Errorf("reconcile orphaned sandboxes: list runs: %w", err)
	}
	for _, run := range runs {
		if !isTerminalRunState(run.State) || run.SandboxRef == "" {
			continue
		}
		s.revokeRunCascade(ctx, run.ID)
		if !s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, "sandbox.orphan_sweep") {
			continue // still stuck; audited above, ref stays set for the next boot
		}
		if serr := s.cfg.Store.SetSandboxRef(ctx, run.ID, ""); serr != nil {
			slog.ErrorContext(ctx, "wardynd: orphan sweep tore down the sandbox but could not clear its ref",
				slog.String("run_id", run.ID.String()), slog.Any("err", serr))
		}
	}
	return nil
}

// sweepOrphanedBuilds reaps envbuild build containers orphaned by a crashed or
// restarted process (see ImageBuildSweeper). Independent of s.cfg.Runner — an
// image builder can be configured on its own — and best-effort: an
// ImageBuilder that doesn't implement the optional capability (nil, a test
// fake, or a future non-docker target) is simply not swept.
func (s *Server) sweepOrphanedBuilds(ctx context.Context) error {
	sweeper, ok := s.cfg.ImageBuilder.(ImageBuildSweeper)
	if !ok {
		return nil
	}
	if err := sweeper.SweepOrphanedBuilds(ctx); err != nil {
		return fmt.Errorf("sweep orphaned envbuild containers: %w", err)
	}
	return nil
}

// buildSweepInterval is how often sweepOrphanedBuilds re-runs after the boot
// pass. What counts as orphaned is decided entirely by the sweep's own age gate
// (2*BuildTimeout); this interval only bounds how long a container survives
// AFTER crossing that line. imageBuildTimeout is the proportionate tick — one
// ContainerList per half hour is free next to the writable layer it reclaims,
// and it caps the leak at a build timeout rather than "until the next boot".
const buildSweepInterval = imageBuildTimeout

// orphanedBuildSweeper re-runs the orphaned-build sweep on a cadence for the
// life of the daemon. Started only when an ImageBuildSweeper is actually
// configured (ReconcileOnBoot), and panic-safe: a panic here must not crash the
// control plane, it just returns the sweep to boot-only.
func (s *Server) orphanedBuildSweeper(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Recover PER TICK (GAP-RECONCILE-6): one panicking tick must not degrade
			// the sweep to boot-only for the whole process lifetime.
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.ErrorContext(ctx, "wardynd: PANIC in orphaned build sweep tick (contained; sweep continues next tick)",
							slog.Any("panic", r))
					}
				}()
				if err := s.sweepOrphanedBuilds(ctx); err != nil {
					slog.WarnContext(ctx, "wardynd: orphaned build sweep", slog.Any("err", err))
				}
			}()
		}
	}
}

// undispatchedGrace is how old a run with no sandbox ref must be before the
// reconciler is allowed to call it abandoned. imageBuildTimeout is the deadline
// EVERY image-build lane runs under (runs_create.go BYOI / devcontainer /
// workspace), but it is measured from the start of the build, which is itself
// after the run row + its grants are written, and CreateSandbox (an image pull)
// still has to finish before sandbox_ref lands. So a run's age at that moment
// can exceed imageBuildTimeout even on the happy path, and the honest ceiling is
// the build deadline plus that tail. Doubling it is the deliberately blunt
// margin: being late costs an abandoned run one more grace period of stranding,
// being early FAILS a healthy run mid-dispatch and tears its sandbox down.
const undispatchedGrace = 2 * imageBuildTimeout

// finalizeUndispatchedRuns finalizes every non-terminal run that never got a
// sandbox ref: the dispatch goroutine that would have provisioned it died with
// the previous process, so nothing will ever start it — FAILED + revoke.
//
// The age gate is what makes this safe to run PERIODICALLY as well as at boot,
// and it has to be run periodically: this is the ONLY reaper of a sandbox-less
// run. The watcher sweep requires a non-empty sandbox_ref and the idle reaper
// only looks at RUNNING runs, so a crash inside a young run's build window
// would otherwise strand it non-terminal forever — PENDING, with a live run
// token and un-revoked grants — because the one boot that could have reaped it
// saw it too young and no later boot is coming.
//
// AGE GATE: the scan is table-wide, and under multiple replicas "boot" is not
// this process's private event — without the gate, ANY replica restarting would
// FAIL every in-flight pre-dispatch run fleet-wide, including ones another live
// replica is still building an image for. undispatchedGrace is the ceiling on
// how long a healthy run can legitimately sit here, so past it nothing on any
// replica is still provisioning the run and only a dead process can explain it.
func (s *Server) finalizeUndispatchedRuns(ctx context.Context) error {
	runs, err := s.cfg.Store.ListRuns(ctx)
	if err != nil {
		return err
	}
	now := s.cfg.Now()
	leaser, hasLease := s.cfg.Store.(store.RunWatcherLeaser)
	var finalized int
	for _, run := range runs {
		if isTerminalRunState(run.State) || run.SandboxRef != "" {
			continue
		}
		if now.Sub(run.CreatedAt) < undispatchedGrace {
			continue // still inside its own dispatch window; not abandoned
		}
		// A FRESH watcher lease means a LIVE process is responsible for this run
		// (GAP-RECONCILE-4): a run whose SetSandboxRef write was merely lost to a
		// transient store error reaches RUNNING with SandboxRef=="" but a live
		// completion watcher heartbeating its lease. Finalizing it would false-fail a
		// working agent mid-task and leak its sandbox forever (reconcileFinalize has
		// ref=="" so tears nothing down). Only reap when no live watcher owns it (or
		// the store cannot report lease freshness — then today's behavior stands).
		if hasLease {
			if fresh, ferr := leaser.RunWatcherFresh(ctx, run.ID, watcherLeaseStaleAfter); ferr == nil && fresh {
				continue
			}
		}
		s.reconcileFinalize(ctx, run.ID, types.RunFailed, "", "never dispatched: no sandbox and no live watcher past the dispatch-grace ceiling")
		finalized++
	}
	if finalized > 0 {
		slog.InfoContext(ctx, "wardynd: boot reconciliation", slog.Int("finalized", finalized))
	}
	return nil
}

// sweepRunWatchers claims the watcher lease on every non-terminal run whose lease
// has gone stale (see store.ClaimStaleRunWatchers — the claim is a conditional
// UPDATE ... RETURNING, so concurrent sweeps on two replicas cannot both adopt
// the same run) and re-derives the state of each run it claimed.
//
// Claimed runs are exactly those with a sandbox and no live watcher: a pod that
// died mid-run, an interactive run (which never had a completion watcher), or a
// run this process itself dispatched before a store error killed its watcher.
func (s *Server) sweepRunWatchers(ctx context.Context) error {
	leaser, ok := s.cfg.Store.(store.RunWatcherLeaser)
	if !ok {
		return nil
	}
	runs, err := leaser.ClaimStaleRunWatchers(ctx, watcherOwner, watcherLeaseStaleAfter)
	if err != nil {
		return err
	}
	base := s.watcherBaseCtx()
	var reattached, finalized int
	for _, run := range runs {
		// STRAND GUARD (GAP-RECONCILE-2): a crash between SetSandboxRef and Exec
		// leaves a non-interactive task run with a sandbox_ref but no persisted
		// agent_exec_id — no agent was ever exec'd and none ever will be. Probing the
		// container with an empty exec id falls back to container Status, which reads
		// the sleep-infinity holder as RUNNING forever, so this run would be babysat
		// eternally (the exact C3 strand the sweep exists to close). Finalize it
		// instead. An interactive run legitimately has no exec id, so it is exempt.
		//
		// W15-c: keyed on a GENUINELY empty AgentExecID only — dispatch
		// (runs_dispatch.go, mainProcessExecID) now persists a non-empty sentinel
		// for an exec-less (krun) launch, which legitimately has no separate exec
		// id but IS a live, healthy run (the container's main process is the
		// agent). Before that fix, dispatch persisted a bare "" for that case too,
		// so this guard could not tell "never exec'd, stranded" apart from
		// "exec-less, healthy" and finalized+killed the latter on every stale-lease
		// sweep. Do NOT widen this back to "AgentExecID doesn't look like a real
		// exec id" or similar — "" must stay reserved for "SetRunAgentExecID was
		// never called".
		//
		// No age gate: dispatch HOLDS this run's watcher lease continuously from just
		// before SetSandboxRef until the completion watcher takes over its own hold
		// (runs_dispatch.go), so a run that reached ClaimStaleRunWatchers with a STALE
		// lease has PROVABLY lost its dispatcher — there is no live process still
		// racing to exec the agent. The earlier age>undispatchedGrace gate was there
		// only because a one-shot lease stamp could go stale mid-dispatch; it left the
		// common fast-crash/multi-replica case (first stale claim well inside the
		// grace) stranded forever, which is the case the continuous hold now closes.
		if !run.Interactive && run.Task != "" && run.AgentExecID == "" {
			s.reconcileFinalize(ctx, run.ID, types.RunFailed, run.SandboxRef,
				"reconciled: dispatched a sandbox but the agent was never exec'd (dispatcher lost mid-dispatch)")
			finalized++
			continue
		}
		st, serr := s.probeAgent(ctx, run.SandboxRef, run.AgentExecID)
		// A genuinely-gone sandbox/agent reports a terminal STATE (RunStopped), not
		// an error — an error means the probe couldn't determine liveness (a docker
		// daemon blip). Do NOT finalize a possibly-healthy run on a transient error;
		// attach a watcher that retries and only gives up after a bounded error run.
		// Only a definitive terminal STATE finalizes here.
		if serr == nil && isTerminalRunState(st.State) {
			final := types.RunFailed
			if st.ExitCode != nil && *st.ExitCode == 0 {
				final = types.RunCompleted
			}
			s.reconcileFinalize(ctx, run.ID, final, run.SandboxRef, "reconciled after watcher lease expired")
			finalized++
			continue
		}
		// Still alive (or momentarily unreachable): attach a watcher so the run
		// finalizes when the agent actually exits, not on a transient probe error.
		go s.reconcileWatch(base, run.ID, run.SandboxRef, run.AgentExecID)
		reattached++
	}
	if reattached > 0 || finalized > 0 {
		slog.InfoContext(ctx, "wardynd: run watcher sweep",
			slog.Int("reattached", reattached), slog.Int("finalized", finalized))
	}
	return nil
}

// watcherProbeTimeout bounds ONE AgentStatus probe. Both probe sites run on the
// daemon-lifetime context, which never expires, so without a deadline a wedged
// docker socket hangs the sweep (single-threaded: one wedged run stalls every
// other run's adoption) or freezes a watcher that keeps its lease fresh from its
// own goroutine — so no replica ever adopts that run either.
const watcherProbeTimeout = 10 * time.Second

// probeAgent runs one deadline-bounded AGENT liveness probe (the persisted exec
// id, not the container — an idle exec sandbox reports RUNNING forever).
func (s *Server) probeAgent(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	pctx, cancel := context.WithTimeout(ctx, watcherProbeTimeout)
	defer cancel()
	return s.cfg.Runner.AgentStatus(pctx, ref, agentExecID)
}

// runWatcherSweeper re-runs the sweep every `every` until ctx ends (daemon
// shutdown). Panic-safe like the watchers it starts; a contained panic ends the
// sweeper, so it is logged at ERROR — adoption is degraded to boot-only until the
// process restarts. The interval is a parameter only so tests can drive it fast.
func (s *Server) runWatcherSweeper(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	// Zero, so the FIRST tick runs the undispatched pass: it costs one extra
	// ListRuns per process and it retries a boot pass that failed on a transient
	// store error. After that the pass is rate-limited to its own slow cadence —
	// it is an unbounded full-table read whose eligibility only changes on the
	// undispatchedGrace timescale, so running it every sweep tick would be pure
	// waste.
	var lastUndispatched time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Recover PER TICK (GAP-RECONCILE-6), not once around the whole loop: a
			// store-driver edge that panics on ONE claimed row must not permanently
			// degrade adoption to boot-only for the process lifetime on the strength
			// of a single log line — the next tick simply tries again.
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.ErrorContext(ctx, "wardynd: PANIC in run watcher sweep tick (contained; adoption continues next tick)",
							slog.Any("panic", r))
					}
				}()
				if now := s.cfg.Now(); now.Sub(lastUndispatched) >= undispatchedGrace {
					lastUndispatched = now
					if err := s.finalizeUndispatchedRuns(ctx); err != nil {
						slog.WarnContext(ctx, "wardynd: undispatched run reconcile", slog.Any("err", err))
					}
					// Same slow cadence as the undispatched pass (both are unbounded
					// full-table reads whose eligibility only changes on the
					// undispatchedGrace timescale), and AFTER it, so a just-finalized
					// ref-empty run's leaked containers are swept the same tick (D13).
					// ponytail: no separate ticker — piggy-backing this cadence keeps
					// one ContainerList per grace period, free next to what it reclaims.
					if err := s.sweepOrphanedSandboxes(ctx); err != nil {
						slog.WarnContext(ctx, "wardynd: orphaned sandbox sweep", slog.Any("err", err))
					}
				}
				if err := s.sweepRunWatchers(ctx); err != nil {
					slog.WarnContext(ctx, "wardynd: run watcher sweep", slog.Any("err", err))
				}
			}()
		}
	}
}

// holdRunWatcherLease marks this process as the live watcher for runID and keeps
// the lease fresh until the returned stop is called. Every watcher goroutine
// holds one: the lease is what stops another replica's sweep from adopting a run
// this process is already watching, and its going stale is what lets a replica
// adopt one whose watcher died with its pod.
//
// Nothing is cleared on stop. A watcher that finalized its run leaves it terminal
// (the sweep skips it by state), and a watcher that exits WITHOUT finalizing —
// daemon shutdown — must leave the lease to expire so another replica takes over.
func (s *Server) holdRunWatcherLease(ctx context.Context, runID uuid.UUID) (stop func()) {
	if _, ok := s.cfg.Store.(store.RunWatcherLeaser); !ok {
		return func() {}
	}
	s.stampRunWatcherLease(ctx, runID)
	done := make(chan struct{})
	go func() {
		// The ticker runs on its OWN goroutine, so the watcher's recover() does not
		// cover it: an unrecovered panic in a store driver here would take the whole
		// daemon down. Contain it — the lease then simply expires and another sweep
		// adopts the run, which is the already-safe path.
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "wardynd: PANIC in run watcher heartbeat (contained; lease left to expire)",
					slog.String("run_id", runID.String()), slog.Any("panic", r))
			}
		}()
		tick := time.NewTicker(watcherHeartbeatInterval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				s.stampRunWatcherLease(ctx, runID)
			}
		}
	}()
	return func() { close(done) }
}

// stampRunWatcherLease writes ONE "I am responsible for this run" heartbeat —
// the single funnel for every lease WRITE (the held lease's ticker, and
// dispatch's one stamp once the run finally has a sandbox to watch). A store
// without the seam (the ~30 test doubles that embed store.Store) simply does not
// lease, which is why the assert lives here rather than at each call site.
//
// Best-effort: a failed stamp only risks another replica adopting a run this one
// still watches, which is already safe — the terminal transition is a CAS, so
// exactly one of the two watchers wins it.
func (s *Server) stampRunWatcherLease(ctx context.Context, runID uuid.UUID) {
	leaser, ok := s.cfg.Store.(store.RunWatcherLeaser)
	if !ok {
		return
	}
	if err := leaser.HeartbeatRunWatcher(ctx, runID, watcherOwner); err != nil {
		slog.WarnContext(ctx, "wardynd: run watcher heartbeat",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// watcherBaseCtx is the daemon-lifetime context every detached watcher and the
// sweeper run on (never a request ctx, which dies when the handler returns).
// api.New sets BaseCtx unconditionally, so this is never nil.
func (s *Server) watcherBaseCtx() context.Context { return s.cfg.BaseCtx }

// reconcileProbeErrorCeiling bounds how LONG (wall-clock) a reconcile watcher
// tolerates PERSISTENT AgentStatus probe errors before giving up on a
// genuinely-wedged ref. It replaced a 12-error (~60s) count (GAP-RECONCILE-5):
// AgentStatus returns an error only for daemon-level unreachability (a gone
// sandbox arrives as a terminal STATE, not an error) or an ambiguous exec-404
// under a still-running container (GAP-RECONCILE-1) — and a routine loaded-dockerd
// restart, or a live-restore exec-map loss, errors for a minute or two. Finalizing
// on that mass-failed every in-flight run adopted onto reconcileWatch after any
// wardynd restart. So the watcher now BACKS OFF while errors persist and only
// finalizes after this generous ceiling, and prefers a definitive "gone"
// observation (a successful probe returning a terminal state) to ever finalizing.
const reconcileProbeErrorCeiling = 30 * time.Minute

// reconcileProbeMaxBackoff caps the error backoff interval.
const reconcileProbeMaxBackoff = 60 * time.Second

// reconcileWatch polls a re-adopted sandbox's agent liveness until it exits, then
// finalizes the run and runs the revoke cascade. Panic-safe (a panic here must
// not crash the control plane).
func (s *Server) reconcileWatch(ctx context.Context, runID uuid.UUID, ref, agentExecID string) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "wardynd: PANIC in reconcile watcher (contained)",
				slog.String("run_id", runID.String()), slog.Any("panic", r))
		}
	}()
	// This goroutine is now the run's watcher, so it owns the lease: hold it for
	// as long as it watches, and let it expire when it stops (so the next sweep,
	// here or on another replica, adopts the run).
	stopLease := s.holdRunWatcherLease(ctx, runID)
	defer stopLease()
	const baseInterval = 5 * time.Second
	tick := time.NewTicker(baseInterval)
	defer tick.Stop()
	backoff := baseInterval
	var firstErr time.Time // when the current run of consecutive errors began
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			st, err := s.probeAgent(ctx, ref, agentExecID)
			if err != nil {
				// A probe error is NOT "the run finished" (a gone sandbox arrives as a
				// terminal STATE, not an error) — finalizing here would false-kill a
				// healthy RUNNING run on a daemon blip or a >60s loaded-dockerd restart
				// (GAP-RECONCILE-5). BACK OFF and keep the sandbox alive; only give up
				// after a generous wall-clock ceiling for a genuinely-wedged ref.
				if firstErr.IsZero() {
					firstErr = s.cfg.Now()
				}
				if s.cfg.Now().Sub(firstErr) >= reconcileProbeErrorCeiling {
					s.reconcileFinalize(ctx, runID, types.RunFailed, ref, "reconciled: sandbox persistently unreachable")
					return
				}
				backoff = min(backoff*2, reconcileProbeMaxBackoff)
				tick.Reset(backoff)
				continue
			}
			if !firstErr.IsZero() { // recovered — reset the run + interval
				firstErr = time.Time{}
				backoff = baseInterval
				tick.Reset(baseInterval)
			}
			if isTerminalRunState(st.State) {
				final := types.RunFailed
				if st.ExitCode != nil && *st.ExitCode == 0 {
					final = types.RunCompleted
				}
				s.reconcileFinalize(ctx, runID, final, ref, "reconciled exit")
				return
			}
		}
	}
}

// reconcileFinalizeTimeout bounds the WHOLE finalize tail (GetRun, CAS, audit
// writes, the revoke cascade, and StopSandbox) so one wedged ContainerStop cannot
// hang the single-threaded sweeper — or the boot pass that gates serveAndShutdown,
// where an unbounded StopSandbox would keep the control plane from ever serving
// /healthz and crashloop the pod (GAP-RECONCILE-3). Generous: it must cover a
// graceful SIGTERM→SIGKILL stop (the driver's own stopTimeout is 10s) plus the
// revoke cascade, so 2× that with headroom.
const reconcileFinalizeTimeout = 60 * time.Second

// reconcileFinalize transitions a stranded run to a terminal state — conditional
// on it still being non-terminal so a concurrent kill/complete is never clobbered
// — then runs the revoke cascade and (best-effort) tears the sandbox down.
func (s *Server) reconcileFinalize(ctx context.Context, runID uuid.UUID, to types.RunState, ref, reason string) {
	// Bound the whole tail (GAP-RECONCILE-3): the sweep loop and the boot pass call
	// this SYNCHRONOUSLY, so an unbounded wedged StopSandbox would stall adoption
	// for the process lifetime (or block boot from serving).
	ctx, cancel := context.WithTimeout(ctx, reconcileFinalizeTimeout)
	defer cancel()
	cur, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: reconcile get run failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	if isTerminalRunState(cur.State) {
		return // already finalized (e.g. a concurrent kill won)
	}
	applied, err := s.casRunState(ctx, runID, cur.State, to)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: reconcile finalize failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	if !applied {
		return // someone else won the transition
	}
	// Shared terminal tail (audit run.reconcile → revoke cascade → sandbox
	// teardown with a teardown_error audit on failure → workspace/record
	// settlement). The completion watcher runs the IDENTICAL sequence via
	// finalizeRunTail, so a swallowed StopSandbox error (which would abandon a
	// live/routable container the now-terminal run's next boot skips) can never
	// creep back into just one of the two finalize paths. A scan/verify run that
	// never delivered its result fails fast instead of hanging the workspace; a
	// record run's evidence is captured from whatever audit events landed.
	s.finalizeRunTail(ctx, runID, ref, "run.reconcile", "success",
		map[string]any{"to": string(to), "reason": reason})
}
