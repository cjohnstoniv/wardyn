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
	return errors.Join(buildErr, s.finalizeUndispatchedRuns(ctx), s.sweepRunWatchers(ctx))
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
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "wardynd: PANIC in orphaned build sweeper (contained; sweep now boot-only)",
				slog.Any("panic", r))
		}
	}()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.sweepOrphanedBuilds(ctx); err != nil {
				slog.WarnContext(ctx, "wardynd: orphaned build sweep", slog.Any("err", err))
			}
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
	var finalized int
	for _, run := range runs {
		if isTerminalRunState(run.State) || run.SandboxRef != "" {
			continue
		}
		if now.Sub(run.CreatedAt) < undispatchedGrace {
			continue // still inside its own dispatch window; not abandoned
		}
		s.reconcileFinalize(ctx, run.ID, types.RunFailed, "", "no sandbox after restart")
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
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "wardynd: PANIC in run watcher sweeper (contained; adoption now boot-only)",
				slog.Any("panic", r))
		}
	}()
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
			if now := s.cfg.Now(); now.Sub(lastUndispatched) >= undispatchedGrace {
				lastUndispatched = now
				if err := s.finalizeUndispatchedRuns(ctx); err != nil {
					slog.WarnContext(ctx, "wardynd: undispatched run reconcile", slog.Any("err", err))
				}
			}
			if err := s.sweepRunWatchers(ctx); err != nil {
				slog.WarnContext(ctx, "wardynd: run watcher sweep", slog.Any("err", err))
			}
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

// reconcileMaxProbeErrors bounds how many CONSECUTIVE AgentStatus probe errors a
// reconcile watcher tolerates before giving up on a persistently-unreachable
// sandbox. A transient error (a docker daemon blip) must NOT finalize a healthy
// RUNNING run — but a permanently-broken ref must not poll forever either. ~1 min
// when probes fail fast at the 5s tick, and up to ~2 min when each one burns the
// full watcherProbeTimeout — that is the number to tune against, since a wedged
// daemon is exactly the case this constant exists for.
const reconcileMaxProbeErrors = 12

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
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	errs := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			st, err := s.probeAgent(ctx, ref, agentExecID)
			if err != nil {
				// A transient probe error is NOT "the run finished" — finalizing here
				// would false-kill a healthy RUNNING run on a docker daemon blip.
				// Tolerate a bounded run of consecutive errors, then give up.
				errs++
				if errs < reconcileMaxProbeErrors {
					continue
				}
				s.reconcileFinalize(ctx, runID, types.RunFailed, ref, "reconciled: sandbox persistently unreachable")
				return
			}
			errs = 0
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

// reconcileFinalize transitions a stranded run to a terminal state — conditional
// on it still being non-terminal so a concurrent kill/complete is never clobbered
// — then runs the revoke cascade and (best-effort) tears the sandbox down.
func (s *Server) reconcileFinalize(ctx context.Context, runID uuid.UUID, to types.RunState, ref, reason string) {
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
