// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// RecordEvidence is what a confined replay's clean verdict is checked against after it is stamped. An audit
// row takes its seq under the chain lock it holds to commit (audit_append), so seq order is commit order:
// every row of a run at or below the position RunAuditSeq returns has committed, and a row that commits
// later sits above it. A capability interface for the reason Pager gives.
type RecordEvidence interface {
	// RunAuditSeq is the highest seq among runID's audit rows, 0 when it has none.
	RunAuditSeq(ctx context.Context, runID uuid.UUID) (int64, error)
	// HasRunAuditEventAfter reports whether runID has an audit row past afterSeq that matches f.
	HasRunAuditEventAfter(ctx context.Context, runID uuid.UUID, afterSeq int64, f AuditFilter) (bool, error)
	// SwapWorkspaceRecordResult replaces one task's record_results entry only while it still equals old,
	// as JSON, and reports whether it did.
	SwapWorkspaceRecordResult(ctx context.Context, id uuid.UUID, taskKey string, old, result json.RawMessage) (bool, error)
}

var _ RecordEvidence = PG{}

// RunAuditSeq is RecordEvidence.RunAuditSeq; audit_events_run_seq_idx answers it.
func (s PG) RunAuditSeq(ctx context.Context, runID uuid.UUID) (int64, error) {
	var seq int64
	if err := s.Pool.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM audit_events WHERE run_id = $1`, runID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: read run audit position: %w", err)
	}
	return seq, nil
}

// HasRunAuditEventAfter is RecordEvidence.HasRunAuditEventAfter, as an EXISTS like HasRunAuditEvent.
func (s PG) HasRunAuditEventAfter(ctx context.Context, runID uuid.UUID, afterSeq int64, f AuditFilter) (bool, error) {
	clauses, args := f.where([]any{runID, afterSeq})
	q := `SELECT EXISTS (SELECT 1 FROM audit_events WHERE ` +
		strings.Join(append([]string{"run_id = $1", "seq > $2"}, clauses...), " AND ") + `)`
	var found bool
	if err := s.Pool.QueryRow(ctx, q, args...).Scan(&found); err != nil {
		return false, fmt.Errorf("store: probe run audit events after a position: %w", err)
	}
	return found, nil
}

// SwapWorkspaceRecordResult is RecordEvidence.SwapWorkspaceRecordResult. jsonb equality ignores key order
// and spacing, so old may be the entry as any earlier read of the row rendered it.
func (s PG) SwapWorkspaceRecordResult(ctx context.Context, id uuid.UUID, taskKey string, old, result json.RawMessage) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE workspaces SET record_results = record_results || jsonb_build_object($2::text, $4::jsonb), updated_at=now()
		 WHERE id=$1 AND record_results -> ($2::text) = $3::jsonb`,
		id, taskKey, string(old), string(result))
	if err != nil {
		return false, fmt.Errorf("store: swap workspace record result: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
