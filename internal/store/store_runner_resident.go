// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrResidentDeliveryRefused refuses a runner_resident delivery whose run is
// not STARTING, whose erase has begun or was abandoned, or whose delivery
// lease another dispatch holds.
var ErrResidentDeliveryRefused = errors.New("runner resident delivery refused")

// Why the org stopped waiting for a runner to confirm a resident erase: the
// closed set of runner_resident_runs.abandoned_reason.
const (
	ResidentAbandonRunnerRevoked = "runner_revoked"
	ResidentAbandonRunnerDeleted = "runner_deleted"
	ResidentAbandonExpired       = "expired_unconfirmed"
)

const (
	// residentAbandonAfter is how long past its not_after an obligation may
	// stay unconfirmed before the org closes it unconfirmed.
	residentAbandonAfter = 24 * time.Hour
	// residentAbandonPage bounds one abandonment transaction.
	residentAbandonPage = 100
	// residentReasonMax bounds erase_last_reason.
	residentReasonMax = 512
)

// residentEraseDueSQL is an open obligation an erase may claim now: no live
// delivery lease, its backoff over, and its run ended. $1 is now, $2 the
// non-terminal states (types.NonTerminalRunStates, pinned to IsTerminal).
const residentEraseDueSQL = `r.erased_at IS NULL AND r.abandoned_at IS NULL AND r.next_erase_at <= $1
	AND (r.delivery_lease_until IS NULL OR r.delivery_lease_until < $1) AND NOT (a.state = ANY($2))`

func pgTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func nonTerminalStates() []string {
	out := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		out = append(out, string(st))
	}
	return out
}

// BeginResidentDelivery opens (or renews, for the same holder) the run's
// obligation row and its delivery lease before any value is sent. It takes the
// runner → run → execution lock order and requires the runner claimed and not
// revoked, the run bound to it, owned by its owner and STARTING, no erase begun
// or abandoned, and any other holder's lease expired. A second begin keeps the
// earlier not_after when it is sooner. The runner binding refusals are
// lockRunnerActionRun's (ErrNotFound, ErrRunnerActionRefused); the rest are
// ErrResidentDeliveryRefused.
func (s PG) BeginResidentDelivery(ctx context.Context, runID, runnerID, holder uuid.UUID, now, until, notAfter time.Time) error {
	now, until, notAfter = pgTime(now), pgTime(until), pgTime(notAfter)
	if holder == uuid.Nil || !until.After(now) || !notAfter.After(now) {
		return ErrResidentDeliveryRefused
	}
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	_, run, _, err := lockRunnerActionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return err
	}
	if run.State != types.RunStarting {
		return ErrResidentDeliveryRefused
	}
	tag, err := tx.Exec(ctx, `INSERT INTO runner_resident_runs AS r (run_id, runner_id, not_after, delivery_holder, delivery_lease_until, next_erase_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (run_id) DO UPDATE SET delivery_holder = EXCLUDED.delivery_holder,
			delivery_lease_until = EXCLUDED.delivery_lease_until, not_after = LEAST(r.not_after, EXCLUDED.not_after)
		WHERE r.runner_id = EXCLUDED.runner_id AND r.erase_begun_at IS NULL AND r.abandoned_at IS NULL
			AND (r.delivery_holder = EXCLUDED.delivery_holder OR r.delivery_lease_until IS NULL OR r.delivery_lease_until < $6)`,
		runID, runnerID, notAfter, holder, until, now)
	if err != nil {
		return fmt.Errorf("store: begin resident delivery: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrResidentDeliveryRefused
	}
	return tx.Commit(ctx)
}

// RenewResidentDelivery extends holder's delivery lease to until, before the
// next value is sent. It refuses once the run left STARTING, an erase began or
// the obligation was abandoned, or another holder took the row.
func (s PG) RenewResidentDelivery(ctx context.Context, runID, holder uuid.UUID, until time.Time) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE runner_resident_runs r SET delivery_lease_until = $3 FROM agent_runs a
		WHERE r.run_id = $1 AND a.id = r.run_id AND r.delivery_holder = $2
			AND r.erase_begun_at IS NULL AND r.abandoned_at IS NULL AND a.state = $4`,
		runID, holder, pgTime(until), string(types.RunStarting))
	if err != nil {
		return fmt.Errorf("store: renew resident delivery: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrResidentDeliveryRefused
	}
	return nil
}

// EndResidentDelivery releases holder's delivery lease, so an erase may claim
// the obligation once the run has ended. Releasing a lease holder no longer
// holds is a no-op.
func (s PG) EndResidentDelivery(ctx context.Context, runID, holder uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runner_resident_runs SET delivery_holder = NULL, delivery_lease_until = NULL
		WHERE run_id = $1 AND delivery_holder = $2`, runID, holder)
	if err != nil {
		return fmt.Errorf("store: end resident delivery: %w", err)
	}
	return nil
}

// DueResidentEraseRunners lists the runners with an obligation an erase may
// claim now.
func (s PG) DueResidentEraseRunners(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	q := `SELECT DISTINCT r.runner_id FROM runner_resident_runs r JOIN agent_runs a ON a.id = r.run_id WHERE ` +
		residentEraseDueSQL + ` ORDER BY r.runner_id`
	return collect(ctx, s.Pool, "list", "due resident erase runners", q, []any{pgTime(now), nonTerminalStates()}, func(row pgx.Row) (uuid.UUID, error) {
		var id uuid.UUID
		return id, row.Scan(&id)
	})
}

// ClaimResidentErase claims at most limit of runnerID's due obligations, in one
// compare-and-set that skips rows another claim holds: it stamps
// erase_begun_at once, counts the attempt and moves next_erase_at out by the
// backoff (30 s, doubling per attempt, at most 1 h), so a claimer that dies
// leaves the row to the next claim after it. It returns the claimed runs.
func (s PG) ClaimResidentErase(ctx context.Context, runnerID uuid.UUID, now time.Time, limit int) ([]uuid.UUID, error) {
	q := `WITH due AS (SELECT r.run_id FROM runner_resident_runs r JOIN agent_runs a ON a.id = r.run_id
			WHERE r.runner_id = $3 AND ` + residentEraseDueSQL + `
			ORDER BY r.next_erase_at, r.run_id LIMIT $4 FOR UPDATE OF r SKIP LOCKED)
		UPDATE runner_resident_runs r SET erase_begun_at = COALESCE(r.erase_begun_at, $1), erase_attempts = r.erase_attempts + 1,
			next_erase_at = $1::timestamptz + LEAST(interval '30 seconds' * power(2, LEAST(r.erase_attempts, 7)), interval '1 hour')
		FROM due WHERE r.run_id = due.run_id RETURNING r.run_id`
	return collect(ctx, s.Pool, "claim", "resident erase", q, []any{pgTime(now), nonTerminalStates(), runnerID, limit}, func(row pgx.Row) (uuid.UUID, error) {
		var id uuid.UUID
		return id, row.Scan(&id)
	})
}

// MarkResidentErased closes the run's obligation: its runner erased and
// reported every resident value of the run.
func (s PG) MarkResidentErased(ctx context.Context, runID uuid.UUID, now time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runner_resident_runs SET erased_at = $2, erase_last_reason = NULL
		WHERE run_id = $1 AND erased_at IS NULL`, runID, pgTime(now))
	if err != nil {
		return fmt.Errorf("store: mark resident erased: %w", err)
	}
	return nil
}

// NoteResidentEraseFailed records why the last erase attempt failed; the
// claim already set when the next one is due.
func (s PG) NoteResidentEraseFailed(ctx context.Context, runID uuid.UUID, reason string) error {
	if len(reason) > residentReasonMax {
		reason = strings.ToValidUTF8(reason[:residentReasonMax], "")
	}
	_, err := s.Pool.Exec(ctx, `UPDATE runner_resident_runs SET erase_last_reason = $2
		WHERE run_id = $1 AND erased_at IS NULL AND abandoned_at IS NULL`, runID, reason)
	if err != nil {
		return fmt.Errorf("store: note resident erase failure: %w", err)
	}
	return nil
}

// AbandonResidentErase closes, unconfirmed, the open obligations whose runner
// was revoked or is gone, or whose not_after passed more than 24 h ago: the
// runner erases on its own then (on revocation, at not_after). Each closed
// obligation writes runner.resident.erase, outcome failure and data outcome
// "unconfirmed", once per runner_resident grant of the run (once for the run
// when it has none), through prepare and in the same transaction as the close,
// so the row and its audit commit or roll back together. It returns how many
// obligations it closed; one call closes at most a page of them.
func (s PG) AbandonResidentErase(ctx context.Context, now time.Time,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (int, error) {
	if prepare == nil {
		return 0, errors.New("store: abandon resident erase needs the audit prepare policy")
	}
	now = pgTime(now)
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("store: begin abandon resident erase: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	type abandoned struct {
		run, runner uuid.UUID
		reason      string
	}
	rows, err := collect(ctx, tx, "list", "abandoned resident erases", `SELECT r.run_id, r.runner_id,
			CASE WHEN rn.id IS NULL THEN $2 WHEN rn.state = 'revoked' OR rn.revoked_at IS NOT NULL THEN $3 ELSE $4 END
		FROM runner_resident_runs r LEFT JOIN runners rn ON rn.id = r.runner_id
		WHERE r.erased_at IS NULL AND r.abandoned_at IS NULL
			AND (rn.id IS NULL OR rn.state = 'revoked' OR rn.revoked_at IS NOT NULL OR r.not_after < $1)
		ORDER BY r.run_id LIMIT $5 FOR UPDATE OF r SKIP LOCKED`,
		[]any{now.Add(-residentAbandonAfter), ResidentAbandonRunnerDeleted, ResidentAbandonRunnerRevoked, ResidentAbandonExpired, residentAbandonPage},
		func(row pgx.Row) (abandoned, error) {
			var a abandoned
			return a, row.Scan(&a.run, &a.runner, &a.reason)
		})
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return 0, err
	}
	for _, a := range rows {
		if _, err := tx.Exec(ctx, `UPDATE runner_resident_runs SET abandoned_at = $2, abandoned_reason = $3 WHERE run_id = $1`,
			a.run, now, a.reason); err != nil {
			return 0, fmt.Errorf("store: abandon resident erase: %w", err)
		}
		if err := auditResidentAbandonTx(ctx, tx, a.run, a.runner, a.reason, prepare); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// auditResidentAbandonTx writes the org's runner.resident.erase rows for one
// abandoned obligation inside tx.
func auditResidentAbandonTx(ctx context.Context, tx pgx.Tx, runID, runnerID uuid.UUID, reason string,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) error {
	grants, err := collect(ctx, tx, "list", "resident grants", `SELECT id FROM credential_grants WHERE run_id = $1 AND delivery = $2 ORDER BY id`,
		[]any{runID, string(types.GrantDeliveryResident)}, func(row pgx.Row) (uuid.UUID, error) {
			var id uuid.UUID
			return id, row.Scan(&id)
		})
	if err != nil {
		return err
	}
	var arrival time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&arrival); err != nil {
		return err
	}
	targets := grants
	if len(targets) == 0 {
		targets = []uuid.UUID{uuid.Nil}
	}
	for _, g := range targets {
		data := map[string]any{"outcome": "unconfirmed", "reason": reason, "runner_id": runnerID}
		target := runID.String()
		if g != uuid.Nil {
			data["grant_id"], target = g, g.String()
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		ev, err := prepare(ctx, types.AuditEvent{ID: uuid.New(), RunID: &runID, Time: arrival, ActorType: types.ActorSystem, Actor: "wardynd",
			Action: "runner.resident.erase", Target: target, Outcome: "failure", Data: raw})
		if err != nil {
			return err
		}
		if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
			return err
		}
	}
	return nil
}
