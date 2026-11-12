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
	// BumpRunLocalFolderGen moves run id's record from guard's generation to the next, stamped at
	// (GenAt), changing nothing else, and answers the record as it then stands. False when the stored
	// record does not meet guard, or the run has no local folder.
	BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, guard FolderStatusGuard, at time.Time) (types.RunLocalFolderStatus, bool, error)
	// GiveUpRunLocalFolder gives up run id's owed copy back in one statement, once the run's keep or its
	// containment is due (GiveUpParams): it drops the Resume, sets Back to p.Back and moves Gen on, so
	// every later write from an owner of the older generation fails. Applied is false, with nothing
	// written, when the run is not due, is not terminal, holds no sandbox, has no folder, or its
	// record is already final; the caller reads the record to tell which.
	GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, p GiveUpParams) (GiveUpOutcome, error)
	// MarkGivenUpAudited records that a record's given_up audit row was accepted, at generation gen
	// only. False when the record is at another generation or not given up.
	MarkGivenUpAudited(ctx context.Context, id uuid.UUID, gen int) (bool, error)
	// ListGivenUpUnaudited pages, by id, the terminal runs whose given-up record has no accepted audit
	// row, limit at a time after the cursor. The cursor returned is the last id of a full page, and nil
	// after a short one, when the caller starts again from the lowest id.
	ListGivenUpUnaudited(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, *uuid.UUID, error)
}

// GiveUpParams is one give-up. Back is the final record to write. Every due runs from the run's end,
// COALESCE(ended_at, updated_at): the owed pass's keep ends Cap after it; when D1, a run also
// gives up Lag after it (ContainmentDueSQL with Lag = ContainmentLag), and when its containment failed
// durably Tau ago or Unsupported (no cut is possible for it).
type GiveUpParams struct {
	Back        types.FolderPassRecord
	Cap         time.Duration
	D1          bool
	Lag         time.Duration
	Unsupported bool
	Tau         time.Duration
}

// GiveUpOutcome is whether the statement applied, and the generation it moved the record to.
type GiveUpOutcome struct {
	Applied bool
	Gen     int
}

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

// BumpRunLocalFolderGen implements RunLocalFolderStatuses. Only gen and gen_at change, so a write that
// landed between the caller's read and this bump is kept. gen_at is the database's now(), whatever the caller's
// at says: the one clock that FolderGenPastBound later reads it against. (at is for stores that keep no clock.)
func (s PG) BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, guard FolderStatusGuard, _ time.Time) (types.RunLocalFolderStatus, bool, error) {
	pred, args := guard.sql(3)
	var raw []byte
	err := s.Pool.QueryRow(ctx, `UPDATE agent_runs
		SET local_folder_status = COALESCE(local_folder_status, '{}'::jsonb) || jsonb_build_object('gen', $2::int, 'gen_at', now())
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

// FolderGenAges stamps and ages a folder record's send time on the database's clock: whether its generation was
// sent bound ago or more, and a send time for a record that has none.
type FolderGenAges interface {
	// FolderGenPastBound is true when the record is at generation gen, has a send time (gen_at), and that time plus
	// bound has passed against now(). A record at another generation, or with no send time, is false.
	FolderGenPastBound(ctx context.Context, id uuid.UUID, gen int, bound time.Duration) (bool, error)
	// StampFolderGenAt sets the send time of a record at generation gen that has none to now(), and reports whether
	// it did. A record at another generation, or one with a send time, is left as it is.
	StampFolderGenAt(ctx context.Context, id uuid.UUID, gen int) (bool, error)
}

var _ FolderGenAges = PG{}

// FolderGenPastBound implements FolderGenAges.
func (s PG) FolderGenPastBound(ctx context.Context, id uuid.UUID, gen int, bound time.Duration) (bool, error) {
	var past bool
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(COALESCE((local_folder_status->>'gen')::int, 0) = $2
		   AND (local_folder_status->>'gen_at')::timestamptz + make_interval(secs => $3) <= now(), false)
		FROM agent_runs WHERE id = $1`, id, gen, bound.Seconds()).Scan(&past)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read folder generation age: %w", err)
	}
	return past, nil
}

// StampFolderGenAt implements FolderGenAges.
func (s PG) StampFolderGenAt(ctx context.Context, id uuid.UUID, gen int) (bool, error) {
	pred, args := FolderStatusGuard{Gen: gen}.sql(2)
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs
		SET local_folder_status = COALESCE(local_folder_status, '{}'::jsonb) || jsonb_build_object('gen_at', now())
		WHERE id = $1 AND local_folder IS NOT NULL AND (local_folder_status->>'gen_at') IS NULL AND `+pred,
		append([]any{id}, args...)...)
	if err != nil {
		return false, fmt.Errorf("store: stamp run local folder send time: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// terminalStateNames is the states RunState.IsTerminal names, as the text[] a `state = ANY($n)` argument takes.
func terminalStateNames() []string {
	return stateNames([]types.RunState{types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived})
}

// GiveUpRunLocalFolder implements RunLocalFolderStatuses. A run's lifetime cap and max age bound a live
// run and so are not terms here: nothing that already ended is ended again by them.
func (s PG) GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, p GiveUpParams) (GiveUpOutcome, error) {
	back, err := json.Marshal(p.Back)
	if err != nil {
		return GiveUpOutcome{}, fmt.Errorf("store: marshal given-up folder record: %w", err)
	}
	const q = `
		UPDATE agent_runs
		SET local_folder_status = (COALESCE(local_folder_status, '{}'::jsonb) - 'resume')
		    || jsonb_build_object('gen', COALESCE((local_folder_status->>'gen')::int, 0) + 1, 'back', $2::jsonb)
		WHERE id = $1
		  AND local_folder IS NOT NULL
		  AND sandbox_ref <> ''
		  AND state = ANY($3)
		  AND NOT (COALESCE(local_folder_status, '{}'::jsonb) ? 'back'
		           AND NOT (COALESCE(local_folder_status, '{}'::jsonb) ? 'resume'))
		  AND ( COALESCE(ended_at, updated_at) + make_interval(secs => $4) <= now()
		     OR ( $5::boolean
		          AND COALESCE(ended_at, updated_at) + make_interval(secs => $6) <= now()
		          AND ( $7::boolean
		             OR (containment_error_at IS NOT NULL
		                 AND containment_error_at <= now() - make_interval(secs => $8)) ) ) )
		RETURNING (local_folder_status->>'gen')::int`
	var gen int
	err = s.Pool.QueryRow(ctx, q, id, back, terminalStateNames(), p.Cap.Seconds(), p.D1, p.Lag.Seconds(), p.Unsupported, p.Tau.Seconds()).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return GiveUpOutcome{}, nil
	}
	if err != nil {
		return GiveUpOutcome{}, fmt.Errorf("store: give up run local folder: %w", err)
	}
	return GiveUpOutcome{Applied: true, Gen: gen}, nil
}

// MarkGivenUpAudited implements RunLocalFolderStatuses. It is a guarded write like SetRunLocalFolderStatusIf,
// and changes nothing but the one field.
func (s PG) MarkGivenUpAudited(ctx context.Context, id uuid.UUID, gen int) (bool, error) {
	pred, args := FolderStatusGuard{Gen: gen}.sql(2)
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs
		SET local_folder_status = jsonb_set(local_folder_status, '{back,given_up_audited}', 'true'::jsonb)
		WHERE id = $1 AND local_folder IS NOT NULL
		  AND (local_folder_status->'back'->>'given_up_at') IS NOT NULL AND `+pred, append([]any{id}, args...)...)
	if err != nil {
		return false, fmt.Errorf("store: mark given-up folder record audited: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListGivenUpUnaudited implements RunLocalFolderStatuses. Its predicate on the record is
// agent_runs_given_up_unaudited_idx's, spelled the same so the index serves it, in id order.
func (s PG) ListGivenUpUnaudited(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, *uuid.UUID, error) {
	q := `SELECT id FROM agent_runs
		WHERE state = ANY($1)
		  AND (local_folder_status->'back'->>'given_up_at') IS NOT NULL
		  AND COALESCE((local_folder_status->'back'->>'given_up_audited')::boolean, false) = false`
	args := []any{terminalStateNames()}
	if after != nil {
		q += ` AND id > $2`
		args = append(args, *after)
	}
	q += fmt.Sprintf(` ORDER BY id LIMIT $%d`, len(args)+1)
	rows, err := s.Pool.Query(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list given-up folder records without an audit row: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, nil, fmt.Errorf("store: scan given-up folder record: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list given-up folder records without an audit row: %w", err)
	}
	if len(ids) < limit || limit <= 0 {
		return ids, nil, nil
	}
	return ids, &ids[len(ids)-1], nil
}
