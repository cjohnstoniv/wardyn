// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package store provides typed CRUD over the Wardyn schema using pgx/v5.
// All writes are serialised through pgxpool; callers supply contexts with
// deadlines. Most operations are methods on PG (see iface.go); InsertAuditEvent
// stays a free function taking the pool explicitly since it predates a Store
// value in the audit.Recorder wiring.
//
// Naming conventions:
//   - Create* inserts and returns the full hydrated row.
//   - Get* fetches by primary key; returns ErrNotFound when absent.
//   - List* returns a slice (empty, never nil) without a hard limit unless
//     stated.
//   - Update*/Decide* are point mutations with explicit optimistic guards.
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
// directory NAMES by a different rule.
//
// It wraps ErrConflict so every caller that only asks "is this a 409?" keeps
// working unchanged; the wrap exists so the ONE caller that writes the sentence
// can tell this refusal apart from UNIQUE(name), which is a different remedy
// (pick another name vs. pick the same home_template, or another root).
var ErrDriveHomeNamespaceConflict = fmt.Errorf(
	"%w: another host_path drive on this host_root derives home directory names by a different rule", ErrConflict)

// ErrDriveSlugConflict is returned by UpsertUserDrive when a drive's name folds
// to a storage-object slug another drive already holds — "Corp NAS" against an
// existing "corp nas", or "Corp NAS (eng)" against "corp-nas-eng".
//
// It is a DIFFERENT refusal from UNIQUE(name) with a different remedy, which is
// why it is its own sentinel: the name really is free, and an admin told "a
// drive named %q already exists" would go looking for a row that is not there.
// What is taken is types.DriveSlug(name) — the fragment every minted object name
// is built from (wardyn-drive-<slug>-<home>) — so the remedy is a name that
// differs by more than case or punctuation.
//
// It wraps ErrConflict for the same reason ErrDriveHomeNamespaceConflict does:
// every caller that only asks "is this a 409?" keeps working unchanged.
var ErrDriveSlugConflict = fmt.Errorf(
	"%w: another user drive's name folds to the same storage-object name", ErrConflict)

// ErrDriveAllocated is returned by UpsertUserDrive when the caller asked for the
// write to apply only while the drive has NO allocations (refuseIfAllocated) and
// the row has some.
//
// It is the STATEMENT-LEVEL half of the API's re-home guard, and it exists
// because that guard is a read followed by an unconditional write: a grant
// created between the two was re-homed silently, exactly as it was before the
// guard existed. The precondition now rides the writing statement, under a
// FOR UPDATE on the drive row taken in the same transaction — which is what
// makes it race-free rather than merely narrow, because the predicate alone
// reads a snapshot an uncommitted grant INSERT is not in (see UpsertUserDrive).
// The guard's own doc named the fix — "the read and the write in ONE
// transaction" — and this is it, without putting a tx handle on the Store
// interface.
//
// It wraps ErrConflict like the two above: every caller that only asks "is this a
// 409?" keeps working, and the ONE caller that writes the sentence can say what
// actually happened — somebody was allocated this drive while it was being
// edited.
var ErrDriveAllocated = fmt.Errorf(
	"%w: this user drive gained an allocation while the write was being prepared", ErrConflict)

// ErrAlreadyDecided is returned when DecideApproval is called on an approval
// that has already left the PENDING state. Fail closed: never allow a second
// decision to silently overwrite the first.
var ErrAlreadyDecided = types.ErrApprovalAlreadyDecided

// ErrDuplicatePending is returned by CreateApproval when a partial unique index
// (approvals_pending_credential_uniq / approvals_pending_noncred_uniq) rejects a
// second open PENDING approval for the same dedup key — i.e. a concurrent raise
// lost the race. Callers treat it as a dedup signal (re-read the existing PENDING
// row and return it), NOT a hard failure. approval.RequestApproval errors.Is-es
// this exact value; both names alias the one sentinel in internal/types.
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
		VALUES ($1,$2,` + db.AppClockAgeSQL("$3") + `,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)
		RETURNING ` + runCols

// CreateRun inserts a new run and returns the persisted row.
func (s PG) CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error) {
	// updated_at ON THE DATABASE'S CLOCK, like every other writer of the column
	// (TouchRun and the six scoped SET ... updated_at=now() writers). It is the
	// idle reaper's measurand, and the reaper now measures against the database's
	// own now(), so the one app-clock writer was the one row whose age carried
	// the daemon/DB skew — 30s of TouchDebounce is the entire margin (B8-F2).
	// Back-dated by the row's own age rather than set to now(), so a caller that
	// stamped the struct earlier in the request keeps that instant.
	updatedAge := int64(0)
	if !r.UpdatedAt.IsZero() {
		updatedAge = db.AppClockAgeMicros(r.UpdatedAt, s.now())
	}
	limitsJSON, err := json.Marshal(r.RunLimits)
	if err != nil {
		return types.AgentRun{}, fmt.Errorf("store: marshal run limits: %w", err)
	}
	row := s.Pool.QueryRow(ctx, createRunSQL,
		r.ID, r.CreatedAt, updatedAge, r.CreatedBy, r.Agent, r.Repo, r.Task,
		r.PolicyID, string(r.ConfinementClass), string(r.State),
		r.SPIFFEID, r.RunnerTarget, r.SandboxRef, r.Interactive, r.WorkspacePath, r.WorkspaceID, r.SourceID, r.Image, r.AutoStopAfterSec,
		r.AgentExecID, r.Title, r.Description, r.WorkspaceIDs, string(r.AutonomyLevel),
		r.EndsAt, r.WaitBudgetSec, limitsJSON, r.GovernanceProfileID, r.ModelProviderID, r.UserType,
	)
	return scanRun(row)
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
// The state predicate is a POSITIVE list (types.NonTerminalRunStates), not a
// `NOT IN (terminal)`: a state added to the enum and forgotten there merely
// undercounts, while the negated form would count a new TERMINAL state as
// active and wedge a capped member at their limit with nothing to stop. That
// list is pinned against RunState.IsTerminal by a test in internal/types.
//
// ponytail: no new index. agent_runs_created_by_idx (0001_init.sql:23) already
// indexes the selective column; the state filter is a cheap check over the few
// rows one member owns. A composite (created_by, state) index is the upgrade if
// a deployment ever has a member with enough historical runs to notice.
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
// a single UPDATE ... WHERE id=$ AND state=$from, returning whether the update
// applied. It is the optimistic guard the completion watcher uses: it only
// transitions a run that is STILL in fromState (e.g. RUNNING), so a concurrent
// kill/stop that already moved the run to a terminal state is never clobbered
// (TOCTOU-safe, like DecideApproval). A false return with a nil error means the
// run existed but was no longer in fromState (or did not exist) — the caller
// treats this as "someone else won the transition" and does nothing.
func (s PG) UpdateRunStateIf(ctx context.Context, id uuid.UUID, fromState, toState types.RunState) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now() WHERE id=$2 AND state=$3`,
		string(toState), id, string(fromState),
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateRunStateIfIdle is UpdateRunStateIf plus an idleness guard: it transitions
// a run from fromState to toState ONLY when the row is still in fromState, its
// updated_at has NOT advanced past notAfter (the snapshot the caller observed),
// AND the run has no open request within its wait (RL-5, hold-aware idle stop).
// This closes the reaper's idleness TOCTOU: the idle scan reads updated_at in a
// snapshot, but an active `wardyn attach` TouchRun (which bumps updated_at while
// leaving state=RUNNING) can land between snapshot and stop. Guarding only on
// state=RUNNING would then stop the now-active run, defeating the keepalive.
// Passing the snapshot's updated_at as notAfter makes a run touched after the
// snapshot no-op the stop (rows-affected 0 => false), so the reaper leaves it be
// and retries on the next tick. Returns (true, nil) when the transition applied.
//
// The hold-aware clause is the SAME guard, at the SAME chokepoint: a run parked
// on a PENDING push/egress/ADO/tool_call request is not idle just because
// nobody has touched it — the design's "idle stop never fires while a request
// is open within its wait" (long-holds-design.md §2.1). It is enforced here,
// inside the CAS itself, rather than as an earlier skip in the reaper's scan
// loop: a request can be raised in the window between the reaper's snapshot and
// this UPDATE executing, and only a check inside the same atomic statement sees
// it. openHoldSQL mirrors approvalExpiresAtSQL's min(requested_at+wait, ends_at)
// in the opposite correlation direction (agent_runs is already the outer query
// here); a NULL result (no run-scoped bound — wait_budget_sec 0 and ends_at
// NULL) means the request is open until the deployment's own approval-expiry
// ceiling reaps it, which is approval.ExpireStale's job, not the idle reaper's.
// That ceiling only exists while its sweeper runs (WARDYN_APPROVAL_EXPIRY_INTERVAL
// > 0); with it disabled, openHoldSQL has no other bound for the NULL case, so
// such a request — and the run parked on it — stays open until decided.
func (s PG) UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, notAfter time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now()
		 WHERE id=$2 AND state=$3 AND updated_at <= $4 AND NOT (`+openHoldSQL+`)`,
		string(toState), id, string(fromState), notAfter,
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional idle update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// openHoldSQL is TRUE when agent_runs (the outer query's row) has a PENDING
// approval that has not yet reached its own min(requested_at+wait, ends_at)
// expiry — an "open request within its wait" (long-holds-design.md §2.1). It
// is a boolean expression, not a full statement, so UpdateRunStateIfIdle can
// splice it straight into a WHERE clause under NOT().
const openHoldSQL = `EXISTS (SELECT 1 FROM approvals a WHERE ` + openHoldCond + `)`

// openHoldCond is openHoldSQL's WHERE body, over the approvals row "a", so
// the pause (store_run_pause.go) can narrow the same definition of "open"
// rather than keep a second copy of it.
const openHoldCond = `a.run_id = agent_runs.id AND a.state = 'PENDING'
		  AND (
			LEAST(a.requested_at + make_interval(secs => NULLIF(agent_runs.wait_budget_sec, 0)), agent_runs.ends_at) IS NULL
			OR LEAST(a.requested_at + make_interval(secs => NULLIF(agent_runs.wait_budget_sec, 0)), agent_runs.ends_at) > now()
		  )`

// execRun is the one body the scoped single-column agent_runs writers below
// share: Exec, wrap a driver error as "store: <verb>", and translate "no row
// matched" into ErrNotFound. verb is the error text a caller matches on, so
// each writer passes its own.
// UpdateRunStateIf/UpdateRunStateIfIdle deliberately do NOT route through here:
// zero rows affected is a legitimate no-op for a guarded transition, not a
// missing row.
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

// SetRunImage scoped-writes ONLY the resolved-image provenance column. Called
// once after image resolution (the image is resolved after the row is
// inserted, so this is a scoped update, not a CreateRun column).
func (s PG) SetRunImage(ctx context.Context, id uuid.UUID, image string) error {
	return s.execRun(ctx, "set run image",
		`UPDATE agent_runs SET image=$1, updated_at=now() WHERE id=$2`, image, id)
}

// SetRunAgentExecID scoped-writes ONLY the agent_exec_id column. Called once
// right after the driver execs the agent (the exec id exists only after Exec, so
// this is a scoped update, not a CreateRun column value). The crash reconciler
// reads it to observe agent liveness across a restart.
func (s PG) SetRunAgentExecID(ctx context.Context, id uuid.UUID, execID string) error {
	return s.execRun(ctx, "set run agent exec id",
		`UPDATE agent_runs SET agent_exec_id=$1, updated_at=now() WHERE id=$2`, execID, id)
}

// SetRunFailureHint scoped-writes ONLY the failure_hint column — the one-line
// operator reason a run FAILED before its agent started (D9). Mirrors
// SetRunImage/SetRunAgentExecID: the hint is known only at the failure site
// (failAndRevoke), after the row exists, so it is a scoped update, not a
// CreateRun value. Best-effort at the call site; ErrNotFound when no row matched.
func (s PG) SetRunFailureHint(ctx context.Context, id uuid.UUID, hint string) error {
	return s.execRun(ctx, "set run failure hint",
		`UPDATE agent_runs SET failure_hint=$1, updated_at=now() WHERE id=$2`, hint, id)
}

// SetRunStatusDetail scoped-writes ONLY the status_detail column — what the
// substrate says a STARTING run is waiting on, in its own words (migration
// 0063). Fed by runner.SandboxSpec.OnWaiting from inside CreateSandbox, once per
// CHANGE of reason.
//
// It does not bump updated_at, and that is load-bearing rather than an
// oversight. agent_runs.updated_at is the clock the idle reaper measures
// idleness by AND the clock the killed-run tail-upload grace is measured from
// (see TouchRun, which exists to bump it, and api/internal_live_run.go). A
// diagnostic line the kubelet triggers must never buy a run more life or hold a
// terminal run's upload door open — so this write is invisible to both.
// Best-effort at the call site; ErrNotFound when no row matched.
func (s PG) SetRunStatusDetail(ctx context.Context, id uuid.UUID, detail string) error {
	return s.execRun(ctx, "set run status detail",
		`UPDATE agent_runs SET status_detail=$1 WHERE id=$2`, detail, id)
}

// TouchRun bumps a run's updated_at to now() without changing any other field.
// It is the activity keepalive the interactive-attach handler calls so the idle
// reaper (which measures idleness by agent_runs.updated_at) does not stop a run
// that a human is actively attached to. Returns ErrNotFound when no row matched.
//
// A TERMINAL run is never touched, and the guard is HERE rather than at
// the four callers (the UI relay, both attach pumps, the SSH channel keepalives)
// because they share one reason and one bug. Each of them touches BEFORE the
// door that refuses a non-RUNNING run, and updated_at is also the clock the
// killed-run tail-upload grace is measured from (api/internal_live_run.go) — so
// an authenticated caller could keep a killed run's row fresh on a cadence and
// hold that door open indefinitely, which is precisely the bound the gate
// claims. One WHERE clause closes every lane, and a caller added later inherits
// it. A refusal is ErrNotFound, which every caller already discards: a
// keepalive for a run that has ended is a no-op by definition.
//
// The predicate is the POSITIVE list (types.NonTerminalRunStates), never
// `NOT IN (terminal)`, for the reason spelled out on CountActiveRunsBy: a state
// added to the enum and forgotten there merely stops a keepalive, while the
// negated form would keep touching a newly-added TERMINAL state.
func (s PG) TouchRun(ctx context.Context, id uuid.UUID) error {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	return s.execRun(ctx, "touch run",
		`UPDATE agent_runs SET updated_at=now() WHERE id=$1 AND state = ANY($2)`, id, states)
}

// runInsertCols / runCols are THE agent_runs column lists, in scanRun's order,
// pasted at six sites until now. The read list is the write list PLUS
// failure_hint — a concatenation, so the one column written by a scoped UPDATE
// (SetRunFailureHint) rather than by CreateRun is visible as exactly that, and
// a column appended to runInsertCols reaches both lists at once.
const runInsertCols = `id, created_at, updated_at, created_by, agent, repo, task, policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id, title, description, workspace_ids, autonomy_level, ` +
	`ends_at, wait_budget_sec, run_limits, governance_profile_id, model_provider_id, user_type`
const runCols = runInsertCols + `, failure_hint, status_detail, lost_at, lost_reason, paused_at, paused_reason, active_at`

// scanRun is the ONE reader for runCols, which is now the ONE spelling of the
// agent_runs column list. A new column is APPENDED to runInsertCols (or to
// runCols alone when a scoped UPDATE rather than CreateRun writes it) and to
// the end of this Scan — appending is still the only edit that cannot silently
// transpose two same-typed columns past the compiler, but there is no longer a
// set of pasted copies to keep in step by hand.
func scanRun(row pgx.Row) (types.AgentRun, error) {
	var r types.AgentRun
	var cc, state, autonomyLevel, lostReason, pausedReason string
	var limitsRaw []byte
	err := row.Scan(
		&r.ID, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy, &r.Agent, &r.Repo, &r.Task,
		&r.PolicyID, &cc, &state,
		&r.SPIFFEID, &r.RunnerTarget, &r.SandboxRef, &r.Interactive, &r.WorkspacePath, &r.WorkspaceID, &r.SourceID, &r.Image, &r.AutoStopAfterSec,
		&r.AgentExecID, &r.Title, &r.Description, &r.WorkspaceIDs, &autonomyLevel,
		&r.EndsAt, &r.WaitBudgetSec, &limitsRaw, &r.GovernanceProfileID, &r.ModelProviderID, &r.UserType,
		&r.FailureHint, &r.StatusDetail, &r.LostAt, &lostReason,
		&r.PausedAt, &pausedReason, &r.ActiveAt,
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

// UpdatePolicy replaces a policy's name and spec and bumps updated_at, returning
// the persisted row. Returns ErrNotFound when no policy has the given id, and
// ErrConflict when the rename collides with run_policies.name's UNIQUE
// constraint — the SAME mapping CreatePolicy has made, because
// the constraint is the same one and a rename onto a taken name is the same
// caller-fixable mistake as an insert under one (B1-F5). Without it the API's
// blanket 500 handed an admin the raw driver text.
//
// The caller is responsible for validating the spec before calling (policies are
// admin-gated config; the API validates every spec before it reaches the store).
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
// Note: agent_runs.policy_id has NO foreign key, so a delete always succeeds even
// while runs still reference the policy — those runs keep a dangling policy_id.
// The run's authorization envelope survives regardless: dispatch records the
// fully-widened spec as a run.policy.resolve event in the append-only audit log.
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

// CreateGrant inserts a credential grant (eligibility record) and returns it.
func (s PG) CreateGrant(ctx context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	specJSON, err := json.Marshal(g.Spec)
	if err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: marshal grant spec: %w", err)
	}
	const q = `
		INSERT INTO credential_grants (id, run_id, created_at, spec)
		VALUES ($1,$2,$3,$4)
		RETURNING id, run_id, created_at, spec`
	return scanGrant(s.Pool.QueryRow(ctx, q, g.ID, g.RunID, g.CreatedAt, specJSON))
}

// ListGrantsByRun returns all grants for a run.
func (s PG) ListGrantsByRun(ctx context.Context, runID uuid.UUID) ([]types.CredentialGrant, error) {
	const q = `SELECT id, run_id, created_at, spec FROM credential_grants WHERE run_id=$1 ORDER BY created_at, id`
	return collect(ctx, s.Pool, "list", "grants", q, []any{runID}, scanGrant)
}

func scanGrant(row pgx.Row) (types.CredentialGrant, error) {
	var g types.CredentialGrant
	var specRaw []byte
	// No ErrNoRows mapping: the only callers are CreateGrant (INSERT ...
	// RETURNING always yields a row) and the ListGrantsByRun iteration.
	err := row.Scan(&g.ID, &g.RunID, &g.CreatedAt, &specRaw)
	if err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: scan grant: %w", err)
	}
	if err := json.Unmarshal(specRaw, &g.Spec); err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: unmarshal grant spec: %w", err)
	}
	return g, nil
}

// ApprovalRequest

// CreateApproval inserts a new approval request.
func (s PG) CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	scopeJSON, err := json.Marshal(a.RequestedScope)
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: marshal approval scope: %w", err)
	}
	// The INSERT list deliberately does NOT name decision_scope /
	// decision_expires_at: a newly-raised approval has no decision yet, and
	// both columns' own DEFAULTs ('' / NULL) are exactly "no decision
	// recorded". The RETURNING list DOES name them, so the caller's
	// ApprovalRequest reflects those defaults rather than the Go zero value
	// of a field that was never assigned.
	const q = `
		INSERT INTO approvals (` + approvalInsertCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING ` + approvalCols
	out, err := scanApproval(s.Pool.QueryRow(ctx, q,
		a.ID, a.RunID, a.GrantID, string(a.Kind), scopeJSON, string(a.State), a.RequestedAt,
		a.DecidedAt, a.DecidedBy, a.MintedJTI, a.Reason,
	))
	if err != nil {
		// A partial unique index (0002 credential / 0022 non-credential) rejecting
		// the insert means a concurrent raise already persisted the open PENDING row.
		// Surface a distinct sentinel so RequestApproval can dedup to the winner
		// instead of erroring.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.ApprovalRequest{}, ErrDuplicatePending
		}
		return types.ApprovalRequest{}, err
	}
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

// CountApprovalsForRun returns how many approvals a run has raised, in ANY state.
// It is the per-run DoS bound behind handleInternalRequestApproval
// (maxApprovalsPerRun): a sandbox picks the hosts it asks about, so without a
// count it can raise rows without limit, and the only reason it had none was that
// api.ApprovalService exposed no way to ask. Counted in the DATABASE — the
// alternative (List + filter in Go) reads every approval row in the deployment on
// every raise, which is the cost this bound exists to avoid.
func (s PG) CountApprovalsForRun(ctx context.Context, runID uuid.UUID) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE run_id = $1`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count approvals for run: %w", err)
	}
	return n, nil
}

// DecideApproval transitions an approval from PENDING to decision.State.
// Returns ErrAlreadyDecided if the approval is not PENDING (fail-closed).
// Uses a single UPDATE with WHERE state='PENDING' to prevent TOCTOU races.
//
// The SET clause below is a SEPARATE list from the RETURNING clause — update
// only the RETURNING and decision.Scope/ExpiresAt would never persist while
// the RETURNING happily echoes the un-updated row back, a green result over a
// silent no-op. Both lists must carry decision_scope/decision_expires_at.
func (s PG) DecideApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision) (types.ApprovalRequest, error) {
	// decided_at ON THE DATABASE'S CLOCK, back-dated by this call's own age —
	// the CreateAPIToken pattern (db.AppClockAgeSQL), and for the same reason.
	// The value is compared against workspaces.egress_edited_at, which Postgres
	// stamps, by the boot heal's ONLY newer-action guard
	// (ReconcileWorkspaceEgressDecisions). Bound from wardynd's clock it carried
	// the daemon/DB skew straight into that inequality: with the daemon running
	// ahead, an operator who approves `always` and then undoes it through the
	// documented PUT seconds later gets the decision RE-APPLIED at the next
	// restart, and the host is back on the allowlist — the durable, fail-OPEN
	// re-widening migration 0055 exists to prevent (B8-F3).
	//
	// The AGE rather than a plain now(), for store_apitokens.go's reason and with
	// its spelling: one definition of the expression, shared with the readers that
	// compare these stamps. Both readings come from s.now(), so the duration
	// carries no skew — and here they are adjacent, so the age is ~0 and the
	// column is effectively now(). The shape is what matters: an admission stamp
	// would slot in unchanged.
	decidedAt := s.now()
	age := db.AppClockAgeMicros(decidedAt, s.now())
	// q is built rather than const for store_apitokens.go's reason: the
	// expression has ONE definition and a const cannot call it.
	q := `
		UPDATE approvals
		SET state=$1, decided_at=` + db.AppClockAgeSQL("$2") + `, decided_by=$3, reason=$4, decision_scope=$5, decision_expires_at=$6
		WHERE id=$7 AND state='PENDING'
		RETURNING ` + approvalCols
	a, err := scanApproval(s.Pool.QueryRow(ctx, q,
		string(decision.State), age, decision.DecidedBy, decision.Reason,
		string(decision.Scope), decision.ExpiresAt, id,
	))
	if errors.Is(err, ErrNotFound) {
		// Row exists but wasn't PENDING, or doesn't exist at all.
		// Distinguish by checking existence.
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
// scanApproval's order (five pasted sites). Same shape as agent_runs: the read
// list adds the two columns a raise does not set. decision_scope /
// decision_expires_at carry their SQL DEFAULTs, which is precisely "no decision
// recorded", and RETURNING names them so the caller sees those rather than the
// Go zero value of a field never assigned.
const approvalInsertCols = `id, run_id, grant_id, kind, requested_scope, state, requested_at, decided_at, decided_by, minted_jti, reason`
const approvalCols = approvalInsertCols + `, decision_scope, decision_expires_at, ` + approvalExpiresAtSQL

// approvalExpiresAtSQL computes a request's expiry from its run's CURRENT end
// and wait: min(requested_at + wait_budget_sec, ends_at). LEAST skips a NULL,
// so a run with only one of the two bounds by that one, and a run with neither
// (wait 0, no end) yields NULL. Read-time rather than a stored column so a
// change to the run's end or wait reaches its open requests with no second
// write, and so no raise path (store.CreateApproval, the broker's own INSERT)
// can forget to set it. Every splice site names the table unaliased, which the
// correlated approvals.run_id relies on.
const approvalExpiresAtSQL = `(SELECT LEAST(approvals.requested_at + make_interval(secs => NULLIF(r.wait_budget_sec, 0)), r.ends_at)
		FROM agent_runs r WHERE r.id = approvals.run_id)`

// scanApproval is the ONE reader for approvalCols, which is now the ONE
// spelling of the approvals column list. A new column is APPENDED to
// approvalInsertCols (or to approvalCols alone when only a decision writes it)
// and to the end of this Scan — appending is still the only edit that cannot
// silently transpose two same-typed columns past the compiler, but there is no
// longer a set of pasted copies to keep in step by hand.
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
// ev is taken by POINTER so the hash chain (migration 0047) can be handed back:
// on success ev.PrevHash/ev.RowHash carry the values Postgres computed, and
// ev.RowHash IS the chain head at that instant. cmd/wardynd's fanoutRecorder
// emits that same value to the audit sinks, which is how an external SIEM ends
// up holding a head hash Wardyn cannot later disown.
//
// It runs in a transaction for ONE reason: pg_advisory_xact_lock must be held
// across the INSERT, so this caller's seq allocation and head read cannot
// interleave with another writer's — keeping seq order and chain order
// identical. Since migration 0056 the trigger takes the same lock and allocates
// seq under it (the identity default's value is discarded), which is what binds
// writers this package knows nothing about; the lock here is re-entrant within
// the transaction and costs nothing. See db.AuditChainLockKey.
func InsertAuditEvent(ctx context.Context, pool *pgxpool.Pool, ev *types.AuditEvent) error {
	// The cap lives here, at the one INSERT every audit writer reaches — the api
	// server, the broker, identity, the approval sweeper and the spool drain
	// alike — rather than in Server.auditEvent, which internal/approval bypasses
	// by building types.AuditEvent values of its own. See CapAuditTarget.
	ev.Target = CapAuditTarget(ev.Target)
	dataJSON, err := json.Marshal(ev.Data)
	if err != nil {
		return fmt.Errorf("store: marshal audit data: %w", err)
	}
	// Read committed is pinned here, not inherited. Since 0056 the head read
	// that decides prev_hash happens INSIDE the trigger, i.e. inside THIS
	// transaction — so under REPEATABLE READ the transaction snapshot, taken by
	// the advisory-lock statement below BEFORE the lock is granted, is the one
	// the head read uses. A writer that queued behind the lock then reads a head
	// from before the winner committed and chains to it: two rows claiming one
	// predecessor, which the verify sweep reports as a break. Executed, not
	// argued: with nothing changed but the isolation level, an in-tree-shaped
	// writer forked the chain.
	//
	// The level came from default_transaction_isolation, a USERSET GUC — settable
	// by any role, per-role or per-database, with no superuser needed and nothing
	// in this tree pinning or checking it. Correctness of the append-only log's
	// link structure must not rest on that. A transaction-level isolation level
	// OVERRIDES the GUC, so setting it here is the fix rather than a request in a
	// runbook; db.ensureAuditTriggers additionally REPORTS a non-read-committed
	// default at boot, for writers this package knows nothing about.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin audit tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	// Bound the wait BEFORE asking for the lock, or the ask has no bound: since
	// 0056 any open transaction that touched audit_events holds this lock, and
	// this call is on the request path. A timeout here is not a lost event - the
	// error travels back to spoolingRecorder, which fsyncs it to the local spool
	// for the drain to replay. See db.AuditChainLockTimeout.
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := tx.Exec(ctx, lockAuditChainSQL, db.AuditChainLockKey); err != nil {
		return fmt.Errorf("store: lock audit chain (waited up to %s; another transaction that inserted into audit_events may still be open): %w",
			db.AuditChainLockTimeout, err)
	}
	// COALESCE: the genesis row's prev_hash is SQL NULL, which will not scan
	// into a string. Empty string and NULL both mean "nothing before this row".
	const q = `
		INSERT INTO audit_events
			(` + auditCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING COALESCE(prev_hash,''), COALESCE(row_hash,'')`
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
// newest first — the global SIEM-style feed the Audit view renders. Per-run
// queries (QueryAuditEvents) stay chronological; this global tail is reverse-
// chronological and bounded by limit.
func (s PG) QueryRecentAuditEvents(ctx context.Context, limit int) ([]types.AuditEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	return s.QueryRecentAuditEventsPage(ctx, Page{Limit: limit})
}

// LatestAuditEventByAction returns the most recent audit event whose action
// equals the given action, or ErrNotFound when none exists. Used by /healthz to
// find the latest kernel.sensor.ping that drives the eBPF ground-truth
// health state (so the stream reports healthy only while beats are arriving).
//
// Most recent by `time`, picked out of a seq-ordered window, and both halves
// are deliberate:
//
//   - `seq DESC` is INSERTION order, and the audit spool replays at-least-once
//     while keeping each event's original ev.Time. A beat spooled through a
//     database blip therefore returns with the HIGHEST seq and an hours-old
//     time, and /healthz — unauthenticated — publishes the eBPF sensor as
//     degraded with a stale last_heartbeat while beats arrive normally (B8-F5).
//
//   - The window stays on `ORDER BY seq DESC`, which is the purpose-built
//     (action, seq DESC) index from 0017, on a hot anonymous path. A plain
//     `ORDER BY time DESC` would abandon it and sort the whole action's history
//     on every probe.
//
//     Twenty is a window, not a proof, and the difference is worth stating: it
//     covers a replay burst of up to twenty rows and costs nineteen extra
//     index-scan rows per probe. A LONGER backlog — the drain replays in
//     batches and loops until the spool clears, so an outage of more than a few
//     heartbeat intervals produces one — pushes the fresh row out of the window
//     and this answers the stale beat again, until the NEXT live beat arrives
//     and takes the top of the window back. That residual is bounded by one
//     heartbeat interval against /healthz's own TTL, and it is exactly the
//     "transient, self-heals at the next beat" the verification accepted for
//     B8-F5; it is pinned as documented behaviour in
//     TestPG_LatestAuditEventByActionAnswersByEventTimeNotInsertionOrder.
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
// by the insert and every read here. internal/broker keeps its own copy — it
// writes this table through its own pool and does not import this package. Not
// used by auditchain.go's audit_row_hash() call: that is the chain function's
// ARGUMENT order (prev_hash first), fixed by the migration, not a column list.
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
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan(&raw)
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

// PutSiteConfig upserts the single operator-wide site config row and returns
// the persisted value. The `singleton` primary key (CHECKed true) makes a
// second row impossible at the schema level; a write REPLACES every key
// types.SiteConfig declares (no partial merge — the API layer decodes and
// validates the full document before calling this) and keeps any key it does
// not, which only a newer wardynd could have written (declaredJSONKeys).
func (s PG) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: marshal site config: %w", err)
	}
	const q = `
		INSERT INTO site_config (singleton, config, updated_at)
		VALUES (true, $1, now())
		ON CONFLICT (singleton) DO UPDATE
			SET config = (site_config.config - $2::text[]) || EXCLUDED.config, updated_at = now()
		RETURNING config`
	var out []byte
	if err := s.Pool.QueryRow(ctx, q, raw, declaredJSONKeys(cfg)).Scan(&out); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: put site config: %w", err)
	}
	var saved types.SiteConfig
	if err := json.Unmarshal(out, &saved); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: unmarshal saved site config: %w", err)
	}
	return saved, nil
}
