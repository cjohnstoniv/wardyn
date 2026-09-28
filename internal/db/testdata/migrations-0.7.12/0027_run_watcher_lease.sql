-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The watcher lease: which control plane is responsible for finishing a run.
--
-- A run's completion watcher is an in-process goroutine blocked on Runner.Wait
-- inside whichever wardynd dispatched it, and re-adoption only ever happened at
-- THAT process's next boot (internal/api/reconcile.go). A pod that never comes
-- back therefore strands its runs non-terminal forever — sandbox up, credentials
-- un-revoked — no matter how many healthy replicas are running.
--
-- watcher_heartbeat is the liveness signal: the goroutine watching a run
-- refreshes it every 30s, and a periodic sweep on EVERY replica claims any
-- non-terminal run whose heartbeat has been silent past the stale window with a
-- single conditional UPDATE ... RETURNING. That atomic claim IS the mutual
-- exclusion: two racing sweeps both re-evaluate the predicate against the row
-- version they blocked on, so exactly one gets the row. Deliberately NOT a
-- per-run pg_try_advisory_lock — db.TryAdvisoryLock borrows a pool connection
-- for the whole hold, so one lock per in-flight run would exhaust the pool.
--
-- watcher_owner is FORENSICS ONLY (which replica adopted the run); it is never
-- a predicate, so it needs no coordination and no uniqueness.
--
-- DEFAULT now() rather than NULL is load-bearing, but only for a run's BIRTH: a
-- row exists for the WHOLE pre-dispatch window (grant writes, then a
-- devcontainer build bounded by imageBuildTimeout = 30 min), and a row born with
-- an already-expired lease would be claimed the instant it appeared. The
-- sandbox_ref guard is what actually protects that window — a run with no
-- sandbox is never claimable — so the default is belt-and-braces there.
--
-- The default does NOT survive a long build. Nothing refreshes the lease while
-- the image builds, so a build longer than the stale window leaves the row
-- claimable the moment sandbox_ref lands, and a sweep would adopt a run whose
-- own dispatch is still setting it up (harmless but inert: the adopted watcher
-- can never observe the exec's exit, and burns a probe + a lease write for the
-- life of the run). The real grace therefore comes from dispatch stamping the
-- lease ONCE right after SetSandboxRef (internal/api/runs_dispatch.go), which
-- buys a full stale window measured from when there is something to watch.
--
-- Existing rows take the migration's timestamp and become claimable one stale
-- window later, which is exactly the intended re-adoption of anything the
-- upgrade left running.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS watcher_owner     TEXT        NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS watcher_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now();

-- Every replica runs the sweep forever and agent_runs grows without bound
-- (0020), so the claim must not re-scan terminal history on every tick. Partial
-- on the NON-terminal states (the complement of types.RunState.IsTerminal) with
-- the heartbeat as the key; ClaimStaleRunWatchers writes its state predicate to
-- match this list verbatim so the planner can prove the implication.
--
-- An index predicate cannot be parameterised, so this list is the one place the
-- set is written out twice. The other is store.nonTerminalRunStates, which
-- carries the full rationale; TestNonTerminalRunStates_MatchesTypes
-- (internal/store/store_watcher_test.go) derives the set from types.RunState and
-- fails if EITHER copy drifts. Edit both, or neither.
CREATE INDEX IF NOT EXISTS agent_runs_watcher_sweep_idx
    ON agent_runs (watcher_heartbeat)
    WHERE state IN ('PENDING','STARTING','RUNNING','WAITING_FOR_CONFIRMATION');
