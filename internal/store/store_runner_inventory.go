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

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerInventoryStore is the optional capability behind the runners management routes: a page of
// the inventory, the unused registration tokens and the count of runs on each runner. Revoking a
// runner is the revoke door's, not this one's.
type RunnerInventoryStore interface {
	RunnerStore
	// ListRunnersPage lists the runners the filter names, newest first, never an unclaimed one that
	// has outlived its wait at now (the sweeper may not have deleted the row yet).
	ListRunnersPage(ctx context.Context, filter types.RunnerFilter, now time.Time, p Page) ([]types.Runner, error)
	// ListUnusedRunnerRegistrationTokensPage lists tokens redeemable at now, newest first, for one
	// owner or, with owner "", every owner.
	ListUnusedRunnerRegistrationTokensPage(ctx context.Context, now time.Time, owner string, p Page) ([]types.RunnerRegistrationToken, error)
	RevokeRunnerRegistrationToken(ctx context.Context, id uuid.UUID, now time.Time) (types.RunnerRegistrationToken, error)
	// RunnerSlotHolders counts the runs holding a sandbox on each of ids, the count max_runs bounds.
	RunnerSlotHolders(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error)
	// RunnerUsage is each named runner's In use, by key (a self-hosted runner's id as text, or a
	// remote-hosted runner's id): the figures admission counts, read without a lock.
	RunnerUsage(ctx context.Context, runners []uuid.UUID, remotes []string, except uuid.UUID) (map[string]types.RunnerTally, error)
	// RunnerPoolsOf names, for each of ids, its owner's live Self-Hosted pools it is a member of, by
	// name; a runner in none is absent.
	RunnerPoolsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]types.RunnerPoolRef, error)
	// UpdateRunnerLimits is the owner's declaration of their own live runner's limits.
	UpdateRunnerLimits(ctx context.Context, id uuid.UUID, owner string, limits types.RunnerLimits) (before *types.RunnerLimits, after types.Runner, err error)
}

var _ RunnerInventoryStore = PG{}

// ListRunnersPage — see RunnerInventoryStore.
func (s PG) ListRunnersPage(ctx context.Context, filter types.RunnerFilter, now time.Time, p Page) ([]types.Runner, error) {
	stateClause := map[types.RunnerFilter]string{
		types.RunnerFilterActive: `state <> 'revoked'`, types.RunnerFilterRevoked: `state = 'revoked'`, types.RunnerFilterAll: `true`,
	}[filter]
	if stateClause == "" {
		return nil, fmt.Errorf("store: list runners: unknown filter %q", filter)
	}
	q, args := p.appendTo(`SELECT `+runnerCols+` FROM runners WHERE `+stateClause+
		` AND (state <> 'unclaimed' OR created_at > $1) ORDER BY created_at DESC, id`, []any{now.Add(-types.RunnerUnclaimedTTL)})
	return collect(ctx, s.Pool, "list", "runners", q, args, scanRunner)
}

// ListUnusedRunnerRegistrationTokensPage — see RunnerInventoryStore. Never a token value: the table
// holds only its hash, which the wire type does not serialize.
func (s PG) ListUnusedRunnerRegistrationTokensPage(ctx context.Context, now time.Time, owner string, p Page) ([]types.RunnerRegistrationToken, error) {
	q, args := p.appendTo(`SELECT `+runnerRegistrationCols+` FROM runner_registration_tokens
		WHERE consumed_at IS NULL AND expires_at > $1 AND ($2 = '' OR owner = $2) ORDER BY created_at DESC, id`, []any{now, owner})
	return collect(ctx, s.Pool, "list", "runner registration tokens", q, args, scanRunnerRegistrationToken)
}

// RevokeRunnerRegistrationToken spends an unused token by setting the column
// ConsumeRunnerRegistrationToken requires NULL, in one conditional UPDATE: a revoke and a
// redemption of the same token take the same row lock, so exactly one of them wins. A token that
// was redeemed, revoked, expired or never existed is ErrNotFound.
func (s PG) RevokeRunnerRegistrationToken(ctx context.Context, id uuid.UUID, now time.Time) (types.RunnerRegistrationToken, error) {
	const q = `UPDATE runner_registration_tokens SET consumed_at = $2
		WHERE id = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING ` + runnerRegistrationCols
	return scanRunnerRegistrationToken(s.Pool.QueryRow(ctx, q, id, now))
}

// RunnerInventoryResult describes committed intake, not permission to dispatch.
// Reconciliation requirements are durable even if the caller loses its reply.
type RunnerInventoryResult struct {
	Complete            bool
	NeedsReconciliation bool
	// AssertedCapHeld reports a page that met a new asserted subject past
	// the runner's cap: nothing was queued and the runner stays not-ready.
	AssertedCapHeld bool
}

// BeginRunnerInventory captures original candidates before fresh PENDING/STATE.
// The authenticated Hub invokes it under its per-runner session admission lock.
// Repeating the same session never resets a partially consumed inventory.
func (s PG) BeginRunnerInventory(ctx context.Context, runnerID uuid.UUID, sessionID string) error {
	session, err := uuid.Parse(sessionID)
	if err != nil || session == uuid.Nil {
		return ErrRunnerExecutionRefused
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed transaction is closed
	owner, err := inventoryOwner(ctx, tx, runnerID)
	if err != nil {
		return err
	}
	var current uuid.UUID
	err = tx.QueryRow(ctx, `SELECT session_id FROM runner_inventories WHERE runner_id=$1 FOR UPDATE`, runnerID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if current == session {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM runner_inventories WHERE runner_id=$1`, runnerID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO runner_inventories(runner_id,session_id,owner) VALUES($1,$2,$3)`, runnerID, session, owner); err != nil {
		return err
	}
	// xmin is a conservative snapshot version: even an unrelated committed run
	// change prevents an omission from being attributed to the old incarnation.
	// No entire candidate set is materialized in application memory.
	_, err = tx.Exec(ctx, `INSERT INTO runner_inventory_candidates(runner_id,run_id,run_version,ref,generation)
		SELECT $1,a.id,a.xmin::text,a.sandbox_ref,COALESCE(e.generation,$3)
		FROM agent_runs a LEFT JOIN runner_executions e ON e.run_id=a.id
		WHERE a.runner_id=$1 AND ($2='' OR a.created_by=$2) AND a.placement='local'
		AND a.state='RUNNING' AND a.lost_at IS NULL AND a.sandbox_ref<>''`, runnerID, owner, uuid.Nil)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// inventoryOwner is the owner a claimed, unrevoked runner's inventory is scoped to: a person's runner
// its owner's runs, an org runner's empty owner every run bound to it (runnerReportsOn's rule, which the
// candidate and omission queries apply as an empty $2 or created_by = $2).
func inventoryOwner(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID) (string, error) {
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='2s'`); err != nil {
		return "", err
	}
	var owner string
	err := tx.QueryRow(ctx, `SELECT owner FROM runners WHERE id=$1 AND state='claimed' AND revoked_at IS NULL
		AND ((owner_kind='person' AND owner<>'') OR (owner_kind='org' AND owner='')) FOR SHARE`, runnerID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrRunnerExecutionRefused
	}
	return owner, err
}

// IntakeRunnerStatePage commits a bounded page and its cursor together. A
// partial page never checks omissions. This path performs no wire call,
// credential renewal, lifecycle transition or accepted-exit acknowledgement.
func (s PG) IntakeRunnerStatePage(ctx context.Context, runnerID uuid.UUID, sessionID string, page runnerwire.StatePage) (RunnerInventoryResult, error) {
	var out RunnerInventoryResult
	session, err := uuid.Parse(sessionID)
	if err != nil || session == uuid.Nil {
		return out, ErrRunnerExecutionRefused
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed transaction is closed
	owner, err := inventoryOwner(ctx, tx, runnerID)
	if err != nil {
		return out, err
	}
	var cursor runnerwire.StateInventory
	err = tx.QueryRow(ctx, `SELECT COALESCE(inventory_id,$4),last_run_id,complete,needs_reconciliation
		FROM runner_inventories WHERE runner_id=$1 AND session_id=$2 AND owner=$3 FOR UPDATE`, runnerID, session, owner, uuid.Nil).
		Scan(&cursor.ID, &cursor.Last, &cursor.Complete, &out.NeedsReconciliation)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrRunnerExecutionRefused
	}
	if err != nil {
		return out, err
	}
	next, err := cursor.Next(page)
	if err != nil {
		return out, err
	}
	for _, row := range page.Runs {
		needs, capped, err := s.intakeRunnerStateRow(ctx, tx, runnerID, row)
		if err != nil {
			return out, err
		}
		out.NeedsReconciliation = out.NeedsReconciliation || needs
		out.AssertedCapHeld = out.AssertedCapHeld || capped
	}
	if next.Complete {
		var omitted bool
		if err := tx.QueryRow(ctx, inventoryOmissionsSQL, runnerID, owner, uuid.Nil).Scan(&omitted); err != nil {
			return out, err
		}
		out.NeedsReconciliation = out.NeedsReconciliation || omitted
	}
	_, err = tx.Exec(ctx, `UPDATE runner_inventories SET inventory_id=$2,last_run_id=$3,complete=$4,needs_reconciliation=$5 WHERE runner_id=$1`, runnerID, next.ID, next.Last, next.Complete, out.NeedsReconciliation)
	if err != nil {
		return out, err
	}
	out.Complete = next.Complete
	return out, tx.Commit(ctx)
}

func (s PG) intakeRunnerStateRow(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, row runnerwire.RunState) (needs, capped bool, err error) {
	if row.SpoolDepth < 0 || row.SpoolDrops < 0 {
		return false, false, ErrRunnerExecutionRefused
	}
	assertion, err := json.Marshal(row)
	if err != nil {
		return false, false, err
	}
	containment, needsLoss, err := s.reconcileRunnerStateRowTx(ctx, tx, runnerID, row)
	if errors.Is(err, ErrNotFound) {
		// No org row: contain the asserted subject per run, so one erased run
		// never refuses the page for the runner's other runs.
		if needs, capped, err = assertedSubjectTx(ctx, tx, runnerID, row); err != nil {
			return false, false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO runner_inventory_observations(runner_id,run_id,assertion) VALUES($1,$2,$3)`, runnerID, row.RunID, assertion)
		return needs, capped, err
	}
	if err != nil {
		return false, false, err
	}
	var proof []byte
	if containment != nil {
		proof, err = json.Marshal(containment)
		if err != nil {
			return false, false, err
		}
		// The stop_proxy commits with the page, so a refused Final always
		// leaves it durable for the next fresh PENDING.
		lostAt := containment.LostAt
		if _, _, err = queueRunnerActionTx(ctx, tx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: runnerID, RunID: row.RunID,
			Kind: types.RunnerActionStopProxy, Ref: containment.Ref, Generation: containment.Generation, LostAt: &lostAt}); err != nil {
			return false, false, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO runner_inventory_observations(runner_id,run_id,assertion,containment,needs_loss) VALUES($1,$2,$3,$4,$5)`, runnerID, row.RunID, assertion, proof, needsLoss); err != nil {
		return false, false, err
	}
	_, err = tx.Exec(ctx, `UPDATE runner_inventory_candidates SET seen=true WHERE runner_id=$1 AND run_id=$2`, runnerID, row.RunID)
	return needsLoss || containment != nil, false, err
}

const inventoryOmissionsSQL = `SELECT EXISTS (
	SELECT 1 FROM runner_inventory_candidates c JOIN agent_runs a ON a.id=c.run_id
	LEFT JOIN runner_executions e ON e.run_id=a.id
	WHERE c.runner_id=$1 AND NOT c.seen AND a.runner_id=$1 AND ($2='' OR a.created_by=$2)
	AND a.placement='local' AND a.state='RUNNING' AND a.lost_at IS NULL
	AND a.sandbox_ref=c.ref AND a.xmin::text=c.run_version AND COALESCE(e.generation,$3)=c.generation
)`
