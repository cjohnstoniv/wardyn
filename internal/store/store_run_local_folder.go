// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLocalFolders is the local-folder seed surface (0195). Optional for RunReviver's reason: the
// test doubles that embed store.Store would route these calls to a nil interface. The api layer
// type-asserts, and refuses a local folder on a store without it; production is always PG.
type RunLocalFolders interface {
	// RunLocalFolderSeed reads run id's local folder and the outcome of its first sync. The folder
	// is nil on a run that has none.
	RunLocalFolderSeed(ctx context.Context, id uuid.UUID) (*types.RunLocalFolder, LocalFolderSeed, error)
	// SetRunLocalFolderSeed records the first sync's outcome, once: seeded when errText is empty,
	// failed with errText otherwise. False when the run has no local folder or an outcome is
	// already recorded.
	SetRunLocalFolderSeed(ctx context.Context, id uuid.UUID, errText string) (bool, error)
}

// LocalFolderSeed is the outcome of a run's first sync: neither field set while it is awaited.
type LocalFolderSeed struct {
	SeededAt *time.Time
	Error    string
}

// RunLocalFolderSeed implements RunLocalFolders.
func (s PG) RunLocalFolderSeed(ctx context.Context, id uuid.UUID) (*types.RunLocalFolder, LocalFolderSeed, error) {
	var raw []byte
	var seed LocalFolderSeed
	var errText *string
	err := s.Pool.QueryRow(ctx, `SELECT local_folder, local_folder_seeded_at, local_folder_seed_error FROM agent_runs WHERE id = $1`, id).
		Scan(&raw, &seed.SeededAt, &errText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, LocalFolderSeed{}, ErrNotFound
	}
	if err != nil {
		return nil, LocalFolderSeed{}, fmt.Errorf("store: read run local folder: %w", err)
	}
	if errText != nil {
		seed.Error = *errText
	}
	if raw == nil {
		return nil, seed, nil
	}
	var f types.RunLocalFolder
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, LocalFolderSeed{}, fmt.Errorf("store: unmarshal run local folder: %w", err)
	}
	return &f, seed, nil
}

// SetRunLocalFolderSeed implements RunLocalFolders.
func (s PG) SetRunLocalFolderSeed(ctx context.Context, id uuid.UUID, errText string) (bool, error) {
	const q = `
		UPDATE agent_runs SET
			local_folder_seeded_at = CASE WHEN $2 = '' THEN now() END,
			local_folder_seed_error = NULLIF($2, '')
		WHERE id = $1 AND local_folder IS NOT NULL
		  AND local_folder_seeded_at IS NULL AND local_folder_seed_error IS NULL`
	tag, err := s.Pool.Exec(ctx, q, id, errText)
	if err != nil {
		return false, fmt.Errorf("store: record run local folder seed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RunLocalFolderStatuses is the computer-folder pass record (0198), optional like RunLocalFolders.
// Every write of the record is guarded (FolderStatusGuard).
type RunLocalFolderStatuses interface {
	// RunLocalFolderStatus reads run id's record, zero while no pass has ended.
	RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error)
	// SetRunLocalFolderStatusIf replaces run id's record when the stored one meets guard, keeping
	// guard's generation. False when it does not, or the run has no local folder.
	SetRunLocalFolderStatusIf(ctx context.Context, id uuid.UUID, guard FolderStatusGuard, st types.RunLocalFolderStatus) (bool, error)
	// BumpRunLocalFolderGen moves run id's record from guard's generation to the next, changing
	// nothing else, and answers the record as it then stands. False when the stored record does not
	// meet guard, or the run has no local folder.
	BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, guard FolderStatusGuard) (types.RunLocalFolderStatus, bool, error)
	// GiveUpRunLocalFolder ends run id's pass back as given up, at any generation, once when allows it:
	// back is recorded with GivenUpAt set, any resume dropped, and the generation moved on, so no owner's
	// later write lands. ev, through prepare, is its one audit row, written with it and only when it
	// applies. It answers the new generation, and false when it did not apply: a live run, one with no
	// sandbox, one not yet due, or a record already done (a back and no resume).
	GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, when FolderGiveUpWhen, back types.FolderPassRecord, ev types.AuditEvent, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (int, bool, error)
}

// FolderGiveUpWhen is when a terminal run's pass back may be given up, on the database's clock: once
// Cap has passed since its end (ended_at, else updated_at), or, for a caller that observed its sandbox
// cannot be kept with its egress cut (D1), once Hold has passed since its containment was due
// (ContainmentDue, with Grace and MaxAge) and either the substrate refused the proxy stop
// (Unsupported) or its containment has failed for ContainErrAge.
type FolderGiveUpWhen struct {
	Cap           time.Duration
	D1            bool
	Hold          time.Duration
	Unsupported   bool
	ContainErrAge time.Duration
	Grace         time.Duration
	MaxAge        time.Duration
}

// ContainmentDue is when run's containment was due: its end, or earlier where it was kept past its
// time: a run kept lost after it ended at the loss plus grace, one kept lost with a lease end at that
// end plus grace (the server's keptUntil), and with a max age (0: none) at its age anchor (admitted_at,
// else created_at) plus maxAge. containmentDueSQL is the same on a row.
func ContainmentDue(run types.AgentRun, grace, maxAge time.Duration) time.Time {
	due := run.UpdatedAt
	if run.EndedAt != nil {
		due = *run.EndedAt
	}
	var kept *time.Time
	switch maxAge = maxAge.Truncate(time.Second); {
	case run.LostAt != nil && run.LostReason == types.LostEnded:
		t := run.LostAt.Add(grace)
		kept = &t
	case run.LostAt != nil && run.EndsAt != nil:
		t := run.EndsAt.Add(grace)
		kept = &t
	case maxAge > 0:
		anchor := run.CreatedAt
		if run.AdmittedAt != nil {
			anchor = *run.AdmittedAt
		}
		t := anchor.Add(maxAge)
		kept = &t
	}
	if kept != nil && kept.Before(due) {
		return *kept
	}
	return due
}

// containmentDueSQL is ContainmentDue on an agent_runs row, its grace in seconds at $g and its max age
// in whole seconds (maxAgeSecs) at $m.
func containmentDueSQL(g, m int) string {
	return fmt.Sprintf(`LEAST(COALESCE(ended_at, updated_at),
        CASE WHEN lost_at IS NOT NULL AND lost_reason = 'ended' THEN lost_at + make_interval(secs => $%[1]d)
             WHEN lost_at IS NOT NULL AND ends_at IS NOT NULL THEN ends_at + make_interval(secs => $%[1]d)
             WHEN $%[2]d > 0 THEN COALESCE(admitted_at, created_at) + make_interval(secs => $%[2]d)
        END)`, g, m)
}

// maxAgeSecs is a max age as containmentDueSQL's integer parameter.
func maxAgeSecs(d time.Duration) int64 { return int64(d / time.Second) }

// FolderStatusGuard is what a write of a run's folder record requires of the stored one: the
// generation its writer read or owns. A record with no generation, or no record, is at 0.
type FolderStatusGuard struct {
	Gen int
}

// sql is guard's predicate on agent_runs.local_folder_status, its arguments numbered from $n, and
// those arguments. It is the one place a status write's guard is spelled.
func (g FolderStatusGuard) sql(n int) (string, []any) {
	return fmt.Sprintf("COALESCE((local_folder_status->>'gen')::int, 0) = $%d", n), []any{g.Gen}
}

// RunLocalFolderStatus implements RunLocalFolderStatuses.
func (s PG) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT local_folder_status FROM agent_runs WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunLocalFolderStatus{}, ErrNotFound
	}
	if err != nil {
		return types.RunLocalFolderStatus{}, fmt.Errorf("store: read run local folder status: %w", err)
	}
	return unmarshalFolderStatus(raw)
}

func unmarshalFolderStatus(raw []byte) (types.RunLocalFolderStatus, error) {
	var st types.RunLocalFolderStatus
	if raw != nil {
		if err := json.Unmarshal(raw, &st); err != nil {
			return types.RunLocalFolderStatus{}, fmt.Errorf("store: unmarshal run local folder status: %w", err)
		}
	}
	return st, nil
}

// SetRunLocalFolderStatusIf implements RunLocalFolderStatuses.
func (s PG) SetRunLocalFolderStatusIf(ctx context.Context, id uuid.UUID, guard FolderStatusGuard, st types.RunLocalFolderStatus) (bool, error) {
	st.Gen = guard.Gen
	raw, err := json.Marshal(st)
	if err != nil {
		return false, fmt.Errorf("store: marshal run local folder status: %w", err)
	}
	pred, args := guard.sql(3)
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = $2 WHERE id = $1 AND local_folder IS NOT NULL AND `+pred,
		append([]any{id, raw}, args...)...)
	if err != nil {
		return false, fmt.Errorf("store: record run local folder status: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// BumpRunLocalFolderGen implements RunLocalFolderStatuses. Only gen changes, so a write that landed
// between the caller's read and this bump is kept.
func (s PG) BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, guard FolderStatusGuard) (types.RunLocalFolderStatus, bool, error) {
	pred, args := guard.sql(3)
	var raw []byte
	err := s.Pool.QueryRow(ctx, `UPDATE agent_runs
		SET local_folder_status = COALESCE(local_folder_status, '{}'::jsonb) || jsonb_build_object('gen', $2::int)
		WHERE id = $1 AND local_folder IS NOT NULL AND `+pred+`
		RETURNING local_folder_status`, append([]any{id, guard.Gen + 1}, args...)...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunLocalFolderStatus{}, false, nil
	}
	if err != nil {
		return types.RunLocalFolderStatus{}, false, fmt.Errorf("store: move run local folder status on: %w", err)
	}
	st, err := unmarshalFolderStatus(raw)
	return st, err == nil, err
}

// errGiveUpNoAudit is a give-up asked without its audit row.
var errGiveUpNoAudit = errors.New("store: a pass back's give-up needs its audit row")

// GiveUpRunLocalFolder implements RunLocalFolderStatuses: one statement, with no generation guard, and
// its audit row in the same transaction. now() is the transaction's time, so back's GivenUpAt and the
// row's time are the statement's.
func (s PG) GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, when FolderGiveUpWhen, back types.FolderPassRecord, ev types.AuditEvent, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (int, bool, error) {
	if prepare == nil {
		return 0, false, errGiveUpNoAudit
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return 0, false, err
	}
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&at); err != nil {
		return 0, false, err
	}
	back.GivenUpAt = &at
	raw, err := json.Marshal(back)
	if err != nil {
		return 0, false, fmt.Errorf("store: marshal a given-up pass back: %w", err)
	}
	var gen int
	err = tx.QueryRow(ctx, `UPDATE agent_runs
SET local_folder_status = (COALESCE(local_folder_status::jsonb,'{}'::jsonb) - 'resume')
    || jsonb_build_object('gen', COALESCE((local_folder_status->>'gen')::int,0)+1, 'back', $2::jsonb)
WHERE id = $1
  AND sandbox_ref <> ''
  AND NOT (state = ANY($3))
  AND NOT (COALESCE(local_folder_status::jsonb,'{}'::jsonb) ? 'back'
           AND NOT (COALESCE(local_folder_status::jsonb,'{}'::jsonb) ? 'resume'))
  AND ( COALESCE(ended_at, updated_at) + make_interval(secs => $4) <= now()
     OR ( $5::boolean
          AND `+containmentDueSQL(9, 10)+` + make_interval(secs => $6) <= now()
          AND ( $7::boolean
             OR (containment_error_at IS NOT NULL
                 AND containment_error_at <= now() - make_interval(secs => $8)) ) ) )
RETURNING (local_folder_status->>'gen')::int`, id, raw, nonTerminalStateNames(),
		when.Cap.Seconds(), when.D1, when.Hold.Seconds(), when.Unsupported, when.ContainErrAge.Seconds(),
		when.Grace.Seconds(), maxAgeSecs(when.MaxAge)).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, tx.Commit(ctx)
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: give up a pass back: %w", err)
	}
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	ev.RunID, ev.Time = &id, at
	if ev, err = prepare(ctx, ev); err != nil {
		return 0, false, err
	}
	if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	if s.SIEM != nil {
		_ = s.SIEM.Emit(context.WithoutCancel(ctx), ev)
	}
	return gen, true, nil
}
