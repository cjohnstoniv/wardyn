// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SystemAuditActor is the actor an unattended retention drop (WARDYN_AUDIT_RETENTION_AUTODROP) records:
// audit_retention_drop treats exactly this name as the system and marks the chained event unattested.
const SystemAuditActor = "wardynd"

// The refusal reasons audit_retention_drop (migration 0119) raises, one SQLSTATE each. They are also the
// wire reasons of POST /audit/retention/drop, registered in internal/authz.
const (
	RetentionNotOldest      = "audit_retention_not_oldest"
	RetentionNotClosed      = "audit_retention_not_closed"
	RetentionInsideWindow   = "audit_retention_inside_window"
	RetentionLiveRun        = "audit_retention_live_run"
	RetentionDigestMismatch = "audit_retention_digest_mismatch"
)

// retentionSQLState maps the SQLSTATEs of audit_retention_drop to reasons. WR006 (not a partition of the
// audit log) is ErrNotFound.
var retentionSQLState = map[string]string{
	"WR001": RetentionNotOldest,
	"WR002": RetentionNotClosed,
	"WR003": RetentionInsideWindow,
	"WR004": RetentionLiveRun,
	"WR005": RetentionDigestMismatch,
}

// AuditRetentionRefused is a drop the database refused. Reason is one of the Retention* constants.
type AuditRetentionRefused struct {
	Reason    string
	Partition string
}

func (e *AuditRetentionRefused) Error() string {
	return fmt.Sprintf("store: audit retention drop of %s refused: %s", e.Partition, e.Reason)
}

// The retention wire types live in internal/types, shared with pkg/client; these are their store-side names.
type (
	AuditRetentionPolicy       = types.AuditRetentionPolicy
	AuditRetentionPartition    = types.AuditRetentionPartition
	AuditRetentionStatus       = types.AuditRetentionStatus
	AuditRetentionDrop         = types.AuditRetentionDrop
	AuditRetentionPolicyChange = types.AuditRetentionPolicyChange
)

// AuditRetention is the OPTIONAL store capability behind the retention endpoints, the daily partition
// ensure and the leader sweeper's autodrop: a test fake or non-Postgres store has no partitions.
type AuditRetention interface {
	AuditRetentionStatus(ctx context.Context) (AuditRetentionStatus, error)
	// DropAuditPartition calls audit_retention_drop. A refusal is *AuditRetentionRefused; a name that is
	// not a partition of the audit log is ErrNotFound.
	DropAuditPartition(ctx context.Context, partition, digest, actor string) (AuditRetentionDrop, error)
	// AutodropAuditPartition drops the oldest partition if it is eligible, computing its digest in the
	// database, as the system actor. ok is false (and nothing was dropped) when it is not.
	AutodropAuditPartition(ctx context.Context) (drop AuditRetentionDrop, ok bool, err error)
	// SetAuditRetentionPolicy calls audit_retention_set_policy.
	SetAuditRetentionPolicy(ctx context.Context, days int) (AuditRetentionPolicyChange, error)
	// EnsureAuditPartitions calls audit_ensure_partitions and returns how many months it created.
	EnsureAuditPartitions(ctx context.Context, months int) (int, error)
	// AuditPartitionsAhead is how many months past the current one already have a partition.
	AuditPartitionsAhead(ctx context.Context) (int, error)
}

var _ AuditRetention = PG{}

// retentionAnnotate turns a database refusal into the typed error.
func retentionAnnotate(err error, partition string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code == "WR006" {
		return ErrNotFound
	}
	if reason, ok := retentionSQLState[pgErr.Code]; ok {
		return &AuditRetentionRefused{Reason: reason, Partition: partition}
	}
	return err
}

func (s PG) DropAuditPartition(ctx context.Context, partition, digest, actor string) (AuditRetentionDrop, error) {
	var d AuditRetentionDrop
	err := s.Pool.QueryRow(ctx, `SELECT dropped_partition, dropped_rows, COALESCE(dropped_seq_lo, 0), COALESCE(dropped_seq_hi, 0), dropped_digest, dropped_event_seq
		FROM audit_retention_drop($1, $2, $3)`, partition, digest, actor).
		Scan(&d.Partition, &d.Rows, &d.SeqLo, &d.SeqHi, &d.Digest, &d.EventSeq)
	if err != nil {
		return AuditRetentionDrop{}, retentionAnnotate(err, partition)
	}
	return d, nil
}

func (s PG) AutodropAuditPartition(ctx context.Context) (AuditRetentionDrop, bool, error) {
	var name string
	var eligible bool
	err := s.Pool.QueryRow(ctx, `SELECT part_name, part_eligible FROM audit_retention_partitions(NULL, false) LIMIT 1`).Scan(&name, &eligible)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !eligible) {
		return AuditRetentionDrop{}, false, nil
	}
	if err != nil {
		return AuditRetentionDrop{}, false, fmt.Errorf("store: read the oldest audit partition: %w", err)
	}
	var digest string
	if err := s.Pool.QueryRow(ctx, `SELECT audit_partition_digest($1)`, name).Scan(&digest); err != nil {
		return AuditRetentionDrop{}, false, fmt.Errorf("store: digest of audit partition %s: %w", name, err)
	}
	d, err := s.DropAuditPartition(ctx, name, digest, SystemAuditActor)
	var refused *AuditRetentionRefused
	if errors.As(err, &refused) || errors.Is(err, ErrNotFound) {
		// Eligible a moment ago, not now (a run started, another replica dropped it): nothing to do.
		return AuditRetentionDrop{}, false, nil
	}
	if err != nil {
		return AuditRetentionDrop{}, false, fmt.Errorf("store: drop audit partition %s: %w", name, err)
	}
	return d, true, nil
}

func (s PG) SetAuditRetentionPolicy(ctx context.Context, days int) (AuditRetentionPolicyChange, error) {
	var c AuditRetentionPolicyChange
	err := s.Pool.QueryRow(ctx, `SELECT outcome, effective_days, pending_days, pending_effective_at
		FROM audit_retention_set_policy($1)`, days).
		Scan(&c.Outcome, &c.EffectiveDays, &c.PendingDays, &c.PendingEffectiveAt)
	if err != nil {
		return AuditRetentionPolicyChange{}, fmt.Errorf("store: set audit retention policy: %w", err)
	}
	c.Days = c.EffectiveDays
	return c, nil
}

func (s PG) EnsureAuditPartitions(ctx context.Context, months int) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT audit_ensure_partitions($1)`, months).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: ensure audit partitions: %w", err)
	}
	return n, nil
}

// partitionsAheadSQL is the month distance, in UTC, from the current month to the newest partition's
// upper bound, less the one month the upper bound itself sits past.
const partitionsAheadSQL = `
	SELECT COALESCE((
	    SELECT ((extract(year FROM hi AT TIME ZONE 'UTC') * 12 + extract(month FROM hi AT TIME ZONE 'UTC'))
	          - (extract(year FROM now() AT TIME ZONE 'UTC') * 12 + extract(month FROM now() AT TIME ZONE 'UTC')) - 1)::int
	      FROM (SELECT max((e->>'hi')::timestamptz) AS hi FROM audit_partition_meta m, jsonb_array_elements(m.manifest) e) x
	     WHERE hi IS NOT NULL), 0)`

func (s PG) AuditPartitionsAhead(ctx context.Context) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, partitionsAheadSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: read audit partitions ahead: %w", err)
	}
	return n, nil
}

func (s PG) AuditRetentionStatus(ctx context.Context) (AuditRetentionStatus, error) {
	// One snapshot, so the policy, the partitions and their eligibility describe the same instant.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AuditRetentionStatus{}, fmt.Errorf("store: begin audit retention status: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only: nothing to undo

	var st AuditRetentionStatus
	if err := tx.QueryRow(ctx, `SELECT retention_days, audit_retention_window(), pending_days, pending_effective_at, cutover
		FROM audit_partition_meta`).
		Scan(&st.Policy.Days, &st.Policy.EffectiveDays, &st.Policy.PendingDays, &st.Policy.PendingEffectiveAt, &st.Cutover); err != nil {
		return AuditRetentionStatus{}, fmt.Errorf("store: read audit retention policy: %w", err)
	}
	if err := tx.QueryRow(ctx, partitionsAheadSQL).Scan(&st.MonthsAhead); err != nil {
		return AuditRetentionStatus{}, fmt.Errorf("store: read audit partitions ahead: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT part_name, part_lo, part_hi, part_rows, part_state, part_eligible, COALESCE(part_refusal, '')
		FROM audit_retention_partitions(NULL, true)`)
	if err != nil {
		return AuditRetentionStatus{}, fmt.Errorf("store: read audit partitions: %w", err)
	}
	defer rows.Close()
	st.Partitions = []AuditRetentionPartition{}
	for rows.Next() {
		var p AuditRetentionPartition
		if err := rows.Scan(&p.Name, &p.Lo, &p.Hi, &p.Rows, &p.State, &p.Eligible, &p.Refusal); err != nil {
			return AuditRetentionStatus{}, fmt.Errorf("store: scan audit partition: %w", err)
		}
		st.Partitions = append(st.Partitions, p)
	}
	if err := rows.Err(); err != nil {
		return AuditRetentionStatus{}, fmt.Errorf("store: iterate audit partitions: %w", err)
	}
	return st, nil
}
