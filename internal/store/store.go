// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package store provides typed CRUD over the Wardyn schema using pgx/v5.
// Callers supply contexts with deadlines. InsertAuditEvent stays a free
// function taking the pool explicitly since it predates a Store value in
// the audit.Recorder wiring.
//
// Naming: Create* inserts and returns the hydrated row; Get* fetches by
// primary key (ErrNotFound when absent); List* returns a slice, empty never
// nil; Update*/Decide* are point mutations with explicit optimistic guards.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// ErrNotFound is returned when a Get* call finds no row.
var ErrNotFound = errors.New("store: not found")

// ErrConflict reports a fenced write losing its race: a source scan slot
// already claimed, or the requirements merge hitting the key cap.
var ErrConflict = errors.New("store: conflict")

// ErrDriveHomeNamespaceConflict is returned by UpsertUserDrive when a host_path
// drive would share a host_root with another host_path drive that derives home
// directory NAMES by a different rule. Wraps ErrConflict (as do
// ErrDriveSlugConflict and ErrDriveAllocated below) so callers asking only
// "is this a 409?" keep working, while the remedy stays distinguishable from a
// plain UNIQUE(name) collision.
var ErrDriveHomeNamespaceConflict = fmt.Errorf(
	"%w: another host_path drive on this host_root derives home directory names by a different rule", ErrConflict)

// ErrDriveSlugConflict is returned by UpsertUserDrive when a drive's name folds
// to a storage-object slug another drive already holds — "Corp NAS" against an
// existing "corp nas", or "Corp NAS (eng)" against "corp-nas-eng". Distinct
// from UNIQUE(name): the name itself is free, but types.DriveSlug(name) — the
// fragment every minted object name is built from — collides, so the remedy is
// a name differing by more than case or punctuation.
var ErrDriveSlugConflict = fmt.Errorf(
	"%w: another user drive's name folds to the same storage-object name", ErrConflict)

// ErrDriveAllocated is returned by UpsertUserDrive when the caller asked for the
// write to apply only while the drive has no allocations (refuseIfAllocated)
// and the row has some. It is the statement-level half of the API's re-home
// guard: since a plain read-then-write would let a grant created in between
// get silently re-homed, the precondition rides the writing statement under a
// FOR UPDATE on the drive row in the same transaction, making it race-free.
var ErrDriveAllocated = fmt.Errorf(
	"%w: this user drive gained an allocation while the write was being prepared", ErrConflict)

// ErrAlreadyDecided is returned when DecideApproval targets an approval that
// has already left PENDING. Fail closed: a second decision never overwrites
// the first.
var ErrAlreadyDecided = types.ErrApprovalAlreadyDecided

// ErrDuplicatePending is returned by CreateApproval when a partial unique index
// rejects a second open PENDING approval for the same dedup key (a concurrent
// raise lost the race). Callers treat it as a dedup signal — re-read and
// return the existing PENDING row — not a hard failure.
var ErrDuplicatePending = types.ErrDuplicatePendingApproval

// Ping proves the pool can actually reach Postgres (a live query round-trip,
// not just a constructed pool).
func (s PG) Ping(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

// AgentRun

// createRunSQL is CreateRun's statement: one placeholder per runInsertCols
// column, in order (TestCreateRunBindsEveryInsertColumn).
var createRunSQL = `
		INSERT INTO agent_runs (` + runInsertCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40)
		RETURNING ` + runCols

// CreateRun inserts a new run and returns the persisted row.
func (s PG) CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error) {
	args, err := s.createRunArgs(r)
	if err != nil {
		return types.AgentRun{}, err
	}
	var out types.AgentRun
	err = s.guarded(ctx, func(q queryRower) (e error) {
		anchor, e := db.CaptureAppClock(ctx, q, s.now)
		if e != nil {
			return e
		}
		if r.UpdatedAt.IsZero() {
			args[2] = anchor.DatabaseAt
		} else {
			args[2] = anchor.Translate(r.UpdatedAt)
		}
		out, e = scanRun(q.QueryRow(ctx, createRunSQL, args...))
		return e
	})
	return out, err
}

// ErrRunCapReached is CreateRunUnderCap's refusal: the deployment already holds
// its cap of non-terminal runs.
var ErrRunCapReached = errors.New("store: deployment run cap reached")

// CreateRunUnderCap is CreateRun with a deployment-wide ceiling on non-terminal
// runs (WARDYN_MAX_CONCURRENT_RUNS). The count and the insert share one
// transaction-scoped advisory lock, so two replicas racing at the cap admit
// exactly the cap. limit <= 0 is unlimited and takes no lock. Rows are counted,
// not kept in a counter, so a crashed path cannot drift it.
func (s PG) CreateRunUnderCap(ctx context.Context, r types.AgentRun, limit int) (types.AgentRun, error) {
	if limit <= 0 {
		return s.CreateRun(ctx, r)
	}
	args, err := s.createRunArgs(r)
	if err != nil {
		return types.AgentRun{}, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.AgentRun{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if g, ok := identityGuardFrom(ctx); ok {
		if err := checkIdentityGuard(ctx, tx, g); err != nil {
			return types.AgentRun{}, err
		}
	}
	active, err := lockAndCountActiveRuns(ctx, tx, uuid.Nil)
	if err != nil {
		return types.AgentRun{}, err
	}
	if active >= limit {
		return types.AgentRun{}, ErrRunCapReached
	}
	anchor, err := db.CaptureAppClock(ctx, tx, s.now)
	if err != nil {
		return types.AgentRun{}, err
	}
	if r.UpdatedAt.IsZero() {
		args[2] = anchor.DatabaseAt
	} else {
		args[2] = anchor.Translate(r.UpdatedAt)
	}
	created, err := scanRun(tx.QueryRow(ctx, createRunSQL, args...))
	if err != nil {
		return types.AgentRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.AgentRun{}, err
	}
	return created, nil
}

// lockAndCountActiveRuns takes the deployment cap's transaction-scoped advisory
// lock and counts the runs it holds (CountNonTerminalRuns' predicate) other than
// except (uuid.Nil for none), in one statement. Every writer that adds a run to
// that count (a create, a kept run's revive) holds the lock from this count to its
// commit. q must be a pgx.Tx.
func lockAndCountActiveRuns(ctx context.Context, q Querier, except uuid.UUID) (int, error) {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, db.RunCapLockClass); err != nil {
		return 0, err
	}
	var active int
	if err := q.QueryRow(ctx, activeRunsCountSQL, nonTerminalStateNames(), except).Scan(&active); err != nil {
		return 0, fmt.Errorf("store: count active runs: %w", err)
	}
	return active, nil
}

// HoldsSandboxSQL is TRUE when the agent_runs row's agent is still running: the run
// is not kept, or it was kept after a control-plane outage and has not reached its
// end, so only its proxy was stopped (api stopLostSandbox). A run kept after its
// end or a reboot, or an outage-kept run past its end, has its agent stopped. It is
// the one rule for a live run under WARDYN_MAX_CONCURRENT_RUNS, in the fleet
// capacity view and under WARDYN_RUN_MAX_AGE.
const HoldsSandboxSQL = `(lost_at IS NULL OR (lost_reason = '` + string(types.LostOutage) + `' AND (ends_at IS NULL OR ends_at > now())))`

const activeRunsCountSQL = `SELECT count(*) FROM agent_runs WHERE state = ANY($1) AND id <> $2 AND ` + HoldsSandboxSQL

// CountNonTerminalRuns is the number of non-terminal run rows that hold a sandbox
// (HoldsSandboxSQL), the quantity CreateRunUnderCap holds under the cap. It takes no
// lock: a pre-flight read for a refusal that must come before an identity is minted,
// never the authority.
func (s PG) CountNonTerminalRuns(ctx context.Context) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, activeRunsCountSQL, nonTerminalStateNames(), uuid.Nil).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count active runs: %w", err)
	}
	return n, nil
}

func (s PG) createRunArgs(r types.AgentRun) ([]any, error) {
	limitsJSON, err := json.Marshal(r.RunLimits)
	if err != nil {
		return nil, fmt.Errorf("store: marshal run limits: %w", err)
	}
	return []any{
		r.ID, r.CreatedAt, r.UpdatedAt, r.CreatedBy, r.Agent, r.Repo, r.Task,
		r.PolicyID, string(r.ConfinementClass), string(r.State),
		r.SPIFFEID, r.RunnerTarget, r.SandboxRef, r.Interactive, r.WorkspacePath, r.WorkspaceID, r.SourceID, r.Image, r.AutoStopAfterSec,
		r.AgentExecID, r.Title, r.Description, r.WorkspaceIDs, string(r.AutonomyLevel),
		r.EndsAt, r.WaitBudgetSec, limitsJSON, r.GovernanceProfileID, r.ModelProviderID, r.UserType,
		r.Preset, r.PresetVersion, r.OperatorOwned, r.CreatedVia, r.DiskMiB,
		string(r.Placement), r.PlacementFilled, r.RunnerID, string(r.EvidenceSource), string(r.Experience),
	}, nil
}

// GetRun returns the run for id, or ErrNotFound.
func (s PG) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	const q = `
		SELECT ` + runCols + `
		FROM agent_runs WHERE id = $1`
	return scanRun(s.Pool.QueryRow(ctx, q, id))
}

// ListRuns returns all runs in reverse creation order (unbounded).
func (s PG) ListRuns(ctx context.Context) ([]types.AgentRun, error) {
	return s.ListRunsPage(ctx, Page{})
}

// CountActiveRunsBy counts createdBy's runs that have not ended yet — the
// governance quota's one read (GovernanceLimits.MaxConcurrentRuns).
//
// The state predicate is a POSITIVE list (types.NonTerminalRunStates), not
// `NOT IN (terminal)`: a state added to the enum and forgotten here merely
// undercounts, whereas the negated form would treat a new terminal state as
// active and wedge a capped member with nothing to stop it. Pinned against
// RunState.IsTerminal by a test in internal/types.
//
// ponytail: no new index — agent_runs_created_by_idx already covers the
// selective column and the state filter is cheap over one member's rows.
func (s PG) CountActiveRunsBy(ctx context.Context, createdBy string) (int, error) {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	var n int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs WHERE created_by = $1 AND state = ANY($2)`,
		createdBy, states,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count active runs: %w", err)
	}
	return n, nil
}

// UpdateRunStateIf conditionally transitions a run from fromState to toState in
// a single UPDATE ... WHERE id=$ AND state=$from, returning whether it applied.
// TOCTOU-safe: a concurrent kill/stop that already moved the run to a terminal
// state is never clobbered. False with a nil error means someone else won the
// transition (or the run doesn't exist); the caller does nothing.
func (s PG) UpdateRunStateIf(ctx context.Context, id uuid.UUID, fromState, toState types.RunState) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now(), ended_at=CASE WHEN $4 THEN now() ELSE ended_at END
		 WHERE id=$2 AND state=$3`,
		string(toState), id, string(fromState), toState.IsTerminal(),
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateRunStateIfIdle is UpdateRunStateIf plus an idleness guard: it transitions
// a run only when still in fromState, updated_at has not advanced past notAfter
// (the caller's snapshot), it has no lost/end mark, and it has no open hold-aware
// request within its wait. Closes the reaper's idleness TOCTOU — an active
// `wardyn run attach` TouchRun can bump updated_at between snapshot and UPDATE,
// so guarding on state alone could stop a now-active run; notAfter makes a
// touched-since-snapshot run no-op instead. The hold-aware clause enforces "idle
// stop never fires while a request is open within its wait" inside the same
// atomic CAS, since a request raised in that window is visible only to a check
// inside the statement.
func (s PG) UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, notAfter time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now(), ended_at=CASE WHEN $5 THEN now() ELSE ended_at END
		 WHERE id=$2 AND state=$3 AND updated_at <= $4 AND lost_at IS NULL AND NOT (`+openHoldSQL+`)`,
		string(toState), id, string(fromState), notAfter, toState.IsTerminal(),
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional idle update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateRunStateIfCreatedBefore transitions a run only when still in fromState,
// created_at is at or before createdNotAfter, and its agent still runs
// (HoldsSandboxSQL: not kept, or kept after an outage before its end; a run kept
// with its agent stopped is left to its files grace). The
// max-age stop's own predicate on the run's age: it carries neither the idleness
// guard nor the open-request guard of UpdateRunStateIfIdle, because an absolute
// age cap exists to end a run that is not going to finish by itself.
func (s PG) UpdateRunStateIfCreatedBefore(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, createdNotAfter time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now(), ended_at=CASE WHEN $5 THEN now() ELSE ended_at END
		 WHERE id=$2 AND state=$3 AND created_at <= $4 AND `+HoldsSandboxSQL,
		string(toState), id, string(fromState), createdNotAfter, toState.IsTerminal(),
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional age update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// openHoldSQL is TRUE when agent_runs (the outer query's row) has a PENDING
// approval that has not yet reached its own min(requested_at+wait, ends_at)
// expiry — an "open request within its wait". A boolean expression, not a full
// statement, so UpdateRunStateIfIdle can splice it into a WHERE under NOT().
const openHoldSQL = `EXISTS (SELECT 1 FROM approvals a WHERE ` + openHoldCond + `)`

// openHoldCond is openHoldSQL's WHERE body, over approvals row "a", shared with
// store_run_pause.go so both use one definition of "open".
const openHoldCond = `a.run_id = agent_runs.id AND a.state = 'PENDING'
		  AND (
			LEAST(a.requested_at + make_interval(secs => NULLIF(agent_runs.wait_budget_sec, 0)), agent_runs.ends_at) IS NULL
			OR LEAST(a.requested_at + make_interval(secs => NULLIF(agent_runs.wait_budget_sec, 0)), agent_runs.ends_at) > now()
		  )`

// execRun is the shared body for the scoped single-column agent_runs writers
// below: Exec, wrap a driver error as "store: <verb>", translate zero rows
// affected into ErrNotFound. UpdateRunStateIf/UpdateRunStateIfIdle deliberately
// skip this: zero rows there is a legitimate guarded no-op, not a missing row.
func (s PG) execRun(ctx context.Context, verb, query string, args ...any) error {
	tag, err := s.Pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: %s: %w", verb, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSandboxRef records the runner reference (container ID / pod name). A
// non-empty ref also records this release as the one that started the run's
// proxy (proxy_release, migration 0084).
func (s PG) SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error {
	return s.execRun(ctx, "set sandbox ref",
		`UPDATE agent_runs SET sandbox_ref=$1, updated_at=now(),
		   proxy_release = CASE WHEN $1 <> '' THEN $3 ELSE proxy_release END
		 WHERE id=$2`, ref, id, version.Version)
}

// SetRunImage scoped-writes ONLY the resolved-image provenance column, once
// the image resolves after the row is inserted.
func (s PG) SetRunImage(ctx context.Context, id uuid.UUID, image string) error {
	return s.execRun(ctx, "set run image",
		`UPDATE agent_runs SET image=$1, updated_at=now() WHERE id=$2`, image, id)
}

// SetRunDiskMiB scoped-writes ONLY the resolved ephemeral-disk-cap column.
// Called once applyEphemeralDisk resolves the effective disk_mib, since that
// needs dispatch-time inputs unavailable at CreateRun.
func (s PG) SetRunDiskMiB(ctx context.Context, id uuid.UUID, mib int) error {
	return s.execRun(ctx, "set run disk mib",
		`UPDATE agent_runs SET disk_mib=$1, updated_at=now() WHERE id=$2`, mib, id)
}

// RunSizing is the configured reservation recorded at dispatch (migration 0117). A nil
// ProxyCPUMillis means the proxy had no CPU cap.
type RunSizing struct {
	RunnerKind            string
	AgentCPURequestMillis int64
	AgentCPULimitMillis   int64
	AgentMemoryRequestMiB int64
	AgentMemoryLimitMiB   int64
	ProxyCPUMillis        *int64
	ProxyMemoryMiB        int64
}

// SetRunSizing scoped-writes ONLY the dispatch-time sizing columns. It does not bump
// updated_at: the record is not activity, and the idle clock reads updated_at.
func (s PG) SetRunSizing(ctx context.Context, id uuid.UUID, z RunSizing) error {
	return s.execRun(ctx, "set run sizing",
		`UPDATE agent_runs SET runner_kind=$1, agent_cpu_request_millis=$2, agent_cpu_limit_millis=$3,
		   agent_memory_request_mib=$4, agent_memory_limit_mib=$5, proxy_cpu_millis=$6, proxy_memory_mib=$7
		 WHERE id=$8`,
		z.RunnerKind, z.AgentCPURequestMillis, z.AgentCPULimitMillis, z.AgentMemoryRequestMiB,
		z.AgentMemoryLimitMiB, z.ProxyCPUMillis, z.ProxyMemoryMiB, id)
}

// SetRunAgentExecID scoped-writes ONLY agent_exec_id, once the driver execs the
// agent. The crash reconciler reads it to observe agent liveness across a restart.
func (s PG) SetRunAgentExecID(ctx context.Context, id uuid.UUID, execID string) error {
	return s.execRun(ctx, "set run agent exec id",
		`UPDATE agent_runs SET agent_exec_id=$1, updated_at=now() WHERE id=$2`, execID, id)
}

// SetRunFailureHint scoped-writes ONLY failure_hint — the one-line operator
// reason a run FAILED before its agent started, known only at the failure site
// (failAndRevoke). Best-effort at the call site; ErrNotFound when no row matched.
func (s PG) SetRunFailureHint(ctx context.Context, id uuid.UUID, hint string) error {
	return s.execRun(ctx, "set run failure hint",
		`UPDATE agent_runs SET failure_hint=$1, updated_at=now() WHERE id=$2`, hint, id)
}

// SetRunStatusDetail scoped-writes ONLY status_detail — what the substrate says
// a STARTING run is waiting on, or the control plane's own `image: Building`
// line on a PENDING run. Fed by runner.SandboxSpec.OnWaiting, once per
// CHANGE of reason. Deliberately does NOT bump updated_at: that column is also
// what the idle reaper and the killed-run tail-upload grace measure by, and a
// diagnostic line a kubelet triggers must never buy a run more life or hold a
// terminal run's upload door open.
func (s PG) SetRunStatusDetail(ctx context.Context, id uuid.UUID, detail string) error {
	return s.execRun(ctx, "set run status detail",
		`UPDATE agent_runs SET status_detail=$1 WHERE id=$2`, detail, id)
}

// TouchRun bumps a run's updated_at to now() without changing any other field —
// the activity keepalive interactive-attach calls so the idle reaper doesn't
// stop a run a human is attached to. The TERMINAL-run guard lives HERE, in one
// WHERE clause, rather than at each of the four callers: updated_at is also the
// clock the killed-run tail-upload grace is measured from, so an authenticated
// caller touching a killed run's row on a cadence could hold that upload door
// open indefinitely. State predicate is the positive list, for the reason given
// on CountActiveRunsBy.
func (s PG) TouchRun(ctx context.Context, id uuid.UUID) error {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	return s.execRun(ctx, "touch run",
		`UPDATE agent_runs SET updated_at=now() WHERE id=$1 AND state = ANY($2)`, id, states)
}

// runInsertCols / runCols are THE agent_runs column lists, in scanRun's order.
// The read list is the write list plus the columns only a scoped UPDATE writes,
// so a column appended to runInsertCols reaches both lists at once.
const runInsertCols = `id, created_at, updated_at, created_by, agent, repo, task, policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id, title, description, workspace_ids, autonomy_level, ` +
	`ends_at, wait_budget_sec, run_limits, governance_profile_id, model_provider_id, user_type, preset, preset_version, operator_owned, created_via, disk_mib, placement, placement_filled, runner_id, evidence_source, experience`
const runCols = runInsertCols + `, failure_hint, status_detail, lost_at, lost_reason, containment_error, containment_error_at, ended_at, paused_at, paused_reason, active_at, end_tightened_at`

// scanRun is the ONE reader for runCols. A new column is appended to
// runInsertCols/runCols and to the end of this Scan — appending is the one edit
// that can't silently transpose two same-typed columns past the compiler.
func scanRun(row pgx.Row) (types.AgentRun, error) {
	var r types.AgentRun
	var cc, state, autonomyLevel, lostReason, pausedReason, placement, evidenceSource, experience string
	var limitsRaw []byte
	var containmentErr *string
	err := row.Scan(
		&r.ID, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy, &r.Agent, &r.Repo, &r.Task,
		&r.PolicyID, &cc, &state,
		&r.SPIFFEID, &r.RunnerTarget, &r.SandboxRef, &r.Interactive, &r.WorkspacePath, &r.WorkspaceID, &r.SourceID, &r.Image, &r.AutoStopAfterSec,
		&r.AgentExecID, &r.Title, &r.Description, &r.WorkspaceIDs, &autonomyLevel,
		&r.EndsAt, &r.WaitBudgetSec, &limitsRaw, &r.GovernanceProfileID, &r.ModelProviderID, &r.UserType,
		&r.Preset, &r.PresetVersion, &r.OperatorOwned, &r.CreatedVia, &r.DiskMiB,
		&placement, &r.PlacementFilled, &r.RunnerID, &evidenceSource, &experience,
		&r.FailureHint, &r.StatusDetail, &r.LostAt, &lostReason, &containmentErr, &r.ContainmentErrorAt, &r.EndedAt,
		&r.PausedAt, &pausedReason, &r.ActiveAt, &r.EndTightenedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.AgentRun{}, ErrNotFound
	}
	if err != nil {
		return types.AgentRun{}, fmt.Errorf("store: scan run: %w", err)
	}
	r.ConfinementClass = types.ConfinementClass(cc)
	r.State = types.RunState(state)
	r.AutonomyLevel = types.AutonomyLevel(autonomyLevel)
	r.LostReason = types.LostReason(lostReason)
	r.PausedReason = types.PauseReason(pausedReason)
	r.Placement = types.Placement(placement)
	r.EvidenceSource = types.RunEvidenceSource(evidenceSource)
	r.Experience = types.RunExperience(experience)
	if containmentErr != nil {
		r.ContainmentError = *containmentErr
	}
	if err := json.Unmarshal(limitsRaw, &r.RunLimits); err != nil {
		return types.AgentRun{}, fmt.Errorf("store: unmarshal run limits: %w", err)
	}
	return r, nil
}

// RunPolicy

// CreatePolicy inserts a policy and returns the persisted row. Returns
// ErrConflict when the name's UNIQUE constraint (run_policies.name) rejects a
// duplicate — the caller maps that to 409, never the raw driver error.
func (s PG) CreatePolicy(ctx context.Context, p types.RunPolicy) (types.RunPolicy, error) {
	specJSON, err := json.Marshal(p.Spec)
	if err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: marshal policy spec: %w", err)
	}
	const q = `
		INSERT INTO run_policies (id, name, created_at, updated_at, spec)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, name, created_at, updated_at, spec`
	out, err := scanPolicy(s.Pool.QueryRow(ctx, q, p.ID, p.Name, p.CreatedAt, p.UpdatedAt, specJSON))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.RunPolicy{}, ErrConflict
		}
		return types.RunPolicy{}, err
	}
	return out, nil
}

// GetPolicy returns the policy for id, or ErrNotFound.
func (s PG) GetPolicy(ctx context.Context, id uuid.UUID) (types.RunPolicy, error) {
	const q = `SELECT id, name, created_at, updated_at, spec FROM run_policies WHERE id = $1`
	return scanPolicy(s.Pool.QueryRow(ctx, q, id))
}

// ListPolicies returns all policies in reverse creation order. The slice is
// empty (never nil) when no policies exist.
func (s PG) ListPolicies(ctx context.Context) ([]types.RunPolicy, error) {
	return s.ListPoliciesPage(ctx, Page{})
}

// UpdatePolicy replaces a policy's name and spec and bumps updated_at. Returns
// ErrNotFound when no policy has the given id, and ErrConflict when the rename
// collides with run_policies.name's UNIQUE constraint — same mapping as
// CreatePolicy, since a rename onto a taken name is the same caller-fixable
// mistake as an insert under one. The caller validates the spec before calling.
func (s PG) UpdatePolicy(ctx context.Context, id uuid.UUID, name string, spec types.RunPolicySpec) (types.RunPolicy, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: marshal policy spec: %w", err)
	}
	const q = `
		UPDATE run_policies SET name=$1, spec=$2, updated_at=now()
		WHERE id=$3
		RETURNING id, name, created_at, updated_at, spec`
	out, err := scanPolicy(s.Pool.QueryRow(ctx, q, name, specJSON, id))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.RunPolicy{}, ErrConflict
		}
		return types.RunPolicy{}, err
	}
	return out, nil
}

// DeletePolicy removes a policy by id. Returns ErrNotFound when no row matched.
// agent_runs.policy_id has no foreign key, so referencing runs keep a dangling
// policy_id; their authorization envelope survives via the run.policy.resolve
// audit event dispatch already recorded.
func (s PG) DeletePolicy(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM run_policies WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("store: delete policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanPolicy(row pgx.Row) (types.RunPolicy, error) {
	var p types.RunPolicy
	var specRaw []byte
	err := row.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt, &specRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunPolicy{}, ErrNotFound
	}
	if err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: scan policy: %w", err)
	}
	if err := json.Unmarshal(specRaw, &p.Spec); err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: unmarshal policy spec: %w", err)
	}
	return p, nil
}

// CredentialGrant

const grantCols = `id, run_id, created_at, spec, delivery`

// CreateGrant inserts a credential grant (eligibility record) and returns it.
func (s PG) CreateGrant(ctx context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	specJSON, err := json.Marshal(g.Spec)
	if err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: marshal grant spec: %w", err)
	}
	const q = `
		INSERT INTO credential_grants (id, run_id, created_at, spec, delivery)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING ` + grantCols
	return scanGrant(s.Pool.QueryRow(ctx, q, g.ID, g.RunID, g.CreatedAt, specJSON, string(g.Delivery)))
}

// ListGrantsByRun returns all grants for a run.
func (s PG) ListGrantsByRun(ctx context.Context, runID uuid.UUID) ([]types.CredentialGrant, error) {
	const q = `SELECT ` + grantCols + ` FROM credential_grants WHERE run_id=$1 ORDER BY created_at, id`
	return collect(ctx, s.Pool, "list", "grants", q, []any{runID}, scanGrant)
}

func scanGrant(row pgx.Row) (types.CredentialGrant, error) {
	var g types.CredentialGrant
	var specRaw []byte
	// No ErrNoRows mapping: the only callers are CreateGrant (INSERT ...
	// RETURNING always yields a row) and the ListGrantsByRun iteration.
	var delivery string
	err := row.Scan(&g.ID, &g.RunID, &g.CreatedAt, &specRaw, &delivery)
	if err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: scan grant: %w", err)
	}
	if err := json.Unmarshal(specRaw, &g.Spec); err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: unmarshal grant spec: %w", err)
	}
	g.Delivery = types.GrantDelivery(delivery)
	return g, nil
}

// ApprovalRequest

// CreateApproval inserts a new approval request. With approval notifications configured it also
// inserts the approval's outbox rows in the same transaction, so no crash can leave an approval nobody
// was told about; without them the INSERT is the only write.
func (s PG) CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	scopeJSON, err := json.Marshal(a.RequestedScope)
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: marshal approval scope: %w", err)
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: begin approval tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var profileID *uuid.UUID
	if notify.Enabled() {
		if _, err := tx.Exec(ctx, notify.BudgetLockSQL, a.RunID.String()); err != nil {
			return types.ApprovalRequest{}, fmt.Errorf("store: lock the run's notification budget: %w", err)
		}
		// Read in the approval's own transaction, so a concurrent profile change cannot route it by stale data.
		if err := tx.QueryRow(ctx, notify.ProfileSQL, a.RunID).Scan(&profileID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return types.ApprovalRequest{}, fmt.Errorf("store: read run profile: %w", err)
		}
	}
	// INSERT omits decision_scope/decision_expires_at: their SQL DEFAULTs already
	// mean "no decision"; RETURNING names them so the caller sees those defaults.
	const q = `
		INSERT INTO approvals (` + approvalInsertCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING ` + approvalCols
	out, err := scanApproval(tx.QueryRow(ctx, q,
		a.ID, a.RunID, a.GrantID, string(a.Kind), scopeJSON, string(a.State), a.RequestedAt,
		a.DecidedAt, a.DecidedBy, a.MintedJTI, a.Reason,
	))
	if err != nil {
		// A partial unique index rejecting the insert means a concurrent raise
		// already persisted the open PENDING row; surface a sentinel so
		// RequestApproval can dedup to the winner instead of erroring. The deferred
		// rollback is what makes the loser enqueue nothing.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.ApprovalRequest{}, ErrDuplicatePending
		}
		return types.ApprovalRequest{}, err
	}
	enq := notify.NewEnqueue(out.Kind, profileID)
	var raised, queued int64
	if enq.On() {
		const src = `WITH ins AS (SELECT $1::uuid AS id, $2::uuid AS run_id, $3::timestamptz AS requested_at)`
		args := append([]any{out.ID, out.RunID, out.RequestedAt}, enq.Args()...)
		if err := tx.QueryRow(ctx, src+enq.Tail(4), args...).Scan(&raised, &queued); err != nil {
			return types.ApprovalRequest{}, fmt.Errorf("store: enqueue approval notifications: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: commit approval: %w", err)
	}
	enq.Done(ctx, out.ID, out.RunID, raised, queued)
	return out, nil
}

// GetApproval returns the approval for id, or ErrNotFound.
func (s PG) GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	const q = `
		SELECT ` + approvalCols + `
		FROM approvals WHERE id = $1`
	return scanApproval(s.Pool.QueryRow(ctx, q, id))
}

// ListApprovals returns approvals filtered by state. Pass empty string to list all.
func (s PG) ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error) {
	return s.ListApprovalsPage(ctx, stateFilter, Page{})
}

// CountApprovalsForRun returns how many approvals a run has raised, in ANY
// state — the per-run DoS bound behind handleInternalRequestApproval
// (maxApprovalsPerRun), since a sandbox picks the hosts it asks about and could
// otherwise raise rows without limit. Counted in the database rather than List
// + filter in Go, which would read every approval row in the deployment.
func (s PG) CountApprovalsForRun(ctx context.Context, runID uuid.UUID) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE run_id = $1`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count approvals for run: %w", err)
	}
	return n, nil
}

// DecideApproval transitions an approval from PENDING to decision.State.
// Returns ErrAlreadyDecided if the approval is not PENDING (fail-closed), via a
// single UPDATE with WHERE state='PENDING' to prevent TOCTOU races. SET is kept
// separate from RETURNING: updating only RETURNING would echo an un-updated row
// back as a green result over a silent no-op.
func (s PG) DecideApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision) (types.ApprovalRequest, error) {
	// The boot-heal newer-action guard compares this with a DB-stamped revert.
	// Freeze the decision before the UPDATE can wait on its row lock.
	decidedAt := s.now()
	anchor, err := db.CaptureAppClock(ctx, s.Pool, s.now)
	if err != nil {
		return types.ApprovalRequest{}, err
	}
	const q = `
		UPDATE approvals
		SET state=$1, decided_at=$2, decided_by=$3, reason=$4, decision_scope=$5, decision_expires_at=$6
		WHERE id=$7 AND state='PENDING'
		RETURNING ` + approvalCols
	a, err := scanApproval(s.Pool.QueryRow(ctx, q,
		string(decision.State), anchor.Translate(decidedAt), decision.DecidedBy, decision.Reason,
		string(decision.Scope), decision.ExpiresAt, id,
	))
	if errors.Is(err, ErrNotFound) {
		// Distinguish "not PENDING" from "doesn't exist" by checking existence.
		var exists bool
		_ = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approvals WHERE id=$1)`, id).Scan(&exists)
		if exists {
			return types.ApprovalRequest{}, ErrAlreadyDecided
		}
		return types.ApprovalRequest{}, ErrNotFound
	}
	return a, err
}

// approvalInsertCols / approvalCols are THE approvals column lists, in
// scanApproval's order. Same shape as agent_runs: the read list adds the
// columns a raise does not set (see CreateApproval).
const approvalInsertCols = `id, run_id, grant_id, kind, requested_scope, state, requested_at, decided_at, decided_by, minted_jti, reason`
const approvalCols = approvalInsertCols + `, decision_scope, decision_expires_at, ` + approvalExpiresAtSQL

// approvalExpiresAtSQL computes a request's expiry from its run's CURRENT end
// and wait: min(requested_at + wait_budget_sec, ends_at); NULL when the run has
// neither bound. Read-time rather than a stored column so a change to the
// run's end or wait reaches open requests with no second write. Every splice
// site must name the approvals table unaliased — the correlated
// approvals.run_id depends on it.
const approvalExpiresAtSQL = `(SELECT LEAST(approvals.requested_at + make_interval(secs => NULLIF(r.wait_budget_sec, 0)), r.ends_at)
		FROM agent_runs r WHERE r.id = approvals.run_id)`

// scanApproval is the ONE reader for approvalCols. A new column is appended to
// approvalInsertCols/approvalCols and to the end of this Scan, same as scanRun.
func scanApproval(row pgx.Row) (types.ApprovalRequest, error) {
	var a types.ApprovalRequest
	var kind, state, decisionScope string
	var scopeRaw []byte
	err := row.Scan(
		&a.ID, &a.RunID, &a.GrantID, &kind, &scopeRaw, &state, &a.RequestedAt,
		&a.DecidedAt, &a.DecidedBy, &a.MintedJTI, &a.Reason, &decisionScope, &a.DecisionExpiresAt, &a.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.ApprovalRequest{}, ErrNotFound
	}
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: scan approval: %w", err)
	}
	a.Kind = types.ApprovalKind(kind)
	a.State = types.ApprovalState(state)
	a.RequestedScope = json.RawMessage(scopeRaw)
	a.DecisionScope = types.ApprovalScope(decisionScope)
	return a, nil
}

// AuditEvent

// InsertAuditEvent appends a single audit event. Implements audit.Recorder.
// The Postgres trigger blocks UPDATE/DELETE; this function only ever INSERTs.
//
// ev is taken by POINTER so the hash chain can be handed back: on success
// ev.PrevHash/ev.RowHash carry the values Postgres computed, and ev.RowHash IS
// the chain head at that instant, which fanoutRecorder emits to the audit sinks
// so an external SIEM ends up holding a head hash Wardyn cannot later disown.
//
// Runs in a transaction because pg_advisory_xact_lock must be held across the
// append: this caller's seq allocation and head read must not interleave with
// another writer's, keeping seq order and chain order identical. audit_append
// takes the same lock itself (the only way a row enters the table since 0111);
// the lock here is re-entrant and free, and keeps the bounded wait below in
// front of it.
func InsertAuditEvent(ctx context.Context, pool *pgxpool.Pool, ev *types.AuditEvent) error {
	// The cap lives at the one INSERT every audit writer reaches (api server,
	// broker, identity, approval sweeper, spool drain), not in Server.auditEvent,
	// which internal/approval bypasses by building its own AuditEvent values.
	ev.Target = CapAuditTarget(ev.Target)
	dataJSON, err := json.Marshal(ev.Data)
	if err != nil {
		return fmt.Errorf("store: marshal audit data: %w", err)
	}
	// Read committed is pinned, not inherited: the trigger's prev_hash head read
	// happens inside THIS transaction, so under REPEATABLE READ a writer queued
	// behind the lock would read a stale snapshot and fork the chain. The
	// isolation level otherwise comes from default_transaction_isolation, a
	// USERSET GUC any role can change, so chain correctness can't depend on it.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin audit tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	// Bound the wait BEFORE asking for the lock, since any open transaction that
	// touched audit_events holds it and this call is on the request path. A
	// timeout is not a lost event: the error reaches spoolingRecorder, which
	// fsyncs it to the local spool for the drain to replay.
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := tx.Exec(ctx, lockAuditChainSQL, db.AuditChainLockKey); err != nil {
		return fmt.Errorf("store: lock audit chain (waited up to %s; another transaction that inserted into audit_events may still be open): %w",
			db.AuditChainLockTimeout, err)
	}
	// audit_append is the only way a row enters audit_events: it allocates seq and recorded_at under
	// the chain lock and the chain trigger refuses a row it did not allocate. COALESCE: the genesis
	// row's prev_hash is SQL NULL, which won't scan into a string; empty string and NULL both mean
	// "nothing before this row".
	const q = `
		SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'')
		FROM audit_append($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	if err := tx.QueryRow(ctx, q,
		ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action,
		ev.Target, ev.Outcome, ev.SourceIP, dataJSON,
	).Scan(&ev.PrevHash, &ev.RowHash); err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit audit event: %w", err)
	}
	return nil
}

// QueryAuditEvents returns audit events for a run in time order.
// limit <= 0 means no explicit limit (returns up to 1000).
func (s PG) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	if limit <= 0 {
		limit = 1000
	}
	return s.QueryAuditEventsPage(ctx, runID, Page{Limit: limit})
}

// QueryRecentAuditEvents returns the most-recent audit events across ALL runs,
// newest first — the global SIEM-style feed the Audit view renders, unlike the
// chronological per-run QueryAuditEvents.
func (s PG) QueryRecentAuditEvents(ctx context.Context, limit int) ([]types.AuditEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	return s.QueryRecentAuditEventsPage(ctx, Page{Limit: limit})
}

// LatestAuditEventByAction returns the most recent audit event whose action
// equals the given action, or ErrNotFound when none exists. Used by /healthz
// to find the latest kernel.sensor.ping driving the eBPF health state.
//
// Most recent by `time`, picked from a seq-ordered window: the audit spool
// replays at-least-once while keeping each event's original ev.Time, so a beat
// spooled through a DB blip could carry the highest seq but an hours-old time
// and falsely mark the sensor degraded. The window orders by seq to keep using
// the purpose-built (action, seq DESC) index; ordering by time directly would
// sort the whole action's history on every probe. Twenty is a window, not a
// proof — a longer replay backlog can push the fresh row out, bounding the
// staleness to one heartbeat interval against /healthz's own TTL.
func (s PG) LatestAuditEventByAction(ctx context.Context, action string) (types.AuditEvent, error) {
	const q = `
		SELECT ` + auditCols + ` FROM (
			SELECT ` + auditCols + `, seq
			FROM audit_events WHERE action=$1 ORDER BY seq DESC LIMIT 20
		) recent ORDER BY time DESC, seq DESC LIMIT 1`
	ev, err := scanAuditEvent(s.Pool.QueryRow(ctx, q, action))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.AuditEvent{}, ErrNotFound
	}
	if err != nil {
		return types.AuditEvent{}, fmt.Errorf("store: latest audit event by action: %w", err)
	}
	return ev, nil
}

// auditCols is THE audit_events column list, in scanAuditEvent's order, shared
// by the insert and every read here. internal/broker keeps its own copy since
// it writes this table through its own pool without importing this package.
const auditCols = `id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data`

func scanAuditEvent(row pgx.Row) (types.AuditEvent, error) {
	var ev types.AuditEvent
	var actorType string
	var dataRaw []byte
	if err := row.Scan(
		&ev.ID, &ev.Time, &ev.RunID, &actorType, &ev.Actor, &ev.Action,
		&ev.Target, &ev.Outcome, &ev.SourceIP, &dataRaw,
	); err != nil {
		return types.AuditEvent{}, err
	}
	ev.ActorType = types.ActorType(actorType)
	if len(dataRaw) > 0 {
		ev.Data = json.RawMessage(dataRaw)
	}
	ev.DeviceID = FederatedDeviceID(ev)
	return ev, nil
}

// SiteConfig

// GetSiteConfig returns the operator-wide site config, or a ZERO-VALUE
// SiteConfig (not an error) when no row has been written yet — first boot has
// no config, and "unconfigured" is a valid, common state rather than a
// failure the caller must special-case.
func (s PG) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	return GetSiteConfigQ(ctx, s.Pool)
}

// GetSiteConfigQ is GetSiteConfig on q.
func GetSiteConfigQ(ctx context.Context, q Querier) (types.SiteConfig, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.SiteConfig{}, nil
	}
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: get site config: %w", err)
	}
	var cfg types.SiteConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: unmarshal site config: %w", err)
	}
	return cfg, nil
}

// PutSiteConfig upserts the single operator-wide site config row. The
// `singleton` primary key (CHECKed true) makes a second row impossible at the
// schema level. A write replaces every key types.SiteConfig declares (no
// partial merge; the API validates the full document first) but keeps any key
// it doesn't, which only a newer wardynd could have written (declaredJSONKeys).
func (s PG) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	return PutSiteConfigQ(ctx, s.Pool, cfg)
}

// PutSiteConfigQ is PutSiteConfig on q. A held governance change applies
// through it inside the decision transaction.
func PutSiteConfigQ(ctx context.Context, q Querier, cfg types.SiteConfig) (types.SiteConfig, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: marshal site config: %w", err)
	}
	const stmt = `
		INSERT INTO site_config (singleton, config, updated_at)
		VALUES (true, $1, now())
		ON CONFLICT (singleton) DO UPDATE
			SET config = (site_config.config - $2::text[]) || EXCLUDED.config, updated_at = now()
		RETURNING config`
	var out []byte
	if err := q.QueryRow(ctx, stmt, raw, declaredJSONKeys(cfg)).Scan(&out); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: put site config: %w", err)
	}
	var saved types.SiteConfig
	if err := json.Unmarshal(out, &saved); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: unmarshal saved site config: %w", err)
	}
	return saved, nil
}
