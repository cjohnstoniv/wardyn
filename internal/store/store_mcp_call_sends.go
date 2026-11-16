// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A keyed MCP call's send record (migration 0288_mcp_call_sends).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MCPCallSend is one keyed MCP call a run proxy sent, or is about to send: at most one per operation key
// in its namespace (Owner and OperationScope).
type MCPCallSend struct {
	Owner          string
	OperationScope string
	OperationKey   string
	ApprovalID     uuid.UUID
	RunID          uuid.UUID
	CallSHA256     string
	SentAt         time.Time  // the database's time of the record, written before the send
	AnsweredAt     *time.Time // when the proxy reported the server's answer; nil until then
}

// MCPCallSendStore keeps the send records. Like PushPathListStore it is not part of Store, so the api
// test doubles need not grow it; the api type-asserts it. A send answers for its key only while
// types.MCPCallSendLive holds on the database's clock (DatabaseNow); a clock that cannot be read fails
// the call.
type MCPCallSendStore interface {
	// RecordMCPCallSend writes rec's send at the database's time unless its key already has a send in its
	// namespace that still answers for it, and returns the row that holds the key: rec's own with made
	// true, else the earlier one, unchanged. A send that no longer answers is overwritten by rec's,
	// unanswered.
	RecordMCPCallSend(ctx context.Context, rec MCPCallSend) (got MCPCallSend, made bool, err error)
	// GetMCPCallSend returns the send of key in owner's scope while it still answers for the key, or
	// ErrNotFound.
	GetMCPCallSend(ctx context.Context, owner, scope, key string) (MCPCallSend, error)
	// MarkMCPCallAnswered stamps answered_at on approvalID's send for runID, once; found is false when
	// that approval recorded no send for that run, or its send was overwritten.
	MarkMCPCallAnswered(ctx context.Context, approvalID, runID uuid.UUID) (found bool, err error)
}

var _ MCPCallSendStore = PG{}

func (s PG) RecordMCPCallSend(ctx context.Context, rec MCPCallSend) (MCPCallSend, bool, error) {
	now, err := s.DatabaseNow(ctx)
	if err != nil {
		return MCPCallSend{}, false, err
	}
	// One statement: the key's first record inserts, and a later one overwrites the row only once its
	// send no longer answers (this WHERE is types.MCPCallSendLive negated). Of two records racing for a
	// key, the second waits for the first to commit, re-checks the row the first wrote, and changes
	// nothing; the read below then sees the first.
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO mcp_call_sends (owner, operation_scope, operation_key, approval_id, run_id, call_sha256, sent_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owner, operation_scope, operation_key) DO UPDATE
			SET approval_id = EXCLUDED.approval_id, run_id = EXCLUDED.run_id, call_sha256 = EXCLUDED.call_sha256,
				sent_at = EXCLUDED.sent_at, answered_at = NULL
			WHERE mcp_call_sends.sent_at <= $7::timestamptz - make_interval(secs => $8)
		RETURNING sent_at`,
		rec.Owner, rec.OperationScope, rec.OperationKey, rec.ApprovalID, rec.RunID, rec.CallSHA256, now,
		types.MCPCallSendWindow.Seconds()).Scan(&rec.SentAt)
	if err == nil {
		return rec, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MCPCallSend{}, false, err
	}
	got, err := s.readMCPCallSend(ctx, rec.Owner, rec.OperationScope, rec.OperationKey)
	return got, false, err
}

func (s PG) GetMCPCallSend(ctx context.Context, owner, scope, key string) (MCPCallSend, error) {
	now, err := s.DatabaseNow(ctx)
	if err != nil {
		return MCPCallSend{}, err
	}
	got, err := s.readMCPCallSend(ctx, owner, scope, key)
	if err == nil && !types.MCPCallSendLive(got.SentAt, now) {
		return MCPCallSend{}, ErrNotFound
	}
	return got, err
}

// readMCPCallSend reads the send of key in owner's scope as it is, whether or not it still answers, or
// ErrNotFound.
func (s PG) readMCPCallSend(ctx context.Context, owner, scope, key string) (MCPCallSend, error) {
	got := MCPCallSend{Owner: owner, OperationScope: scope, OperationKey: key}
	err := s.Pool.QueryRow(ctx, `
		SELECT approval_id, run_id, call_sha256, sent_at, answered_at FROM mcp_call_sends
		WHERE owner = $1 AND operation_scope = $2 AND operation_key = $3`,
		owner, scope, key).Scan(&got.ApprovalID, &got.RunID, &got.CallSHA256, &got.SentAt, &got.AnsweredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPCallSend{}, ErrNotFound
	}
	return got, err
}

func (s PG) MarkMCPCallAnswered(ctx context.Context, approvalID, runID uuid.UUID) (bool, error) {
	var found bool
	err := s.Pool.QueryRow(ctx, `
		WITH stamped AS (
			UPDATE mcp_call_sends SET answered_at = clock_timestamp()
			WHERE approval_id = $1 AND run_id = $2 AND answered_at IS NULL
		)
		SELECT EXISTS (SELECT 1 FROM mcp_call_sends WHERE approval_id = $1 AND run_id = $2)`,
		approvalID, runID).Scan(&found)
	return found, err
}
