// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrAuditPartitionOpen is a partition that can still receive rows: it has no digest yet.
var ErrAuditPartitionOpen = errors.New("store: audit partition is still open")

// PartitionManifest is what a partition's digest binds besides its rows: its name, the range of seq and
// recorded_at it actually holds (recorded_at as integer microseconds since the epoch, the unit
// audit_row_hash uses) and its row count. An empty partition has Rows 0 and every range zero.
//
// Header is the canonical text the digest starts from. It is the migration 0119 definition, which
// audit_partition_digest(p_partition) implements in the database: the two are pinned together by a test.
type PartitionManifest struct {
	Partition    string `json:"partition"`
	Rows         int64  `json:"row_count"`
	SeqLo        int64  `json:"seq_lo"`
	SeqHi        int64  `json:"seq_hi"`
	RecordedLoUS int64  `json:"recorded_lo_us"`
	RecordedHiUS int64  `json:"recorded_hi_us"`
}

// Header is the JSON array of strings the fold starts from, in jsonb's own text form (a comma and a
// space between elements), so a manifest the database and Go each render is byte-identical. A range of
// an empty partition renders as an empty string.
func (m PartitionManifest) Header() string {
	rng := func(v int64) string {
		if m.Rows == 0 {
			return ""
		}
		return strconv.FormatInt(v, 10)
	}
	parts := []string{m.Partition, rng(m.SeqLo), rng(m.SeqHi), rng(m.RecordedLoUS), rng(m.RecordedHiUS), strconv.FormatInt(m.Rows, 10)}
	for i, p := range parts {
		b, _ := json.Marshal(p) // a string never fails to marshal
		parts[i] = string(b)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// PartitionDigest is the streaming fold of a closed partition, in seq order:
//
//	d_0 = sha256(Header)            d_i = sha256(d_{i-1} || row_hash_i)
//
// every value lowercase hex text. It holds one 64-byte state however many rows are added, so a
// partition larger than Postgres's 1 GB field limit digests in constant memory.
type PartitionDigest struct {
	d   [sha256.Size * 2]byte
	buf []byte
}

// NewPartitionDigest starts the fold at the manifest's header.
func NewPartitionDigest(m PartitionManifest) *PartitionDigest {
	p := &PartitionDigest{}
	sum := sha256.Sum256([]byte(m.Header()))
	hex.Encode(p.d[:], sum[:])
	return p
}

// Add folds one row's hash (the stored row_hash; a row from before the chain began folds the hash
// audit_row_hash(NULL, ...) would have given it) into the digest.
func (p *PartitionDigest) Add(rowHash string) {
	p.buf = append(append(p.buf[:0], p.d[:]...), rowHash...)
	sum := sha256.Sum256(p.buf)
	hex.Encode(p.d[:], sum[:])
}

// Sum is the digest so far, as 64 hex characters.
func (p *PartitionDigest) Sum() string { return string(p.d[:]) }

// AuditPartitionRow is one stored row of a partition, exactly as the table holds it: the columns
// audit_row_hash covers, the hashes, and the position.
type AuditPartitionRow struct {
	Seq        int64
	RecordedUS int64
	TimeUS     int64
	ID         uuid.UUID
	RunID      *uuid.UUID
	ActorType  string
	Actor      string
	Action     string
	Target     string
	Outcome    string
	SourceIP   string
	// Data is the jsonb's own text form, the exact bytes audit_row_hash embeds; nil for SQL NULL.
	Data *string
	// PrevHash and RowHash are nil for a row from before the chain began.
	PrevHash *string
	RowHash  *string
	// FoldHash is what the digest folds: RowHash, or for a hashless row the hash it would have had.
	FoldHash string
}

// AuditPartitionExporter is the OPTIONAL store capability behind GET /audit/export?partition=. A test
// fake or non-Postgres store has no partitions.
type AuditPartitionExporter interface {
	// ExportAuditPartition reads one closed partition in seq order from ONE snapshot. header is called
	// once, before any row, with the manifest of that snapshot; row once per row. ErrNotFound when name
	// is not a partition of the audit log, ErrAuditPartitionOpen when it can still receive rows; in
	// both cases neither callback ran. An error from either callback stops the read and is returned.
	ExportAuditPartition(ctx context.Context, name string, header func(PartitionManifest) error, row func(AuditPartitionRow) error) error
}

var _ AuditPartitionExporter = PG{}

// partitionClosedSQL finds the named partition among the children of audit_events and says whether the
// chain's high-water recorded_at has reached its upper bound: the same rule audit_partition_digest
// applies. The name is a bind parameter, matched against the catalog; it is never spliced into SQL
// until it has been found there.
const partitionClosedSQL = `
	SELECT n.nspname::text, c.relname::text,
	       COALESCE((regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz
	                <= m.hw_recorded_at, false)
	  FROM pg_inherits i
	  JOIN pg_class c ON c.oid = i.inhrelid
	  JOIN pg_namespace n ON n.oid = c.relnamespace
	  CROSS JOIN audit_partition_meta m
	 WHERE i.inhparent = 'audit_events'::regclass AND c.relname = $1`

func (s PG) ExportAuditPartition(ctx context.Context, name string, header func(PartitionManifest) error, row func(AuditPartitionRow) error) error {
	// One snapshot for the manifest and the rows, so the header can never disagree with the stream.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("store: begin audit partition export: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only: nothing to undo

	var schema, rel string
	var closed bool
	switch err := tx.QueryRow(ctx, partitionClosedSQL, name).Scan(&schema, &rel, &closed); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("store: look up audit partition: %w", err)
	}
	if !closed {
		return ErrAuditPartitionOpen
	}
	table := pgx.Identifier{schema, rel}.Sanitize()

	m := PartitionManifest{Partition: rel}
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(min(seq), 0), COALESCE(max(seq), 0),
			COALESCE((extract(epoch FROM min(recorded_at)) * 1000000)::bigint, 0),
			COALESCE((extract(epoch FROM max(recorded_at)) * 1000000)::bigint, 0) FROM `+table).
		Scan(&m.Rows, &m.SeqLo, &m.SeqHi, &m.RecordedLoUS, &m.RecordedHiUS); err != nil {
		return fmt.Errorf("store: read audit partition manifest: %w", err)
	}
	if err := header(m); err != nil {
		return err
	}

	rows, err := tx.Query(ctx, `
		SELECT seq, (extract(epoch FROM recorded_at) * 1000000)::bigint,
		       (extract(epoch FROM time) * 1000000)::bigint,
		       id, run_id, actor_type, actor, action, target, outcome, source_ip, data::text,
		       prev_hash, row_hash,
		       COALESCE(row_hash, audit_row_hash(NULL, id, time, run_id, actor_type, actor, action, target,
		                                         outcome, source_ip, data))
		  FROM `+table+` ORDER BY seq`)
	if err != nil {
		return fmt.Errorf("store: read audit partition: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r AuditPartitionRow
		if err := rows.Scan(&r.Seq, &r.RecordedUS, &r.TimeUS, &r.ID, &r.RunID, &r.ActorType, &r.Actor, &r.Action,
			&r.Target, &r.Outcome, &r.SourceIP, &r.Data, &r.PrevHash, &r.RowHash, &r.FoldHash); err != nil {
			return fmt.Errorf("store: scan audit partition row: %w", err)
		}
		if err := row(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate audit partition: %w", err)
	}
	return nil
}
