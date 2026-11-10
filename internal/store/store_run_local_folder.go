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
	// GiveUpRunLocalFolder ends run id's pass back as given up, at any generation: back is recorded,
	// any resume dropped, and the generation moved on, so no owner's later write lands. It answers the
	// new generation, and false when the record was already done (folderStatusDoneSQL) or the run
	// has no local folder: the give-up is then not recorded again.
	GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, when FolderGiveUpWhen, back types.FolderPassRecord) (int, bool, error)
}

// FolderGiveUpWhen is when a pass back may be given up: only for a terminal run, and only once
// GiveUpAt has passed or the caller observed that the sandbox cannot be kept with its egress cut
// (D1). A live run, or one before its time, is never given up.
type FolderGiveUpWhen struct {
	GiveUpAt time.Time
	D1       bool
}

// folderStatusDoneSQL is a record that is done: a pass back recorded and no resume kept. NULL (no
// record) is not done.
const folderStatusDoneSQL = `COALESCE(local_folder_status ? 'back' AND NOT local_folder_status ? 'resume', false)`

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

// GiveUpRunLocalFolder implements RunLocalFolderStatuses: one statement, with no generation guard.
// TODO(finality WHERE text, run-end design rev 2.2): the terminal and time-or-D1 predicate below is
// the caller-passed form; replace it with the frozen text when it lands.
func (s PG) GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, when FolderGiveUpWhen, back types.FolderPassRecord) (int, bool, error) {
	raw, err := json.Marshal(back)
	if err != nil {
		return 0, false, fmt.Errorf("store: marshal a given-up pass back: %w", err)
	}
	var gen int
	err = s.Pool.QueryRow(ctx, `UPDATE agent_runs
		SET local_folder_status = (COALESCE(local_folder_status, '{}'::jsonb) - 'resume')
			|| jsonb_build_object('gen', COALESCE((local_folder_status->>'gen')::int, 0) + 1, 'back', $2::jsonb)
		WHERE id = $1 AND local_folder IS NOT NULL AND NOT `+folderStatusDoneSQL+`
		  AND NOT (state = ANY($3)) AND ($4::timestamptz <= now() OR $5::bool)
		RETURNING (local_folder_status->>'gen')::int`, id, raw, nonTerminalStates(), pgTime(when.GiveUpAt), when.D1).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: give up a pass back: %w", err)
	}
	return gen, true, nil
}
