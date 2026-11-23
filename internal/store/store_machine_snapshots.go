// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Snapshots of Linux VM runs' disks, the forks that use them (holds) and where each fork came from (lineage).
// owner_subject is written once, on INSERT, and no statement here updates it. A snapshot row a hold names cannot
// be deleted (the foreign key is ON DELETE RESTRICT), so a layer a fork uses outlives every delete and expiry.

// ErrMachineSnapshotBusy is returned by CreateMachineSnapshot when another snapshot of the same run is being made.
var ErrMachineSnapshotBusy = fmt.Errorf("%w: a snapshot of this run is being made", ErrConflict)

// ErrMachineSnapshotInUse is returned when a fork holds the snapshot, so it cannot be deleted.
var ErrMachineSnapshotInUse = fmt.Errorf("%w: a fork uses this snapshot", ErrConflict)

// ErrMachineSnapshotUnavailable is returned by ClaimMachineSnapshotFork when the snapshot is gone or not ready.
var ErrMachineSnapshotUnavailable = errors.New("store: the snapshot is not ready")

// ErrForkSupportSession is returned by ClaimMachineSnapshotFork when a support helper was let into the snapshot's
// source run, or that run's row is gone, so the fact cannot be read.
var ErrForkSupportSession = errors.New("store: a support session was accepted on the snapshot's source run")

// MachineSnapshotStore is the OPTIONAL store of machine snapshots.
type MachineSnapshotStore interface {
	// CreateMachineSnapshot inserts m as creating. ErrMachineSnapshotBusy when another snapshot of the same run is
	// creating.
	CreateMachineSnapshot(ctx context.Context, m types.MachineSnapshot) (types.MachineSnapshot, error)
	// GetMachineSnapshot reads one snapshot; ErrNotFound when there is none.
	GetMachineSnapshot(ctx context.Context, id uuid.UUID) (types.MachineSnapshot, error)
	// ListMachineSnapshotsByOwner lists owner's snapshots that are not being deleted, newest first.
	ListMachineSnapshotsByOwner(ctx context.Context, owner string) ([]types.MachineSnapshot, error)
	// MarkMachineSnapshotReady records the committed layer and moves a creating snapshot to ready. False when the
	// row is no longer creating (another writer failed it first).
	MarkMachineSnapshotReady(ctx context.Context, id uuid.UUID, layerSHA256 string, bytes int64, baseStamp string) (bool, error)
	// FailMachineSnapshot moves a creating snapshot to failed with failure. False when it is no longer creating.
	FailMachineSnapshot(ctx context.Context, id uuid.UUID, failure string) (bool, error)
	// SetMachineSnapshotLayerGone records that the runner holds no layer for a failed snapshot.
	SetMachineSnapshotLayerGone(ctx context.Context, id uuid.UUID) error
	// BeginMachineSnapshotDelete moves a snapshot of owner ("" for any owner) to deleting, unless a fork holds
	// it: then it answers the number of holds and ErrMachineSnapshotInUse.
	BeginMachineSnapshotDelete(ctx context.Context, id uuid.UUID, owner string) (int, error)
	// DeleteMachineSnapshotRow deletes a deleting or failed snapshot's row once its layer is gone.
	// ErrMachineSnapshotInUse when a hold still names it.
	DeleteMachineSnapshotRow(ctx context.Context, id uuid.UUID) error
	// ClaimMachineSnapshotFork, in one transaction, re-reads the ready snapshot FOR SHARE, refuses when its
	// source run had a support session or is gone, and writes the fork's hold and lineage rows.
	ClaimMachineSnapshotFork(ctx context.Context, snapshotID, forkRunID uuid.UUID) error
	// ReleaseMachineSnapshotHold deletes the fork's hold on the snapshot.
	ReleaseMachineSnapshotHold(ctx context.Context, snapshotID, forkRunID uuid.UUID) error
	// SnapshotImageIDs lists every image id a snapshot on runnerID backs onto, so an image cache pruner can keep
	// those images.
	SnapshotImageIDs(ctx context.Context, runnerID uuid.UUID) ([]string, error)
}

var _ MachineSnapshotStore = PG{}

const machineSnapshotCols = `id, source_run_id, owner_subject, runner_id, machine_kind, image_ref, image_digest, image_id,
	vm_machine, layer_name, COALESCE(layer_sha256, ''), COALESCE(bytes, 0), COALESCE(base_stamp, ''), unit_id,
	workspace_ids, state, COALESCE(failure, ''), COALESCE(label, ''), created_at, last_used_at, expires_at, cut_start,
	cut_end, layer_gone_at`

func scanMachineSnapshot(row pgx.Row) (types.MachineSnapshot, error) {
	var m types.MachineSnapshot
	var kind, state string
	var vm []byte
	err := row.Scan(&m.ID, &m.SourceRunID, &m.OwnerSubject, &m.RunnerID, &kind, &m.ImageRef, &m.ImageDigest, &m.ImageID,
		&vm, &m.LayerName, &m.LayerSHA256, &m.Bytes, &m.BaseStamp, &m.UnitID,
		&m.WorkspaceIDs, &state, &m.Failure, &m.Label, &m.CreatedAt, &m.LastUsedAt, &m.ExpiresAt, &m.CutStart,
		&m.CutEnd, &m.LayerGoneAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.MachineSnapshot{}, ErrNotFound
	}
	if err != nil {
		return types.MachineSnapshot{}, fmt.Errorf("store: scan machine snapshot: %w", err)
	}
	if err := json.Unmarshal(vm, &m.VMMachine); err != nil {
		return types.MachineSnapshot{}, fmt.Errorf("store: decode machine snapshot vm_machine: %w", err)
	}
	m.MachineKind, m.State = types.MachineKind(kind), types.MachineSnapshotState(state)
	return m, nil
}

// CreateMachineSnapshot — see MachineSnapshotStore.
func (s PG) CreateMachineSnapshot(ctx context.Context, m types.MachineSnapshot) (types.MachineSnapshot, error) {
	vm, err := json.Marshal(m.VMMachine)
	if err != nil {
		return types.MachineSnapshot{}, fmt.Errorf("store: encode machine snapshot vm_machine: %w", err)
	}
	ws := m.WorkspaceIDs
	if ws == nil {
		ws = []uuid.UUID{}
	}
	out, err := scanMachineSnapshot(s.Pool.QueryRow(ctx, `
		INSERT INTO machine_snapshots (id, source_run_id, owner_subject, runner_id, machine_kind, image_ref, image_digest,
			image_id, vm_machine, layer_name, unit_id, workspace_ids, state, label, expires_at, cut_start, cut_end)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'creating', NULLIF($13, ''), $14, $15, $16)
		RETURNING `+machineSnapshotCols,
		m.ID, m.SourceRunID, m.OwnerSubject, m.RunnerID, string(m.MachineKind), m.ImageRef, m.ImageDigest,
		m.ImageID, vm, m.LayerName, m.UnitID, ws, m.Label, m.ExpiresAt, m.CutStart, m.CutEnd))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "machine_snapshots_one_creating" {
		return types.MachineSnapshot{}, ErrMachineSnapshotBusy
	}
	return out, err
}

// GetMachineSnapshot — see MachineSnapshotStore.
func (s PG) GetMachineSnapshot(ctx context.Context, id uuid.UUID) (types.MachineSnapshot, error) {
	return scanMachineSnapshot(s.Pool.QueryRow(ctx, `SELECT `+machineSnapshotCols+` FROM machine_snapshots WHERE id = $1`, id))
}

// ListMachineSnapshotsByOwner — see MachineSnapshotStore.
func (s PG) ListMachineSnapshotsByOwner(ctx context.Context, owner string) ([]types.MachineSnapshot, error) {
	const q = `SELECT ` + machineSnapshotCols + ` FROM machine_snapshots
		WHERE owner_subject = $1 AND state <> 'deleting' ORDER BY created_at DESC, id`
	return collect(ctx, s.Pool, "list", "machine snapshots", q, []any{owner}, scanMachineSnapshot)
}

// MarkMachineSnapshotReady — see MachineSnapshotStore.
func (s PG) MarkMachineSnapshotReady(ctx context.Context, id uuid.UUID, layerSHA256 string, bytes int64, baseStamp string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE machine_snapshots SET state = 'ready', layer_sha256 = $2, bytes = $3, base_stamp = $4
		WHERE id = $1 AND state = 'creating'`, id, layerSHA256, bytes, baseStamp)
	if err != nil {
		return false, fmt.Errorf("store: mark machine snapshot ready: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FailMachineSnapshot — see MachineSnapshotStore.
func (s PG) FailMachineSnapshot(ctx context.Context, id uuid.UUID, failure string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE machine_snapshots SET state = 'failed', failure = $2
		WHERE id = $1 AND state = 'creating'`, id, failure)
	if err != nil {
		return false, fmt.Errorf("store: fail machine snapshot: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetMachineSnapshotLayerGone — see MachineSnapshotStore.
func (s PG) SetMachineSnapshotLayerGone(ctx context.Context, id uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE machine_snapshots SET layer_gone_at = now()
		WHERE id = $1 AND state = 'failed' AND layer_gone_at IS NULL`, id); err != nil {
		return fmt.Errorf("store: set machine snapshot layer gone: %w", err)
	}
	return nil
}

// BeginMachineSnapshotDelete — see MachineSnapshotStore. The row is taken FOR UPDATE, and a fork's claim takes it
// FOR SHARE, so a claim and a delete cannot interleave.
func (s PG) BeginMachineSnapshotDelete(ctx context.Context, id uuid.UUID, owner string) (int, error) {
	holds := 0
	err := s.inTx(ctx, func(q Querier) error {
		var found uuid.UUID
		err := q.QueryRow(ctx, `SELECT id FROM machine_snapshots WHERE id = $1 AND ($2 = '' OR owner_subject = $2)
			FOR UPDATE`, id, owner).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock machine snapshot: %w", err)
		}
		if err := q.QueryRow(ctx, `SELECT count(*) FROM machine_snapshot_holds WHERE snapshot_id = $1`, id).Scan(&holds); err != nil {
			return fmt.Errorf("store: count machine snapshot holds: %w", err)
		}
		if holds > 0 {
			return ErrMachineSnapshotInUse
		}
		if _, err := q.Exec(ctx, `UPDATE machine_snapshots SET state = 'deleting' WHERE id = $1 AND state <> 'failed'`, id); err != nil {
			return fmt.Errorf("store: begin machine snapshot delete: %w", err)
		}
		return nil
	})
	return holds, err
}

// DeleteMachineSnapshotRow — see MachineSnapshotStore.
func (s PG) DeleteMachineSnapshotRow(ctx context.Context, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM machine_snapshots WHERE id = $1 AND state IN ('deleting', 'failed')`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrMachineSnapshotInUse
	}
	if err != nil {
		return fmt.Errorf("store: delete machine snapshot: %w", err)
	}
	return nil
}

// ClaimMachineSnapshotFork — see MachineSnapshotStore.
func (s PG) ClaimMachineSnapshotFork(ctx context.Context, snapshotID, forkRunID uuid.UUID) error {
	return s.inTx(ctx, func(q Querier) error {
		var source *uuid.UUID
		err := q.QueryRow(ctx, `SELECT source_run_id FROM machine_snapshots WHERE id = $1 AND state = 'ready' FOR SHARE`,
			snapshotID).Scan(&source)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMachineSnapshotUnavailable
		}
		if err != nil {
			return fmt.Errorf("store: lock machine snapshot for a fork: %w", err)
		}
		if source == nil {
			return ErrForkSupportSession
		}
		var accepted bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_support_grants WHERE run_id = $1 AND accepted_at IS NOT NULL)`,
			*source).Scan(&accepted); err != nil {
			return fmt.Errorf("store: read the source run's support grants: %w", err)
		}
		if accepted {
			return ErrForkSupportSession
		}
		if _, err := q.Exec(ctx, `INSERT INTO machine_snapshot_holds (snapshot_id, run_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, snapshotID, forkRunID); err != nil {
			return fmt.Errorf("store: insert machine snapshot hold: %w", err)
		}
		if _, err := q.Exec(ctx, `INSERT INTO run_fork_sources (run_id, snapshot_id, source_run_id) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, forkRunID, snapshotID, *source); err != nil {
			return fmt.Errorf("store: insert run fork source: %w", err)
		}
		return nil
	})
}

// ReleaseMachineSnapshotHold — see MachineSnapshotStore.
func (s PG) ReleaseMachineSnapshotHold(ctx context.Context, snapshotID, forkRunID uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM machine_snapshot_holds WHERE snapshot_id = $1 AND run_id = $2`,
		snapshotID, forkRunID); err != nil {
		return fmt.Errorf("store: release machine snapshot hold: %w", err)
	}
	return nil
}

// SnapshotImageIDs — see MachineSnapshotStore.
func (s PG) SnapshotImageIDs(ctx context.Context, runnerID uuid.UUID) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT image_id FROM machine_snapshots WHERE runner_id = $1 ORDER BY image_id`, runnerID)
	if err != nil {
		return nil, fmt.Errorf("store: list snapshot image ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: list snapshot image ids: %w", err)
	}
	return ids, nil
}
