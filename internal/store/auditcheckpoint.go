// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AuditCheckpointMaxRows caps the rows one signed checkpoint covers, so a receipt's hash list stays bounded
// (10,000 hashes, about 670 KB). A burst larger than this is several checkpoints.
const AuditCheckpointMaxRows = 10000

// AuditCheckpoint is one stored signed checkpoint: the exact statement bytes that were signed, the Ed25519
// signature over them, and the signing key's id. The statement's own fields are read with
// auditreceipt.ParseStatement; the columns here are what the table indexes.
type AuditCheckpoint struct {
	SeqLo     int64
	SeqHi     int64
	Kid       string
	Statement string
	Signature []byte
	CreatedAt time.Time
}

// CheckpointRow is one row a checkpoint covers: its position and its stored row_hash, re-hashed and
// link-checked before it was returned.
type CheckpointRow struct {
	Seq     int64
	RowHash string
}

// AuditCheckpointBreak is AuditCheckpointRange's refusal: the chain is broken at Seq, so nothing in the range
// may be signed. The signer stops for the life of the process.
type AuditCheckpointBreak struct {
	Seq    int64
	Reason string
}

func (e *AuditCheckpointBreak) Error() string {
	return fmt.Sprintf("store: the audit chain is broken at seq %d: %s", e.Seq, e.Reason)
}

// ErrAuditCheckpointExists is InsertAuditCheckpoint's answer when a stored checkpoint already covers any seq in
// the new range.
var ErrAuditCheckpointExists = errors.New("store: an audit checkpoint already covers that range")

// AuditCheckpointer is the optional store capability behind signed audit checkpoints and receipts. A test fake
// or non-Postgres store has no chain to sign.
type AuditCheckpointer interface {
	AuditChainHead(ctx context.Context) (seq int64, rowHash string, err error)
	AuditCheckpointRange(ctx context.Context, afterSeq int64, afterHash string, max int) (rows []CheckpointRow, highWater int64, err error)
	InsertAuditCheckpoint(ctx context.Context, cp AuditCheckpoint) (existing AuditCheckpoint, err error)
	LatestAuditCheckpoint(ctx context.Context) (cp AuditCheckpoint, found bool, err error)
	AuditCheckpoints(ctx context.Context, afterSeq int64, limit int) ([]AuditCheckpoint, error)
	AuditCheckpointRowHashes(ctx context.Context, seqLo, seqHi int64) ([]CheckpointRow, error)
	AuditReceiptRows(ctx context.Context, ids []uuid.UUID, runID *uuid.UUID, afterSeq int64, limit int) ([]AuditPartitionRow, error)
}

var _ AuditCheckpointer = PG{}

// AuditCheckpointRange reads the rows a new checkpoint covers: seq above afterSeq, up to the chain's
// high-water mark, at most max of them, oldest first. Every row is re-hashed with audit_row_hash and walked
// with the sweep's rules (auditChainWalk), seeded with afterHash, the previous checkpoint's head_hash (or, for
// the first checkpoint, the head the checkpoints started after; "" only when the chain had no row). So the
// first row must chain to that head, and a hashless row anywhere in the range is a break, never legacy.
//
// One removal is accepted, the one the sweep accepts: when the newest attested retention drop removed rows
// above afterSeq (signing was behind it, so they were never signed), the first row must chain to that drop's
// recorded tail instead. A drop that ended at or below afterSeq changes nothing, and any other gap is a break.
//
// One repeatable-read, read-only snapshot: audit_append advances hw_seq under the chain lock with the insert,
// so seq order is commit order and every row at or below the high-water mark read here is already visible.
// On a break it returns *AuditCheckpointBreak and no rows. highWater is returned with the rows so the caller
// can report how far behind signing is.
func (s PG) AuditCheckpointRange(ctx context.Context, afterSeq int64, afterHash string, max int) ([]CheckpointRow, int64, error) {
	if max < 1 || max > AuditCheckpointMaxRows {
		return nil, 0, fmt.Errorf("store: audit checkpoint range of %d rows (want 1 to %d)", max, AuditCheckpointMaxRows)
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, fmt.Errorf("store: begin audit checkpoint read: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only: nothing to undo

	var hw int64
	if err := tx.QueryRow(ctx, `SELECT hw_seq FROM audit_partition_meta`).Scan(&hw); err != nil {
		return nil, 0, fmt.Errorf("store: read audit high-water mark: %w", err)
	}
	tail, dropSeq, err := newestDropAnchor(ctx, tx, true)
	if err != nil {
		return nil, 0, err
	}
	prev, ranged := checkpointRangeSeed(afterSeq, afterHash, dropSeq, tail)

	// The column list is the sweep's (VerifyAuditChain), so the walk sees what the sweep sees.
	rows, err := tx.Query(ctx, `
		SELECT seq,
		       row_hash IS NULL,
		       prev_hash IS NULL,
		       COALESCE(prev_hash,''),
		       COALESCE(row_hash,''),
		       COALESCE(audit_row_hash(prev_hash, id, time, run_id, actor_type, actor,
		                               action, target, outcome, source_ip, data), '')
		FROM audit_events
		WHERE seq > $1 AND seq <= $2
		ORDER BY seq
		LIMIT $3`, afterSeq, hw, max)
	if err != nil {
		return nil, 0, fmt.Errorf("store: read audit checkpoint range: %w", err)
	}
	defer rows.Close()

	// anchored: a hashless row is a break even as the first one read; ranged: prev began as the row before the
	// range, not a retention drop's tail.
	w := &auditChainWalk{st: AuditChainStatus{OK: true}, prev: prev, anchored: true, ranged: ranged}
	var out []CheckpointRow
	for rows.Next() {
		var l auditChainLink
		if err := rows.Scan(&l.seq, &l.unchained, &l.prevIsNull, &l.prev, &l.row, &l.want); err != nil {
			return nil, 0, fmt.Errorf("store: scan audit checkpoint row: %w", err)
		}
		if !w.step(l) {
			return nil, 0, &AuditCheckpointBreak{Seq: w.st.BrokenSeq, Reason: w.st.Reason}
		}
		out = append(out, CheckpointRow{Seq: l.seq, RowHash: l.row})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate audit checkpoint range: %w", err)
	}
	return out, hw, nil
}

// checkpointRangeSeed is the hash the range's first row must chain to, and whether the walk is ranged: the
// recorded tail of the newest drop with rows when that drop removed rows above afterSeq, else afterHash. A
// drop whose tail is "" (a hashless range) leaves nothing to chain to, so the head's hash still counts.
func checkpointRangeSeed(afterSeq int64, afterHash string, dropSeq int64, tail string) (prev string, ranged bool) {
	if dropSeq > afterSeq && tail != "" {
		return tail, false
	}
	return afterHash, true
}

// AuditChainHead is the chain's high-water mark: the seq and row_hash of the newest row ever appended ("" while
// the log holds no hashed row). The first checkpoint starts after it.
func (s PG) AuditChainHead(ctx context.Context) (int64, string, error) {
	var seq int64
	var hash string
	if err := s.Pool.QueryRow(ctx, `SELECT hw_seq, COALESCE(hw_row_hash, '') FROM audit_partition_meta`).Scan(&seq, &hash); err != nil {
		return 0, "", fmt.Errorf("store: read audit high-water mark: %w", err)
	}
	return seq, hash, nil
}

const auditCheckpointCols = `seq_lo, seq_hi, kid, statement, signature, created_at`

func scanAuditCheckpoint(r pgx.Row) (AuditCheckpoint, error) {
	var c AuditCheckpoint
	err := r.Scan(&c.SeqLo, &c.SeqHi, &c.Kid, &c.Statement, &c.Signature, &c.CreatedAt)
	return c, err
}

// InsertAuditCheckpoint stores one signed checkpoint. When a stored checkpoint's range overlaps its range (the
// table's exclusion constraint), it writes nothing and returns the first such stored row with
// ErrAuditCheckpointExists: the caller checks the stored row's signature, because a row a database actor
// inserted carries no valid one. The conflict needs no UPDATE grant, which the app role does not hold.
func (s PG) InsertAuditCheckpoint(ctx context.Context, cp AuditCheckpoint) (AuditCheckpoint, error) {
	var seqLo int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO audit_checkpoints (seq_lo, seq_hi, kid, statement, signature)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
		RETURNING seq_lo`, cp.SeqLo, cp.SeqHi, cp.Kid, cp.Statement, cp.Signature).Scan(&seqLo)
	if err == nil {
		return AuditCheckpoint{}, nil
	}
	// DO NOTHING answers a unique or exclusion conflict with no row; an exclusion violation (23P01) raised
	// anyway takes the same path.
	var pgErr *pgconn.PgError
	if !errors.Is(err, pgx.ErrNoRows) && !(errors.As(err, &pgErr) && pgErr.Code == "23P01") {
		return AuditCheckpoint{}, fmt.Errorf("store: insert audit checkpoint: %w", err)
	}
	existing, err := scanAuditCheckpoint(s.Pool.QueryRow(ctx, `SELECT `+auditCheckpointCols+` FROM audit_checkpoints
		WHERE int8range(seq_lo, seq_hi, '[]') && int8range($1, $2, '[]') ORDER BY seq_lo LIMIT 1`, cp.SeqLo, cp.SeqHi))
	if err != nil {
		return AuditCheckpoint{}, fmt.Errorf("store: read the conflicting audit checkpoint: %w", err)
	}
	return existing, ErrAuditCheckpointExists
}

// LatestAuditCheckpoint is the checkpoint with the highest seq_hi, the one the next checkpoint chains to.
func (s PG) LatestAuditCheckpoint(ctx context.Context) (AuditCheckpoint, bool, error) {
	c, err := scanAuditCheckpoint(s.Pool.QueryRow(ctx, `SELECT `+auditCheckpointCols+` FROM audit_checkpoints
		ORDER BY seq_hi DESC LIMIT 1`))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return AuditCheckpoint{}, false, nil
	case err != nil:
		return AuditCheckpoint{}, false, fmt.Errorf("store: read the latest audit checkpoint: %w", err)
	}
	return c, true, nil
}

// AuditCheckpoints pages the checkpoints oldest first: those covering any row above afterSeq (seq_hi >
// afterSeq), at most limit. Paging by the last page's seq_hi never skips or repeats one.
func (s PG) AuditCheckpoints(ctx context.Context, afterSeq int64, limit int) ([]AuditCheckpoint, error) {
	return collect(ctx, s.Pool, "list", "audit checkpoints", `SELECT `+auditCheckpointCols+` FROM audit_checkpoints
		WHERE seq_hi > $1 ORDER BY seq_lo LIMIT $2`, []any{afterSeq, limit}, scanAuditCheckpoint)
}

// AuditCheckpointRowHashes is a checkpoint's hash list with its seqs: the seq and stored row_hash of every row
// from seqLo to seqHi, in seq order. A receipt carries both, because the signed seq_digest binds each seq to
// its row and a row's hash does not cover its seq. A hashless row in the range reads as "", which no fold
// accepts.
func (s PG) AuditCheckpointRowHashes(ctx context.Context, seqLo, seqHi int64) ([]CheckpointRow, error) {
	return collect(ctx, s.Pool, "read", "audit checkpoint row hashes", `SELECT seq, COALESCE(row_hash, '') FROM audit_events
		WHERE seq >= $1 AND seq <= $2 ORDER BY seq`, []any{seqLo, seqHi}, func(r pgx.Row) (CheckpointRow, error) {
		var c CheckpointRow
		err := r.Scan(&c.Seq, &c.RowHash)
		return c, err
	})
}

// AuditReceiptRows reads the rows a receipt is for, exactly as stored (sealed fields stay ciphertext, which is
// what was hashed): the rows with the given ids, or with ids empty every row of runID, above afterSeq, oldest
// first, at most limit. Ask for one more than a cap to learn the cap was passed.
func (s PG) AuditReceiptRows(ctx context.Context, ids []uuid.UUID, runID *uuid.UUID, afterSeq int64, limit int) ([]AuditPartitionRow, error) {
	if len(ids) == 0 && runID == nil {
		return []AuditPartitionRow{}, nil
	}
	where, arg := `id = ANY($1)`, any(ids)
	if len(ids) == 0 {
		where, arg = `run_id = $1`, *runID
	}
	return collect(ctx, s.Pool, "read", "audit receipt rows", `
		SELECT seq, (extract(epoch FROM recorded_at) * 1000000)::bigint,
		       (extract(epoch FROM time) * 1000000)::bigint,
		       id, run_id, actor_type, actor, action, target, outcome, source_ip, data::text,
		       prev_hash, row_hash,
		       COALESCE(row_hash, audit_row_hash(NULL, id, time, run_id, actor_type, actor, action, target,
		                                         outcome, source_ip, data))
		  FROM audit_events
		 WHERE `+where+` AND seq > $2
		 ORDER BY seq
		 LIMIT $3`, []any{arg, afterSeq, limit}, func(row pgx.Row) (AuditPartitionRow, error) {
		var r AuditPartitionRow
		err := row.Scan(&r.Seq, &r.RecordedUS, &r.TimeUS, &r.ID, &r.RunID, &r.ActorType, &r.Actor, &r.Action,
			&r.Target, &r.Outcome, &r.SourceIP, &r.Data, &r.PrevHash, &r.RowHash, &r.FoldHash)
		return r, err
	})
}
