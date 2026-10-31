// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Workloads and their namespace bindings (migration 0241_workloads). The rows are stored requests and
// pointers; the apply that reads them, and every gate a launch passes, is in internal/api.
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

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MaxWorkloadsPerOwner is the most workloads one owner holds.
const MaxWorkloadsPerOwner = 100

// WorkloadLaunchClaim is how long a claim on a workload's launch holds before another replica may take it.
const WorkloadLaunchClaim = 60 * time.Second

// WorkloadSource says which writer owns a workload: the API and CLI, or a custom resource.
type WorkloadSource string

const (
	WorkloadSourceAPI WorkloadSource = "api"
	WorkloadSourceK8s WorkloadSource = "k8s"
)

// WorkloadWrite is what PutWorkload did: created the row, moved its generation, or found the spec
// unchanged and wrote only the description.
type WorkloadWrite string

const (
	WorkloadCreated   WorkloadWrite = "created"
	WorkloadUpdated   WorkloadWrite = "updated"
	WorkloadUnchanged WorkloadWrite = "unchanged"
	// WorkloadHeld: a custom resource's report of another uid from a runner whose removals do not count
	// (PutWorkload), so nothing was written; the runner that last applied it reports the change itself.
	WorkloadHeld WorkloadWrite = "held"
)

// ErrWorkloadSourceConflict is returned by PutWorkload when the row belongs to the other writer, or to a
// custom resource of another pool or namespace. Wraps ErrConflict.
var ErrWorkloadSourceConflict = fmt.Errorf("%w: the workload belongs to another writer", ErrConflict)

// ErrWorkloadCapReached is returned by PutWorkload when the owner holds MaxWorkloadsPerOwner workloads
// already and the write would add one.
var ErrWorkloadCapReached = errors.New("store: the owner holds the most workloads allowed")

// ErrWorkloadSuperseded is returned by SetWorkloadRun when a launch of the same or a later generation was
// recorded first, or the caller's claim lapsed and was taken again, so the run the caller started is not
// the workload's and must be ended; and by RecordWorkloadRefusal when the caller's claim is no longer the
// workload's, so the refusal is not recorded. Wraps ErrConflict.
var ErrWorkloadSuperseded = fmt.Errorf("%w: a launch of this generation was already recorded", ErrConflict)

// ErrWorkloadStaleSession is returned by a runner report's write (PutWorkload, DeleteK8sWorkload,
// PruneK8sWorkload) when the session the report came in on is not the runner's current inventory session
// (BeginRunnerInventory): a frame of a session a newer one replaced writes nothing, and the runner's next
// full report carries the resource.
var ErrWorkloadStaleSession = errors.New("store: the report came in on a runner session that is no longer current")

// ErrWorkloadBindingConflict is returned by CreateWorkloadBinding when the namespace is bound on the pool
// already. Wraps ErrConflict.
var ErrWorkloadBindingConflict = fmt.Errorf("%w: the namespace is already bound on this pool", ErrConflict)

// WorkloadRefusal is why the last launch attempt of a generation was refused.
type WorkloadRefusal struct {
	Generation int64     `json:"generation"`
	Reason     string    `json:"reason"`
	Message    string    `json:"message,omitempty"`
	At         time.Time `json:"at"`
}

// Workload is one stored workload. The names follow the client's Workload; the fields after Intent are
// the apply's bookkeeping.
type Workload struct {
	ID    uuid.UUID
	Owner string
	Name  string
	// Source is who writes the row. SourcePoolID, SourceNamespace, SourceRunnerID, K8sGeneration and
	// K8sUID are set for a custom resource only; SourcePoolID is nil if its pool row was removed.
	Source          WorkloadSource
	SourcePoolID    *uuid.UUID
	SourceNamespace string
	SourceRunnerID  *uuid.UUID
	K8sGeneration   *int64
	K8sUID          string
	Description     string
	RunIn           string
	RestartKey      string
	// Intent is the door-normalised intent; Digest hashes it with RunIn and RestartKey.
	Intent json.RawMessage
	Digest string
	// Generation counts digest changes; CurrentGeneration is the one CurrentRunID was launched from.
	Generation        int64
	CurrentRunID      *uuid.UUID
	CurrentGeneration int64
	LastRefusal       *WorkloadRefusal
	LaunchClaimUntil  *time.Time
	// ClaimToken names the claim ClaimWorkloadLaunch took; it is set on the workload that call returns only,
	// and SetWorkloadRun and RecordWorkloadRefusal take it back.
	ClaimToken uuid.UUID
	// StatusPushed is the hash of the last status sent to SourceRunnerID.
	StatusPushed string
	CreatedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// WorkloadView is a workload with the state of its current run, as the reads report it: RunState is empty
// while it has no run, and ServiceHealth when the run has no service health yet. RunPausedAt and
// ServiceObservedAt are the run's paused_at and its service health's observed_at, for the API to project
// that health as it projects a run's.
type WorkloadView struct {
	Workload
	RunState          types.RunState
	ServiceHealth     string
	RunPausedAt       *time.Time
	ServiceObservedAt *time.Time
}

// WorkloadPut is the record half of an apply. Digest is the caller's, over Intent, RunIn and RestartKey.
// SourcePoolID, SourceNamespace, SourceRunnerID, K8sGeneration and K8sUID are for a custom resource and
// nil or empty otherwise. By is the writer, kept on a created row.
type WorkloadPut struct {
	Owner           string
	Name            string
	Source          WorkloadSource
	SourcePoolID    *uuid.UUID
	SourceNamespace string
	SourceRunnerID  *uuid.UUID
	K8sGeneration   *int64
	K8sUID          string
	Description     string
	RunIn           string
	RestartKey      string
	Intent          json.RawMessage
	Digest          string
	By              string
	// SourceSession is the session SourceRunnerID's report came in on: when set, the write holds only while
	// it is that runner's current one (ErrWorkloadStaleSession).
	SourceSession string
}

// WorkloadReporter is the runner a custom resource's report came from and the session it came in on. A
// zero Session fences nothing: a write no runner report made.
type WorkloadReporter struct {
	Runner  uuid.UUID
	Session string
}

// workloadSessionFence is ErrWorkloadStaleSession unless session is runner's current inventory session; an
// empty session fences nothing. The runner row is locked first, as RecordRunnerCaps locks it, so a fresh
// session's BeginRunnerInventory waits for this write and a write after it sees that session.
func workloadSessionFence(ctx context.Context, q Querier, runner uuid.UUID, session string) error {
	if session == "" {
		return nil
	}
	if _, err := q.Exec(ctx, `SELECT 1 FROM runners WHERE id = $1 FOR NO KEY UPDATE`, runner); err != nil {
		return err
	}
	var current bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_inventories WHERE runner_id = $1 AND session_id::text = $2::text)`,
		runner, session).Scan(&current); err != nil {
		return err
	}
	if !current {
		return ErrWorkloadStaleSession
	}
	return nil
}

// workloadSourceCountsSQL is TRUE when runner $n's report about workloads row w counts as its resource's
// removal (a prune, or a report of another uid): w names no source runner, $n is the runner that last
// applied it (source_runner_id), or that runner has left the row's pool, so another may take over. One
// runner's reports are in order; two runners' are not, so another runner's snapshot can be older than the
// source's last apply.
func workloadSourceCountsSQL(n int) string {
	p := fmt.Sprintf("$%d", n)
	return `(w.source_runner_id IS NULL OR w.source_runner_id = ` + p + ` OR NOT EXISTS (SELECT 1 FROM runner_pool_members m
		WHERE m.pool_id = w.source_pool_id AND m.runner_id = w.source_runner_id))`
}

// WorkloadBinding authorises the custom resources of one namespace, on the runners of one pool, to
// launch as a service identity. Sponsor is the identity's sponsor when the binding was made.
type WorkloadBinding struct {
	ID        uuid.UUID
	PoolID    uuid.UUID
	Namespace string
	Service   string
	Sponsor   string
	CreatedBy string
	CreatedAt time.Time
}

// WorkloadStore is the optional store capability behind the workload routes: a store without it answers
// them 501, as RunnerPoolStore does.
type WorkloadStore interface {
	PutWorkload(ctx context.Context, in WorkloadPut) (Workload, WorkloadWrite, error)
	ClaimWorkloadLaunch(ctx context.Context, id uuid.UUID) (Workload, bool, error)
	SetWorkloadRun(ctx context.Context, id, claim uuid.UUID, generation int64, runID uuid.UUID) (*uuid.UUID, error)
	RecordWorkloadRefusal(ctx context.Context, id, claim uuid.UUID, r WorkloadRefusal) error
	RestartWorkload(ctx context.Context, id uuid.UUID) (Workload, error)
	SetWorkloadStatusPushed(ctx context.Context, id uuid.UUID, hash string) error
	GetWorkload(ctx context.Context, owner, name string) (WorkloadView, error)
	ListWorkloads(ctx context.Context, p Page) ([]WorkloadView, error)
	ListWorkloadsOf(ctx context.Context, owners []string, p Page) ([]WorkloadView, error)
	ListK8sWorkloadStatuses(ctx context.Context) ([]WorkloadView, error)
	ListLapsedK8sWorkloadLaunches(ctx context.Context, limit int) ([]Workload, error)
	ListBoundK8sWorkloads(ctx context.Context, poolID uuid.UUID, namespace string) ([]Workload, error)
	DeleteWorkload(ctx context.Context, owner, name string) (Workload, error)
	DeleteK8sWorkload(ctx context.Context, w Workload, by WorkloadReporter) (Workload, error)
	PruneK8sWorkload(ctx context.Context, w Workload, by WorkloadReporter) (Workload, error)
	OweWorkloadRunEnd(ctx context.Context, e WorkloadRunEnd) error
	ListWorkloadRunEnds(ctx context.Context, olderThan time.Duration, limit int) ([]WorkloadRunEnd, error)
	DropWorkloadRunEnd(ctx context.Context, runID uuid.UUID) error
	RestoreWorkloadRun(ctx context.Context, e WorkloadRunEnd, refusal WorkloadRefusal) (bool, error)
	CreateWorkloadBinding(ctx context.Context, b WorkloadBinding) (WorkloadBinding, error)
	ListWorkloadBindings(ctx context.Context, p Page) ([]WorkloadBinding, error)
	DeleteWorkloadBinding(ctx context.Context, id uuid.UUID) (WorkloadBinding, error)
	BindingForRunner(ctx context.Context, runner uuid.UUID, namespace string) (WorkloadBinding, error)
}

var _ WorkloadStore = PG{}

const workloadCols = `w.id, w.owner, w.name, w.source, w.source_pool_id, COALESCE(w.source_namespace, ''), w.source_runner_id,
	w.k8s_generation, w.description, w.run_in, w.restart_key, w.intent, w.digest, w.generation, w.current_run_id,
	w.current_generation, w.last_refusal, w.launch_claim_until, w.status_pushed, w.created_by, w.created_at, w.updated_at, w.k8s_uid`

// workloadViewCols and workloadViewFrom read a workload with its current run. The run's service health is
// read by name out of the row's JSON, so this query does not depend on the order the migration that adds
// that column lands in.
const (
	workloadViewCols = workloadCols + `, COALESCE(r.state, ''), COALESCE(to_jsonb(r) -> 'service_status' ->> 'health', ''), r.paused_at,
		(to_jsonb(r) -> 'service_status' ->> 'observed_at')::timestamptz`
	workloadViewFrom = ` FROM workloads w LEFT JOIN agent_runs r ON r.id = w.current_run_id`
)

func scanWorkloadInto(row pgx.Row, w *Workload, extra ...any) error {
	var refusal []byte
	err := row.Scan(append([]any{&w.ID, &w.Owner, &w.Name, &w.Source, &w.SourcePoolID, &w.SourceNamespace, &w.SourceRunnerID,
		&w.K8sGeneration, &w.Description, &w.RunIn, &w.RestartKey, &w.Intent, &w.Digest, &w.Generation, &w.CurrentRunID,
		&w.CurrentGeneration, &refusal, &w.LaunchClaimUntil, &w.StatusPushed, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt, &w.K8sUID}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: scan workload: %w", err)
	}
	if len(refusal) > 0 {
		w.LastRefusal = &WorkloadRefusal{}
		if err := json.Unmarshal(refusal, w.LastRefusal); err != nil {
			return fmt.Errorf("store: decode workload refusal: %w", err)
		}
	}
	return nil
}

func scanWorkload(row pgx.Row) (Workload, error) {
	var w Workload
	err := scanWorkloadInto(row, &w)
	return w, err
}

func scanWorkloadView(row pgx.Row) (WorkloadView, error) {
	var v WorkloadView
	err := scanWorkloadInto(row, &v.Workload, &v.RunState, &v.ServiceHealth, &v.RunPausedAt, &v.ServiceObservedAt)
	return v, err
}

func sameUUIDPtr(a, b *uuid.UUID) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// PutWorkload records a workload, for one writer. It holds the owner's lock, so two concurrent applies of
// the same spec leave one created row and one generation, and the cap cannot be passed by racing creates.
//   - No row: it is created at generation 1 (WorkloadCreated), or ErrWorkloadCapReached.
//   - A row of the other source, or for a custom resource of another pool or namespace:
//     ErrWorkloadSourceConflict, and nothing is written.
//   - A custom resource of the stored metadata.uid at a lower metadata.generation than stored: a late
//     report from a stale cache, answered WorkloadUnchanged with nothing written, whatever its digest.
//     Generations are compared within one uid only: a new uid is a new resource.
//   - A custom resource of another metadata.uid, from a runner whose removals do not count
//     (workloadSourceCountsSQL): its report may be older than the stored one, so it is WorkloadHeld and
//     nothing is written. The source runner's own report of the new uid, or of the old one's delete,
//     moves the row.
//   - A report whose session is not its runner's current one: ErrWorkloadStaleSession, nothing written.
//   - A different digest: the spec is replaced and the generation moves by one (WorkloadUpdated).
//   - The same digest: only the description is written, and for a custom resource the reporting runner,
//     metadata.generation and metadata.uid (WorkloadUnchanged).
//
// It never touches the run columns: whether a launch is owed is the generation against CurrentGeneration.
func (s PG) PutWorkload(ctx context.Context, in WorkloadPut) (Workload, WorkloadWrite, error) {
	var out Workload
	var write WorkloadWrite
	err := s.inTx(ctx, func(q Querier) error {
		if in.SourceSession != "" {
			if in.SourceRunnerID == nil {
				return errors.New("store: put workload: a fenced report names no runner")
			}
			if err := workloadSessionFence(ctx, q, *in.SourceRunnerID, in.SourceSession); err != nil {
				return err
			}
		}
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text))`, "workloads.put/"+in.Owner); err != nil {
			return err
		}
		cur, err := scanWorkload(q.QueryRow(ctx, `SELECT `+workloadCols+` FROM workloads w WHERE w.owner = $1 AND w.name = $2 FOR UPDATE`, in.Owner, in.Name))
		switch {
		case errors.Is(err, ErrNotFound):
			var n int
			if err := q.QueryRow(ctx, `SELECT count(*) FROM workloads WHERE owner = $1`, in.Owner).Scan(&n); err != nil {
				return err
			}
			if n >= MaxWorkloadsPerOwner {
				return ErrWorkloadCapReached
			}
			var ns *string
			if in.SourceNamespace != "" {
				ns = &in.SourceNamespace
			}
			out, err = scanWorkload(q.QueryRow(ctx, `
				INSERT INTO workloads AS w (owner, name, source, source_pool_id, source_namespace, source_runner_id, k8s_generation,
					description, run_in, restart_key, intent, digest, created_by, k8s_uid)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
				RETURNING `+workloadCols,
				in.Owner, in.Name, in.Source, in.SourcePoolID, ns, in.SourceRunnerID, in.K8sGeneration,
				in.Description, in.RunIn, in.RestartKey, []byte(in.Intent), in.Digest, in.By, in.K8sUID))
			write = WorkloadCreated
			return err
		case err != nil:
			return err
		}
		if cur.Source != in.Source || in.Source == WorkloadSourceK8s &&
			(!sameUUIDPtr(cur.SourcePoolID, in.SourcePoolID) || cur.SourceNamespace != in.SourceNamespace) {
			return ErrWorkloadSourceConflict
		}
		if in.Source == WorkloadSourceK8s && in.K8sUID != "" && in.K8sUID == cur.K8sUID &&
			in.K8sGeneration != nil && cur.K8sGeneration != nil && *in.K8sGeneration < *cur.K8sGeneration {
			// Another runner's informer cache, behind this one's: the stored generation stands.
			out, write = cur, WorkloadUnchanged
			return nil
		}
		if in.Source == WorkloadSourceK8s && in.K8sUID != "" && cur.K8sUID != "" && in.K8sUID != cur.K8sUID && in.SourceRunnerID != nil {
			var counts bool
			if err := q.QueryRow(ctx, `SELECT `+workloadSourceCountsSQL(2)+` FROM workloads w WHERE w.id = $1`,
				cur.ID, *in.SourceRunnerID).Scan(&counts); err != nil {
				return err
			}
			if !counts {
				out, write = cur, WorkloadHeld
				return nil
			}
		}
		if cur.Digest != in.Digest {
			out, err = scanWorkload(q.QueryRow(ctx, `
				UPDATE workloads w SET intent = $2, digest = $3, run_in = $4, restart_key = $5, description = $6,
					source_runner_id = $7, k8s_generation = $8, k8s_uid = $9, generation = w.generation + 1, updated_at = now()
				WHERE w.id = $1 RETURNING `+workloadCols,
				cur.ID, []byte(in.Intent), in.Digest, in.RunIn, in.RestartKey, in.Description, in.SourceRunnerID, in.K8sGeneration, in.K8sUID))
			write = WorkloadUpdated
			return err
		}
		out, err = scanWorkload(q.QueryRow(ctx, `
			UPDATE workloads w SET description = $2, source_runner_id = $3, k8s_generation = $4, k8s_uid = $5,
				updated_at = CASE WHEN w.description <> $2 THEN now() ELSE w.updated_at END
			WHERE w.id = $1 RETURNING `+workloadCols,
			cur.ID, in.Description, in.SourceRunnerID, in.K8sGeneration, in.K8sUID))
		write = WorkloadUnchanged
		return err
	})
	if err != nil {
		return Workload{}, "", err
	}
	return out, write, nil
}

// ClaimWorkloadLaunch takes the right to launch the workload's current generation for WorkloadLaunchClaim.
// It reports false, with no error, when none is owed (the current generation was launched) or another
// replica holds the claim. A claim a crash left lapses by itself. The workload it returns is the one to
// launch: its Generation is the generation, and its ClaimToken the claim, to hand to SetWorkloadRun or
// RecordWorkloadRefusal.
func (s PG) ClaimWorkloadLaunch(ctx context.Context, id uuid.UUID) (Workload, bool, error) {
	var w Workload
	err := scanWorkloadInto(s.Pool.QueryRow(ctx, `
		UPDATE workloads w SET launch_claim_until = now() + make_interval(secs => $2::float8), launch_claim_token = gen_random_uuid()
		WHERE w.id = $1 AND w.current_generation < w.generation AND (w.launch_claim_until IS NULL OR w.launch_claim_until < now())
		RETURNING `+workloadCols+`, w.launch_claim_token`, id, WorkloadLaunchClaim.Seconds()), &w, &w.ClaimToken)
	if errors.Is(err, ErrNotFound) {
		return Workload{}, false, nil
	}
	return w, err == nil, err
}

// workloadRunRanSQL is whether the agent_runs row tbl took its slot: it holds one now (a state in the
// array parameter states), or it was admitted from its queue and the start did not refuse it ("queue: "
// status detail). A run that waited and ended without either never ran.
func workloadRunRanSQL(tbl, states string) string {
	return `(` + tbl + `.state = ANY(` + states + `) OR (` + tbl + `.admitted_at IS NOT NULL AND ` + tbl + `.status_detail NOT LIKE 'queue: %'))`
}

// SetWorkloadRun records that runID is the run of the workload's given generation, launched under claim: it
// becomes the current run, the generation current, the last refusal is cleared and the claim released. It
// returns the run it replaces, nil if there was none, for the caller to end; the same transaction records
// that run as owed its end (WorkloadRunEnd), so an end the caller does not finish is still owed. When runID
// waited in its pool's queue its start can still be refused, so the run it replaces, while it runs, is not
// handed back: its owed end waits on runID (AfterRunID). Unless it holds a slot in runID's pool: runID
// could not start while it runs, so it is handed back to end now. A run it replaces that never ran is
// handed back, and a run already waiting on it waits on runID instead; once it ran, that older run is owed
// its end at once.
// ErrNotFound when the workload is gone, and ErrWorkloadSuperseded when claim is no longer the workload's
// (it lapsed and was taken again, or its holder already recorded) or that generation, or a later one, was
// recorded first; the caller ends runID (OweWorkloadRunEnd).
func (s PG) SetWorkloadRun(ctx context.Context, id, claim uuid.UUID, generation int64, runID uuid.UUID) (*uuid.UUID, error) {
	var prev *uuid.UUID
	var deferred bool
	err := s.inTx(ctx, func(q Querier) error {
		var cur int64
		var token *uuid.UUID
		var owner, name string
		err := q.QueryRow(ctx, `SELECT current_run_id, current_generation, launch_claim_token, owner, name FROM workloads WHERE id = $1 FOR UPDATE`,
			id).Scan(&prev, &cur, &token, &owner, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if token == nil || *token != claim || cur >= generation {
			return ErrWorkloadSuperseded
		}
		if _, err = q.Exec(ctx, `
			UPDATE workloads SET current_run_id = $2, current_generation = $3, last_refusal = NULL, launch_claim_until = NULL,
				launch_claim_token = NULL
			WHERE id = $1`, id, runID, generation); err != nil {
			return err
		}
		if prev == nil || *prev == runID {
			return nil
		}
		// prev serves while it holds a slot, and took over once it ran (workloadRunRanSQL). runID waits for
		// the run that serves (prev, or else the older run that waited on prev) only when it waited in its
		// queue and that run holds no slot in its pool: a replacement queued behind its own workload's run
		// could never start, so that run ends now (a replace in place, with a gap).
		var serving, ran, waited bool
		if err := q.QueryRow(ctx, `SELECT state = ANY($2), `+workloadRunRanSQL("agent_runs", "$2")+` FROM agent_runs WHERE id = $1`,
			*prev, slotHoldingStateNames()).Scan(&serving, &ran); err != nil {
			return err
		}
		if err := q.QueryRow(ctx, `
			SELECT (n.state = 'QUEUED' OR n.admitted_at IS NOT NULL) AND NOT EXISTS (
				SELECT 1 FROM agent_runs o WHERE o.runner_pool_id = n.runner_pool_id AND o.state = ANY($3) AND `+HoldsSandboxSQL+`
					AND CASE WHEN $4 THEN o.id = $2 ELSE o.id IN (SELECT run_id FROM workload_run_ends WHERE after_run_id = $2) END)
			FROM agent_runs n WHERE n.id = $1`, runID, *prev, slotHoldingStateNames(), ran).Scan(&waited); err != nil {
			return err
		}
		var after *uuid.UUID
		if waited {
			after = &runID
		}
		// An older run waiting on prev: prev took over from it once it ran, so it ends at once; while prev
		// never ran, it waits on runID instead, or ends at once when runID did not wait.
		waiters := after
		if ran {
			waiters = nil
		}
		if _, err := q.Exec(ctx, `UPDATE workload_run_ends SET after_run_id = $2 WHERE after_run_id = $1`, *prev, waiters); err != nil {
			return err
		}
		end := WorkloadRunEnd{RunID: *prev, Owner: owner, Name: name, Generation: generation, Reason: WorkloadEndReplaced}
		if serving && waited {
			end.AfterRunID, deferred = after, true // prev ends once runID runs
		}
		return oweWorkloadRunEnd(ctx, q, end)
	})
	if err != nil || deferred {
		return nil, err
	}
	return prev, nil
}

// RecordWorkloadRefusal stores why the launch under claim was refused and releases the claim, so the next
// apply tries again. The current run is untouched: a spec the gates refuse never ends a working service.
// ErrNotFound when the workload is gone, and ErrWorkloadSuperseded, with nothing written, when claim is no
// longer the workload's: a later claimer's claim and outcome stand.
func (s PG) RecordWorkloadRefusal(ctx context.Context, id, claim uuid.UUID, r WorkloadRefusal) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("store: encode workload refusal: %w", err)
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE workloads SET last_refusal = $3, launch_claim_until = NULL, launch_claim_token = NULL
		WHERE id = $1 AND launch_claim_token = $2`, id, claim, b)
	if err != nil {
		return fmt.Errorf("store: record workload refusal: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workloads WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("store: record workload refusal: %w", err)
	}
	if exists {
		return ErrWorkloadSuperseded
	}
	return ErrNotFound
}

// RestartWorkload marks the workload's spec as not launched, so the next claim starts a fresh run of it
// (the old run is ended once that run is recorded). It leaves a claim in flight alone. ErrNotFound when
// the workload is gone.
func (s PG) RestartWorkload(ctx context.Context, id uuid.UUID) (Workload, error) {
	return scanWorkload(s.Pool.QueryRow(ctx, `UPDATE workloads w SET current_generation = 0 WHERE w.id = $1 RETURNING `+workloadCols, id))
}

// SetWorkloadStatusPushed records the hash of the status just sent to the workload's runner. It does not
// touch updated_at. ErrNotFound when the workload is gone.
func (s PG) SetWorkloadStatusPushed(ctx context.Context, id uuid.UUID, hash string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE workloads SET status_pushed = $2 WHERE id = $1`, id, hash)
	if err != nil {
		return fmt.Errorf("store: set workload status pushed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetWorkload returns one owner's workload by name, with its run's state, or ErrNotFound.
func (s PG) GetWorkload(ctx context.Context, owner, name string) (WorkloadView, error) {
	return scanWorkloadView(s.Pool.QueryRow(ctx, `SELECT `+workloadViewCols+workloadViewFrom+` WHERE w.owner = $1 AND w.name = $2`, owner, name))
}

// ListWorkloads returns every owner's workloads, by owner then name.
func (s PG) ListWorkloads(ctx context.Context, p Page) ([]WorkloadView, error) {
	q, args := p.appendTo(`SELECT `+workloadViewCols+workloadViewFrom+` ORDER BY w.owner, w.name`, nil)
	return collect(ctx, s.Pool, "list", "workloads", q, args, scanWorkloadView)
}

// ListWorkloadsOf returns the workloads of the named owners, by owner then name. No owners, no workloads.
func (s PG) ListWorkloadsOf(ctx context.Context, owners []string, p Page) ([]WorkloadView, error) {
	if len(owners) == 0 {
		return []WorkloadView{}, nil
	}
	q, args := p.appendTo(`SELECT `+workloadViewCols+workloadViewFrom+` WHERE w.owner = ANY($1) ORDER BY w.owner, w.name`, []any{owners})
	return collect(ctx, s.Pool, "list", "owned workloads", q, args, scanWorkloadView)
}

// ListK8sWorkloadStatuses returns every custom-resource workload with its current run's state and service
// health: what the status sweep hashes against StatusPushed.
func (s PG) ListK8sWorkloadStatuses(ctx context.Context) ([]WorkloadView, error) {
	return collect(ctx, s.Pool, "list", "k8s workload statuses",
		`SELECT `+workloadViewCols+workloadViewFrom+` WHERE w.source = 'k8s' ORDER BY w.id`, nil, scanWorkloadView)
}

// ListBoundK8sWorkloads returns the custom-resource workloads the resources of namespace recorded on the
// pool poolID: the rows a runner's full list of that namespace, under its binding on that pool, answers for.
func (s PG) ListBoundK8sWorkloads(ctx context.Context, poolID uuid.UUID, namespace string) ([]Workload, error) {
	return collect(ctx, s.Pool, "list", "bound k8s workloads", `SELECT `+workloadCols+` FROM workloads w
		WHERE w.source = 'k8s' AND w.source_pool_id = $1 AND w.source_namespace = $2 ORDER BY w.id`, []any{poolID, namespace}, scanWorkload)
}

// ListLapsedK8sWorkloadLaunches returns up to limit custom-resource workloads whose launch is owed under a
// claim that lapsed: a launch that took its claim and neither recorded a run nor a refusal (its replica went
// away, or the launch failed). The oldest lapse first. A refused launch, or one never claimed, is not one.
func (s PG) ListLapsedK8sWorkloadLaunches(ctx context.Context, limit int) ([]Workload, error) {
	return collect(ctx, s.Pool, "list", "lapsed workload launches", `SELECT `+workloadCols+` FROM workloads w
		WHERE w.source = 'k8s' AND w.current_generation < w.generation AND w.launch_claim_until < now()
		ORDER BY w.launch_claim_until, w.id LIMIT $1`, []any{limit}, scanWorkload)
}

// DeleteWorkload removes one owner's workload and returns the removed row, whose CurrentRunID the caller
// ends, or ErrNotFound. The run itself is not touched here; the same transaction records it as owed its
// end (WorkloadRunEnd).
func (s PG) DeleteWorkload(ctx context.Context, owner, name string) (Workload, error) {
	return s.deleteWorkload(ctx, WorkloadReporter{}, `w.owner = $1 AND w.name = $2`, owner, name)
}

// DeleteK8sWorkload is DeleteWorkload for w, a custom resource's workload as the caller read it, and only
// while the row is still that resource's: the same row id, metadata.uid, pool and namespace. ErrNotFound,
// with nothing deleted and no end owed, when the row is gone or an apply of a recreated resource has moved
// it to another uid since the read. by is the runner whose delete report it is (ErrWorkloadStaleSession):
// a uid's delete counts from any runner of the pool, since a deleted uid never comes back.
func (s PG) DeleteK8sWorkload(ctx context.Context, w Workload, by WorkloadReporter) (Workload, error) {
	return s.deleteWorkload(ctx, by, k8sWorkloadRowSQL, w.ID, w.K8sUID, w.SourcePoolID, w.SourceNamespace)
}

// PruneK8sWorkload is DeleteK8sWorkload for a row by's full list of the namespace no longer names, and only
// while by's removals count for it (workloadSourceCountsSQL), checked in the delete itself: an apply by
// another runner that lands between the list's read and the delete keeps the row. ErrNotFound otherwise.
func (s PG) PruneK8sWorkload(ctx context.Context, w Workload, by WorkloadReporter) (Workload, error) {
	return s.deleteWorkload(ctx, by, k8sWorkloadRowSQL+` AND `+workloadSourceCountsSQL(5),
		w.ID, w.K8sUID, w.SourcePoolID, w.SourceNamespace, by.Runner)
}

// k8sWorkloadRowSQL matches a custom resource's workloads row as it was read: id, uid, pool and namespace.
const k8sWorkloadRowSQL = `w.id = $1 AND w.source = 'k8s' AND w.k8s_uid = $2 AND w.source_pool_id = $3 AND w.source_namespace = $4`

// deleteWorkload deletes the workload where matches and owes its current run's end in the same transaction,
// and at once the end of a run that waited on it to take over. A runner's report (by) writes only on that
// runner's current session (workloadSessionFence).
func (s PG) deleteWorkload(ctx context.Context, by WorkloadReporter, where string, args ...any) (Workload, error) {
	var w Workload
	err := s.inTx(ctx, func(q Querier) error {
		if err := workloadSessionFence(ctx, q, by.Runner, by.Session); err != nil {
			return err
		}
		var err error
		w, err = scanWorkload(q.QueryRow(ctx, `DELETE FROM workloads w WHERE `+where+` RETURNING `+workloadCols, args...))
		if err != nil || w.CurrentRunID == nil {
			return err
		}
		// A run still waiting on the current one to take over from it has nothing left to wait for.
		if _, err = q.Exec(ctx, `UPDATE workload_run_ends SET after_run_id = NULL, reason = $2 WHERE after_run_id = $1`,
			*w.CurrentRunID, WorkloadEndDeleted); err != nil {
			return err
		}
		return oweWorkloadRunEnd(ctx, q, WorkloadRunEnd{RunID: *w.CurrentRunID, Owner: w.Owner, Name: w.Name, Generation: w.Generation, Reason: WorkloadEndDeleted})
	})
	if err != nil {
		return Workload{}, err
	}
	return w, nil
}

// The reasons a workload's run is owed its end, which its run.kill row carries.
const (
	WorkloadEndReplaced = "workload_replaced" // a newer launch of the workload replaced it
	WorkloadEndDeleted  = "workload_deleted"  // the workload was deleted
)

// WorkloadRunEnd is a run a workload let go of that is still owed its end (migration 0244): written in the
// transaction that let it go, and dropped once the run is ended, so an end cut short is still owed. Owner,
// Name and Generation are the workload's, for the run.kill row; Reason is WorkloadEndReplaced or
// WorkloadEndDeleted. AfterRunID, when set, is the queued run that replaces it: the end waits until that
// run is running. AfterState and AfterDetail are that run's state and status_detail, and AfterRan whether
// it took its slot (workloadRunRanSQL: a run that ran and then ended still replaced this one), as
// ListWorkloadRunEnds read them.
type WorkloadRunEnd struct {
	RunID       uuid.UUID
	Owner       string
	Name        string
	Generation  int64
	Reason      string
	AfterRunID  *uuid.UUID
	AfterState  types.RunState
	AfterDetail string
	AfterRan    bool
}

func scanWorkloadRunEnd(row pgx.Row) (WorkloadRunEnd, error) {
	var e WorkloadRunEnd
	err := row.Scan(&e.RunID, &e.Owner, &e.Name, &e.Generation, &e.Reason, &e.AfterRunID, &e.AfterState, &e.AfterDetail, &e.AfterRan)
	return e, err
}

// oweWorkloadRunEnd records e on q. A run already owed its end keeps the row it has.
func oweWorkloadRunEnd(ctx context.Context, q Querier, e WorkloadRunEnd) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO workload_run_ends (run_id, owner, name, generation, reason, after_run_id) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (run_id) DO NOTHING`, e.RunID, e.Owner, e.Name, e.Generation, e.Reason, e.AfterRunID); err != nil {
		return fmt.Errorf("store: owe a workload's run its end: %w", err)
	}
	return nil
}

// OweWorkloadRunEnd records that e's run is owed its end: a run a launch started that the workload did not
// take, because another launch was recorded first or the workload is gone.
func (s PG) OweWorkloadRunEnd(ctx context.Context, e WorkloadRunEnd) error {
	return oweWorkloadRunEnd(ctx, s.Pool, e)
}

// ListWorkloadRunEnds returns up to limit owed ends recorded more than olderThan ago, oldest first, each
// with the state of the run it waits on.
func (s PG) ListWorkloadRunEnds(ctx context.Context, olderThan time.Duration, limit int) ([]WorkloadRunEnd, error) {
	return collect(ctx, s.Pool, "list", "owed workload run ends", `
		SELECT e.run_id, e.owner, e.name, e.generation, e.reason, e.after_run_id, COALESCE(a.state, ''), COALESCE(a.status_detail, ''),
			COALESCE(`+workloadRunRanSQL("a", "$3")+`, false)
		FROM workload_run_ends e LEFT JOIN agent_runs a ON a.id = e.after_run_id
		WHERE e.created_at < now() - make_interval(secs => $1::float8) ORDER BY e.created_at, e.run_id LIMIT $2`,
		[]any{olderThan.Seconds(), limit, slotHoldingStateNames()}, scanWorkloadRunEnd)
}

// RestoreWorkloadRun gives a workload back the run e is owed to end, because e.AfterRunID, the queued run
// that was to replace it, ended before it ran: e's run is the current run again, the generation is owed a
// launch again (current_generation 0, as a restart leaves it) and refusal is recorded. It reports whether
// it restored. Nothing is written when e no longer waits on that run. When no workload holds that run any
// more, e's run is owed its end at once instead.
func (s PG) RestoreWorkloadRun(ctx context.Context, e WorkloadRunEnd, refusal WorkloadRefusal) (bool, error) {
	if e.AfterRunID == nil {
		return false, nil
	}
	b, err := json.Marshal(refusal)
	if err != nil {
		return false, fmt.Errorf("store: encode workload refusal: %w", err)
	}
	restored := false
	err = s.inTx(ctx, func(q Querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM workload_run_ends WHERE run_id = $1 AND after_run_id = $2`, e.RunID, *e.AfterRunID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if tag, err = q.Exec(ctx, `UPDATE workloads SET current_run_id = $2, current_generation = 0, last_refusal = $3 WHERE current_run_id = $1`,
			*e.AfterRunID, e.RunID, b); err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			e.AfterRunID = nil
			return oweWorkloadRunEnd(ctx, q, e)
		}
		restored = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: restore a workload's run: %w", err)
	}
	return restored, nil
}

// DropWorkloadRunEnd removes runID's owed end once the run is ended. One already gone is not an error.
func (s PG) DropWorkloadRunEnd(ctx context.Context, runID uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM workload_run_ends WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("store: drop a workload run's owed end: %w", err)
	}
	return nil
}

const workloadBindingCols = `b.id, b.pool_id, b.namespace, b.service, b.sponsor, b.created_by, b.created_at`

func scanWorkloadBinding(row pgx.Row) (WorkloadBinding, error) {
	var b WorkloadBinding
	err := row.Scan(&b.ID, &b.PoolID, &b.Namespace, &b.Service, &b.Sponsor, &b.CreatedBy, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkloadBinding{}, ErrNotFound
	}
	if err != nil {
		return WorkloadBinding{}, fmt.Errorf("store: scan workload binding: %w", err)
	}
	return b, nil
}

// CreateWorkloadBinding binds b.Namespace on the live organization pool b.PoolID to the service identity
// b.Service, with the sponsor the caller read. ErrNotFound when the pool is unknown, deleted or a person's
// own (a personal runner must never launch as a service), and
// ErrWorkloadBindingConflict when the namespace is bound on the pool already. Whether the identity may be
// bound is the caller's to decide.
func (s PG) CreateWorkloadBinding(ctx context.Context, b WorkloadBinding) (WorkloadBinding, error) {
	out, err := scanWorkloadBinding(s.Pool.QueryRow(ctx, `
		INSERT INTO workload_bindings AS b (pool_id, namespace, service, sponsor, created_by)
		SELECT p.id, $2, $3, $4, $5 FROM runner_pools p WHERE p.id = $1 AND p.state <> 'deleted' AND p.scope = 'organization'
		RETURNING `+workloadBindingCols, b.PoolID, b.Namespace, b.Service, b.Sponsor, b.CreatedBy))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return WorkloadBinding{}, ErrWorkloadBindingConflict
	}
	return out, err
}

// ListWorkloadBindings returns every binding, by pool then namespace.
func (s PG) ListWorkloadBindings(ctx context.Context, p Page) ([]WorkloadBinding, error) {
	q, args := p.appendTo(`SELECT `+workloadBindingCols+` FROM workload_bindings b ORDER BY b.pool_id, b.namespace`, nil)
	return collect(ctx, s.Pool, "list", "workload bindings", q, args, scanWorkloadBinding)
}

// DeleteWorkloadBinding removes a binding and returns the removed row, or ErrNotFound. Runs it launched
// are untouched.
func (s PG) DeleteWorkloadBinding(ctx context.Context, id uuid.UUID) (WorkloadBinding, error) {
	return scanWorkloadBinding(s.Pool.QueryRow(ctx, `DELETE FROM workload_bindings b WHERE b.id = $1 RETURNING `+workloadBindingCols, id))
}

// BindingForRunner returns the binding of namespace on a live pool the runner is a member of, or
// ErrNotFound: the namespace is unbound, or bound only on pools the runner is not in. Whether the
// binding still carries authority is the caller's to check. A runner in two pools that bind the namespace
// gets the older binding.
func (s PG) BindingForRunner(ctx context.Context, runner uuid.UUID, namespace string) (WorkloadBinding, error) {
	return scanWorkloadBinding(s.Pool.QueryRow(ctx, `
		SELECT `+workloadBindingCols+`
		FROM workload_bindings b
		JOIN runner_pool_members m ON m.pool_id = b.pool_id AND m.runner_id = $1
		JOIN runner_pools p ON p.id = b.pool_id AND p.state <> 'deleted'
		WHERE b.namespace = $2
		ORDER BY b.created_at, b.id LIMIT 1`, runner, namespace))
}
