// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

// The organisation's side of a Kubernetes runner's node-loss containment (design K1 §3.1 Q2 and (b)):
// the runner's containment-pending report, kept until the runner reports the proxy stopped, and the node
// records a security operator sends to release a run the runner cannot prove stopped.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrRunNodeRecordStale refuses a node record whose run is no longer held as containment pending, or
// whose runner record changed since the record was approved: the approvals lapse.
var ErrRunNodeRecordStale = errors.New("store: the run's containment record changed since the node record was approved")

// Containment-pending transitions a state page can make, each audited after the page commits.
const (
	ContainmentPendingEntered    = "entered"
	ContainmentPendingNameReused = "name_reused"
	ContainmentPendingCleared    = "cleared"
)

// RunContainmentEvent is one transition of a run's containment-pending report.
type RunContainmentEvent struct {
	RunID, RunnerID     uuid.UUID
	State, Kind         string
	NameReused, NoCause bool
}

// RunContainmentPendingRow is the stored report, with the run's last token renewal the bound derives from.
type RunContainmentPendingRow struct {
	RunnerID            uuid.UUID
	Kind                string
	Since               time.Time
	NameReused          bool
	NoCausalAssociation bool
	Tuple               json.RawMessage
	TokenRenewedAt      time.Time
}

// syncRunContainmentPendingTx keeps the organisation's copy of row's containment-pending report. A report
// is stored or refreshed: its name reuse and missing causal association only ever latch on, and its since
// keeps the earliest. The copy is removed only when the runner reports the proxy stopped, which it does
// only on completion evidence; a page that merely omits the report removes nothing. A report beside a
// proxy-stopped stamp, or of an unknown kind, is refused.
func syncRunContainmentPendingTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, row runnerwire.RunState) (*RunContainmentEvent, error) {
	p := row.ContainmentPending
	if p == nil {
		if row.ProxyStoppedAt == nil {
			return nil, nil
		}
		var ev RunContainmentEvent
		err := tx.QueryRow(ctx, `DELETE FROM run_containment_pending WHERE run_id=$1 AND runner_id=$2 RETURNING kind, name_reused, no_causal_association`,
			row.RunID, runnerID).Scan(&ev.Kind, &ev.NameReused, &ev.NoCause)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		ev.RunID, ev.RunnerID, ev.State = row.RunID, runnerID, ContainmentPendingCleared
		return &ev, nil
	}
	if (p.Kind != runner.ContainmentAgentOnly && p.Kind != runner.ContainmentPendingDelete) || p.Since.IsZero() || row.ProxyStoppedAt != nil {
		return nil, ErrRunnerExecutionRefused
	}
	var tuple []byte
	if p.Tuple != nil {
		var err error
		if tuple, err = json.Marshal(p.Tuple); err != nil {
			return nil, err
		}
	}
	var held bool
	var wasReused bool
	err := tx.QueryRow(ctx, `SELECT true, name_reused FROM run_containment_pending WHERE run_id=$1 FOR UPDATE`, row.RunID).Scan(&held, &wasReused)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	ev := RunContainmentEvent{RunID: row.RunID, RunnerID: runnerID, Kind: p.Kind}
	err = tx.QueryRow(ctx, `INSERT INTO run_containment_pending (run_id, runner_id, kind, since, name_reused, no_causal_association, tuple)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (run_id) DO UPDATE SET kind = EXCLUDED.kind, since = LEAST(run_containment_pending.since, EXCLUDED.since),
			name_reused = run_containment_pending.name_reused OR EXCLUDED.name_reused,
			no_causal_association = run_containment_pending.no_causal_association OR EXCLUDED.no_causal_association,
			tuple = EXCLUDED.tuple, observed_at = clock_timestamp()
		WHERE run_containment_pending.runner_id = EXCLUDED.runner_id
		RETURNING name_reused, no_causal_association`,
		row.RunID, runnerID, p.Kind, p.Since.UTC(), p.NameReused, p.NoCausalAssociation, tuple).Scan(&ev.NameReused, &ev.NoCause)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunnerExecutionRefused // another runner's report for this run
	}
	if err != nil {
		return nil, err
	}
	switch {
	case !held:
		ev.State = ContainmentPendingEntered
	case ev.NameReused && !wasReused:
		ev.State = ContainmentPendingNameReused
	default:
		return nil, nil
	}
	return &ev, nil
}

// GetRunContainmentPending is the run's stored containment-pending report, or ErrNotFound.
func (s PG) GetRunContainmentPending(ctx context.Context, runID uuid.UUID) (RunContainmentPendingRow, error) {
	var out RunContainmentPendingRow
	err := s.Pool.QueryRow(ctx, `SELECT c.runner_id, c.kind, c.since, c.name_reused, c.no_causal_association, c.tuple, a.token_renewed_at
		FROM run_containment_pending c JOIN agent_runs a ON a.id = c.run_id WHERE c.run_id = $1`, runID).
		Scan(&out.RunnerID, &out.Kind, &out.Since, &out.NameReused, &out.NoCausalAssociation, &out.Tuple, &out.TokenRenewedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

// ListRunNodeRecords is the node records made for a run, newest first.
func (s PG) ListRunNodeRecords(ctx context.Context, runID uuid.UUID) ([]types.RunNodeRecord, error) {
	return collect(ctx, s.Pool, "list", "run node records", `SELECT id, attestation, source, source_id, state, reason, created_at, decided_at
		FROM run_node_records WHERE run_id = $1 ORDER BY created_at DESC, id`, []any{runID}, func(row pgx.Row) (types.RunNodeRecord, error) {
		var r types.RunNodeRecord
		err := row.Scan(&r.ID, &r.Attestation, &r.Source, &r.SourceID, &r.State, &r.Reason, &r.CreatedAt, &r.DecidedAt)
		return r, err
	})
}

// RunNodeRecordWrite is one approved node record to send to the run's runner. NameReused,
// NoCausalAssociation and Tuple are the containment record the approvals were given against.
type RunNodeRecordWrite struct {
	RunID               uuid.UUID
	Record              runner.NodeRecord
	NameReused          bool
	NoCausalAssociation bool
}

// QueueRunNodeRecord stores rec and queues the stop_proxy that carries it to the run's runner, in one
// transaction. See QueueRunNodeRecordQ.
func (s PG) QueueRunNodeRecord(ctx context.Context, rec RunNodeRecordWrite) (types.RunnerPendingAction, error) {
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return types.RunnerPendingAction{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	a, err := queueRunNodeRecordTx(ctx, tx, rec)
	if err != nil {
		return types.RunnerPendingAction{}, err
	}
	return a, tx.Commit(ctx)
}

// QueueRunNodeRecordQ is QueueRunNodeRecord inside a governance decision's transaction.
func QueueRunNodeRecordQ(ctx context.Context, q Querier, rec RunNodeRecordWrite) (types.RunnerPendingAction, error) {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return types.RunnerPendingAction{}, errors.New("store: a node record is queued inside a transaction")
	}
	return queueRunNodeRecordTx(ctx, tx, rec)
}

// queueRunNodeRecordTx binds the record to the run as the organisation holds it now: the run must still
// be held as containment pending by its own runner, with the tuple, name reuse and association the
// approvals saw (ErrRunNodeRecordStale otherwise), and no other stop_proxy may be waiting on the runner for
// it (ErrConflict). The stop_proxy is bound like any other action (owner, ref, execution generation) and
// carries the record as its proof instead of an outage loss.
func queueRunNodeRecordTx(ctx context.Context, tx pgx.Tx, rec RunNodeRecordWrite) (types.RunnerPendingAction, error) {
	var runnerID uuid.UUID
	var held RunContainmentPendingRow
	err := tx.QueryRow(ctx, `SELECT c.runner_id, c.name_reused, c.no_causal_association, c.tuple FROM run_containment_pending c
		JOIN agent_runs a ON a.id = c.run_id AND a.runner_id = c.runner_id WHERE c.run_id = $1`, rec.RunID).
		Scan(&runnerID, &held.NameReused, &held.NoCausalAssociation, &held.Tuple)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPendingAction{}, ErrRunNodeRecordStale
	}
	if err != nil {
		return types.RunnerPendingAction{}, err
	}
	owner, run, generation, err := lockRunnerActionRun(ctx, tx, runnerID, rec.RunID)
	if err != nil {
		return types.RunnerPendingAction{}, err
	}
	// Re-read under the run's lock: a state page that landed since the approval changes what it bound.
	err = tx.QueryRow(ctx, `SELECT name_reused, no_causal_association, tuple FROM run_containment_pending WHERE run_id = $1 AND runner_id = $2 FOR UPDATE`,
		rec.RunID, runnerID).Scan(&held.NameReused, &held.NoCausalAssociation, &held.Tuple)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPendingAction{}, ErrRunNodeRecordStale
	}
	if err != nil {
		return types.RunnerPendingAction{}, err
	}
	tuple, err := json.Marshal(rec.Record.Tuple)
	if err != nil {
		return types.RunnerPendingAction{}, err
	}
	if held.NameReused != rec.NameReused || held.NoCausalAssociation != rec.NoCausalAssociation || !SameJSON(held.Tuple, tuple) || run.SandboxRef == "" {
		return types.RunnerPendingAction{}, ErrRunNodeRecordStale
	}
	// One open stop_proxy per run (runner_pending_actions_one_open_unsettled): another record, or an
	// outage stop queued before the runner reported the run pending, is answered first.
	var waiting bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pending_actions WHERE run_id = $1 AND kind = 'stop_proxy'
		AND applied_at IS NULL AND settled_at IS NULL)`, rec.RunID).Scan(&waiting); err != nil {
		return types.RunnerPendingAction{}, err
	}
	if waiting {
		return types.RunnerPendingAction{}, ErrConflict
	}
	proof, err := json.Marshal(rec.Record)
	if err != nil {
		return types.RunnerPendingAction{}, err
	}
	var generationArg *uuid.UUID
	if generation != uuid.Nil {
		generationArg = &generation
	}
	a, err := scanAction(tx.QueryRow(ctx, `
		INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner, execution_generation, proof)
		VALUES ($1,$2,$3,'stop_proxy',$4,1,$5,$6,$7)
		RETURNING `+actionCols, uuid.New(), runnerID, rec.RunID, run.SandboxRef, owner, generationArg, proof))
	if err != nil {
		return types.RunnerPendingAction{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_node_records (id, run_id, runner_id, action_id, attestation, source, source_id) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		rec.Record.ID, rec.RunID, runnerID, a.ID, rec.Record.Attestation, rec.Record.Source, rec.Record.SourceID); err != nil {
		return types.RunnerPendingAction{}, fmt.Errorf("store: record node record: %w", err)
	}
	return a, nil
}

// settleRunNodeRecordTx records the runner's answer to a record-carrying stop_proxy: refused with its
// reason, or accepted (the runner took the record as its infrastructure fence and stopped the proxy).
func settleRunNodeRecordTx(ctx context.Context, tx pgx.Tx, a types.RunnerPendingAction, outcome, reason string) error {
	if a.NodeRecord == nil {
		return nil
	}
	state := types.NodeRecordAccepted
	if outcome == types.RunnerActionRefused {
		state = types.NodeRecordRefused
	} else {
		reason = ""
	}
	_, err := tx.Exec(ctx, `UPDATE run_node_records SET state = $2, reason = $3, decided_at = clock_timestamp() WHERE action_id = $1 AND state = 'sent'`,
		a.ID, state, reason)
	return err
}

// SameJSON reports whether two encodings hold the same JSON value, whatever their key order or spacing.
// Both empty (or both JSON null) compare equal.
func SameJSON(a, b []byte) bool {
	norm := func(x []byte) ([]byte, bool) {
		x = bytes.TrimSpace(x)
		if len(x) == 0 || bytes.Equal(x, []byte("null")) {
			return nil, true
		}
		var v any
		if json.Unmarshal(x, &v) != nil {
			return nil, false
		}
		out, err := json.Marshal(v)
		return out, err == nil
	}
	x, ok1 := norm(a)
	y, ok2 := norm(b)
	return ok1 && ok2 && bytes.Equal(x, y)
}
