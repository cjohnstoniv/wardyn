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

// LaunchIntentStore reads and erases launch intents (migration 0171_run_launch_intents): the setup a
// person launched a run with, kept as a kind Run document for Run again. It has no write method: CreateRun
// and CreateRunUnderCap insert AgentRun.LaunchIntent in the run row's own transaction, so a run and its
// intent are written together or not at all. Optional like ComponentStore: callers type-assert.
type LaunchIntentStore interface {
	// ListLaunchIntents returns createdBy's intents, newest first, at most limit, without documents.
	ListLaunchIntents(ctx context.Context, createdBy string, limit int) ([]LaunchIntentRecord, error)
	// GetLaunchIntent returns the run's intent when createdBy created the run; ErrNotFound otherwise.
	GetLaunchIntent(ctx context.Context, runID uuid.UUID, createdBy string) (LaunchIntentRecord, error)
	// DeleteLaunchIntentsCreatedBy erases createdBy's intents and returns how many. It waits for every
	// launch of theirs that is writing one, and a launch that reaches the write after it began keeps none.
	DeleteLaunchIntentsCreatedBy(ctx context.Context, createdBy string) (int, error)
}

var _ LaunchIntentStore = PG{}

// LaunchIntentRecord is one kept intent. Title is the run's title now; Shape is the document's
// intent.shape. Document is set by GetLaunchIntent only.
type LaunchIntentRecord struct {
	RunID     uuid.UUID
	Title     string
	Shape     string
	CreatedAt time.Time
	Document  json.RawMessage
}

// errLaunchIntentOutsideTx refuses an intent write handed anything but a transaction: on a pool the
// run row and the intent would be two commits, and the person lock would end with its own statement.
var errLaunchIntentOutsideTx = errors.New("store: a launch intent is written only inside the run's transaction")

// The person lock linearizes a launch's intent against an erasure of its creator's intents (D25-C). A
// launch takes it shared, without waiting, as the first statement of the transaction that inserts its run,
// and holds it to commit. An erasure takes it exclusively and waits. So a launch that took it first commits
// before the erasure deletes, and the erasure removes its intent; a launch that finds an erasure holding it,
// or queued for it, keeps no intent and still launches. Keyed by a hash of the creator: two people who share
// a hash only make one launch keep no intent while the other's erasure runs.
const (
	launchIntentTryLockSQL = `SELECT pg_try_advisory_xact_lock_shared(hashtext($1))`
	launchIntentEraseSQL   = `SELECT pg_advisory_xact_lock(hashtext($1))`
)

func launchIntentPersonKey(createdBy string) string {
	return "run_launch_intents.person\x1f" + createdBy
}

// lockLaunchIntent is the first statement of a run insert that carries r.LaunchIntent: it reports whether
// the intent may be written in this transaction. No intent, nothing taken.
func lockLaunchIntent(ctx context.Context, q queryRower, r types.AgentRun) (bool, error) {
	if r.LaunchIntent == nil {
		return false, nil
	}
	if _, ok := q.(pgx.Tx); !ok {
		return false, errLaunchIntentOutsideTx
	}
	var held bool
	if err := q.QueryRow(ctx, launchIntentTryLockSQL, launchIntentPersonKey(r.CreatedBy)).Scan(&held); err != nil {
		return false, fmt.Errorf("store: lock launch intent: %w", err)
	}
	return held, nil
}

// insertLaunchIntent writes the intent of the run just inserted on q, the same transaction, and returns
// the run with it set.
func insertLaunchIntent(ctx context.Context, q queryRower, run types.AgentRun, doc json.RawMessage) (types.AgentRun, error) {
	if _, ok := q.(pgx.Tx); !ok {
		return run, errLaunchIntentOutsideTx
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `INSERT INTO run_launch_intents (run_id, created_by, document, created_at)
		VALUES ($1, $2, $3, $4) RETURNING run_id`, run.ID, run.CreatedBy, []byte(doc), run.CreatedAt).Scan(&id); err != nil {
		return run, fmt.Errorf("store: insert launch intent: %w", err)
	}
	run.LaunchIntent = doc
	return run, nil
}

// createRunWithIntent is CreateRun's body when the run carries an intent: always one transaction, holding
// the owner's identity rows when ctx carries a guard, as guarded does.
func (s PG) createRunWithIntent(ctx context.Context, fn func(q queryRower) error) error {
	g, guarded := identityGuardFrom(ctx)
	var since int64
	if guarded {
		var err error
		if since, err = ReadRevocationSeq(ctx, s.Pool); err != nil {
			return err
		}
	}
	return s.inTx(ctx, func(q Querier) error {
		if guarded {
			if err := checkIdentityGuard(ctx, q.(pgx.Tx), g, since); err != nil {
				return err
			}
		}
		return fn(q)
	})
}

const launchIntentCols = `i.run_id, r.title, COALESCE(i.document #>> '{intent,shape}', ''), i.created_at`

func scanLaunchIntent(row pgx.Row, dst ...any) (LaunchIntentRecord, error) {
	var rec LaunchIntentRecord
	err := row.Scan(append([]any{&rec.RunID, &rec.Title, &rec.Shape, &rec.CreatedAt}, dst...)...)
	return rec, err
}

// ListLaunchIntents — see LaunchIntentStore.
func (s PG) ListLaunchIntents(ctx context.Context, createdBy string, limit int) ([]LaunchIntentRecord, error) {
	return collect(ctx, s.Pool, "list", "launch intents", `SELECT `+launchIntentCols+`
		FROM run_launch_intents i JOIN agent_runs r ON r.id = i.run_id
		WHERE i.created_by = $1 ORDER BY i.created_at DESC, i.run_id DESC LIMIT $2`,
		[]any{createdBy, limit}, func(row pgx.Row) (LaunchIntentRecord, error) { return scanLaunchIntent(row) })
}

// GetLaunchIntent — see LaunchIntentStore.
func (s PG) GetLaunchIntent(ctx context.Context, runID uuid.UUID, createdBy string) (LaunchIntentRecord, error) {
	var doc []byte
	rec, err := scanLaunchIntent(s.Pool.QueryRow(ctx, `SELECT `+launchIntentCols+`, i.document
		FROM run_launch_intents i JOIN agent_runs r ON r.id = i.run_id
		WHERE i.run_id = $1 AND i.created_by = $2`, runID, createdBy), &doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return LaunchIntentRecord{}, ErrNotFound
	}
	if err != nil {
		return LaunchIntentRecord{}, fmt.Errorf("store: get launch intent: %w", err)
	}
	rec.Document = doc
	return rec, nil
}

// DeleteLaunchIntentsCreatedBy — see LaunchIntentStore.
func (s PG) DeleteLaunchIntentsCreatedBy(ctx context.Context, createdBy string) (int, error) {
	if createdBy == "" {
		return 0, errors.New("store: erase launch intents: a person is required")
	}
	var n int
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, launchIntentEraseSQL, launchIntentPersonKey(createdBy)); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `DELETE FROM run_launch_intents WHERE created_by = $1`, createdBy)
		n = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: erase launch intents: %w", err)
	}
	return n, nil
}
