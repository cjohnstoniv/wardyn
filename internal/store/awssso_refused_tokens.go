// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AWSSSORefusedTokenStore shares the AWS SSO refused-renewal mark across replicas and restarts
// (internal/api/awssso_refused.go). A refused mark is cleared when a renewal succeeds, unlike the
// spent mark of AWSSSOSpentTokenStore.
type AWSSSORefusedTokenStore interface {
	// MarkAWSSSOTokenRefused upserts fingerprint's refusal, replacing the detail and the age.
	MarkAWSSSOTokenRefused(ctx context.Context, fingerprint, owner, detail string, markedAt time.Time) error
	// AWSSSOTokenRefused reports the detail AWS last refused fingerprint with; ok is false with no mark.
	AWSSSOTokenRefused(ctx context.Context, fingerprint string) (detail string, ok bool, err error)
	// ClearAWSSSOTokenRefused drops fingerprint's mark; none is not an error.
	ClearAWSSSOTokenRefused(ctx context.Context, fingerprint string) error
	// PruneAWSSSORefusedTokens deletes marks older than cutoff, reporting the count removed.
	PruneAWSSSORefusedTokens(ctx context.Context, cutoff time.Time) (int, error)
}

var _ AWSSSORefusedTokenStore = PG{}

// MarkAWSSSOTokenRefused implements AWSSSORefusedTokenStore.
func (s PG) MarkAWSSSOTokenRefused(ctx context.Context, fingerprint, owner, detail string, markedAt time.Time) error {
	const q = `INSERT INTO aws_sso_refused_tokens (fingerprint, owner, detail, marked_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (fingerprint) DO UPDATE SET owner = EXCLUDED.owner, detail = EXCLUDED.detail, marked_at = EXCLUDED.marked_at`
	if _, err := s.Pool.Exec(ctx, q, fingerprint, owner, detail, markedAt); err != nil {
		return fmt.Errorf("store: mark aws sso token refused: %w", err)
	}
	return nil
}

// AWSSSOTokenRefused implements AWSSSORefusedTokenStore.
func (s PG) AWSSSOTokenRefused(ctx context.Context, fingerprint string) (string, bool, error) {
	var detail string
	err := s.Pool.QueryRow(ctx, `SELECT detail FROM aws_sso_refused_tokens WHERE fingerprint = $1`, fingerprint).Scan(&detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read aws sso token refused: %w", err)
	}
	return detail, true, nil
}

// ClearAWSSSOTokenRefused implements AWSSSORefusedTokenStore.
func (s PG) ClearAWSSSOTokenRefused(ctx context.Context, fingerprint string) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM aws_sso_refused_tokens WHERE fingerprint = $1`, fingerprint); err != nil {
		return fmt.Errorf("store: clear aws sso token refused: %w", err)
	}
	return nil
}

// PruneAWSSSORefusedTokens implements AWSSSORefusedTokenStore
// (aws_sso_refused_tokens_marked_at_idx backs the WHERE clause).
func (s PG) PruneAWSSSORefusedTokens(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM aws_sso_refused_tokens WHERE marked_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune aws sso refused tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
