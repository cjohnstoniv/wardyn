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
)

// AuditChainRange is the verdict on one desktop recording's stretch of the audit chain
// (GET /api/v1/runs/{id}/recording-chain): from the run's first row at or after the recording
// started to the recording's own desktop.recording.write row. Every row in that stretch is
// checked, other runs' rows included, because the chain is one chain.
type AuditChainRange struct {
	OK bool `json:"ok"`
	// FirstSeq/LastSeq bound the range; Checked is how many rows it holds.
	FirstSeq int64 `json:"first_seq"`
	LastSeq  int64 `json:"last_seq"`
	Checked  int64 `json:"checked"`
	// BrokenSeq/Reason name the first row that failed; set only when OK is false.
	BrokenSeq int64  `json:"broken_seq,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Marks are the run's desktop.recording rows in the range, so the player can name a gap's entries.
	Marks []AuditChainMark `json:"marks"`
}

// AuditChainMark is one audit row's place in the chain.
type AuditChainMark struct {
	ID  uuid.UUID `json:"id"`
	Seq int64     `json:"seq"`
}

// AuditChainRangeVerifier is the optional store capability behind GET /runs/{id}/recording-chain,
// as AuditChainVerifier is for the whole chain: a store with no chain answers 501.
type AuditChainRangeVerifier interface {
	// VerifyRecordingAuditRange reports found=false when the run has no desktop.recording.write row
	// for key.
	VerifyRecordingAuditRange(ctx context.Context, runID uuid.UUID, key string, from time.Time) (rng AuditChainRange, found bool, err error)
}

var _ AuditChainRangeVerifier = PG{}

// VerifyRecordingAuditRange walks the chain over the recording's range with the rules
// VerifyAuditChain uses (auditChainWalk), starting from the row just before the range, so a row
// removed at the range's edge breaks the first link as one removed inside it would. With no row
// before the range (retention dropped it), the first link is the range's own and only the whole
// sweep can check it. One repeatable-read snapshot, paged like the whole sweep.
func (s PG) VerifyRecordingAuditRange(ctx context.Context, runID uuid.UUID, key string, from time.Time) (AuditChainRange, bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AuditChainRange{}, false, fmt.Errorf("store: begin audit range check: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only: nothing to undo

	var lo, hi int64
	err = tx.QueryRow(ctx, `
		SELECT w.seq, COALESCE((SELECT min(seq) FROM audit_events
		                         WHERE run_id = $1 AND "time" >= $3 AND seq <= w.seq), w.seq)
		  FROM audit_events w
		 WHERE w.run_id = $1 AND w.action = 'desktop.recording.write' AND w.outcome = 'success' AND w.target = $2
		 ORDER BY w.seq LIMIT 1`, runID, key, from).Scan(&hi, &lo)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuditChainRange{}, false, nil
	}
	if err != nil {
		return AuditChainRange{}, false, fmt.Errorf("store: read recording audit range: %w", err)
	}

	rng := AuditChainRange{FirstSeq: lo, LastSeq: hi, Marks: []AuditChainMark{}}
	w := newAuditChainWalk()
	w.ranged = true
	// The row before the range is read for its hash only: it is the first link's other end.
	var predHash string
	err = tx.QueryRow(ctx, `SELECT COALESCE(row_hash, '') FROM audit_events WHERE seq < $1 ORDER BY seq DESC LIMIT 1`, lo).Scan(&predHash)
	hasPred := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AuditChainRange{}, false, fmt.Errorf("store: read the row before the range: %w", err)
	}
	if hasPred {
		w.prev, w.anchored = predHash, predHash != ""
	}

	const q = `
		SELECT seq,
		       row_hash IS NULL,
		       prev_hash IS NULL,
		       COALESCE(prev_hash,''),
		       COALESCE(row_hash,''),
		       COALESCE(audit_row_hash(prev_hash, id, time, run_id, actor_type, actor,
		                               action, target, outcome, source_ip, data), ''),
		       id,
		       run_id IS NOT DISTINCT FROM $4 AND action LIKE 'desktop.recording.%'
		FROM audit_events
		WHERE seq > $1 AND seq <= $3
		ORDER BY seq
		LIMIT $2`
	after := lo - 1
	for {
		rows, err := tx.Query(ctx, q, after, AuditChainPageSize, hi, runID)
		if err != nil {
			return AuditChainRange{}, false, fmt.Errorf("store: read audit range: %w", err)
		}
		n, broke := 0, false
		for rows.Next() {
			var l auditChainLink
			var id uuid.UUID
			var mark bool
			if err := rows.Scan(&l.seq, &l.unchained, &l.prevIsNull, &l.prev, &l.row, &l.want, &id, &mark); err != nil {
				rows.Close()
				return AuditChainRange{}, false, fmt.Errorf("store: scan audit range row: %w", err)
			}
			n++
			after = l.seq
			if !hasPred && w.st.Checked == 0 && !l.unchained {
				w.prev, w.anchored = l.prev, l.prev != ""
			}
			if mark {
				rng.Marks = append(rng.Marks, AuditChainMark{ID: id, Seq: l.seq})
			}
			if !w.step(l) {
				broke = true
				break
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil && w.st.OK {
			return AuditChainRange{}, false, fmt.Errorf("store: iterate audit range: %w", err)
		}
		if broke || int32(n) < AuditChainPageSize {
			break
		}
		if err := ctx.Err(); err != nil {
			return AuditChainRange{}, false, fmt.Errorf("store: audit range check cancelled: %w", err)
		}
	}
	rng.OK, rng.Checked, rng.BrokenSeq, rng.Reason = w.st.OK, w.st.Checked+w.st.Legacy, w.st.BrokenSeq, w.st.Reason
	return rng, true, nil
}
