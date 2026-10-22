// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ApprovalRuleStore holds approval rules (migration 0224). A side interface, as BoundaryStore is: a store
// without it keeps no rules, so the approver has nothing to match.
type ApprovalRuleStore interface {
	ListApprovalRules(ctx context.Context) ([]types.ApprovalRule, error)
	GetApprovalRule(ctx context.Context, name string) (types.ApprovalRule, error)
	PutApprovalRule(ctx context.Context, w types.ApprovalRuleWrite, by string) (types.ApprovalRule, error)
	DeleteApprovalRule(ctx context.Context, name string) (types.ApprovalRule, error)
}

var _ ApprovalRuleStore = PG{}

const approvalRuleCols = `id, name, version, mode, spec, created_by, created_at, updated_by, updated_at`

func scanApprovalRule(row pgx.Row) (types.ApprovalRule, error) {
	var r types.ApprovalRule
	var spec []byte
	if err := row.Scan(&r.ID, &r.Name, &r.Version, &r.Mode, &spec, &r.CreatedBy, &r.CreatedAt, &r.UpdatedBy, &r.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return types.ApprovalRule{}, ErrNotFound
		}
		return types.ApprovalRule{}, err
	}
	if err := json.Unmarshal(spec, &r.ApprovalRuleSpec); err != nil {
		return types.ApprovalRule{}, fmt.Errorf("store: unmarshal approval rule spec: %w", err)
	}
	return r, nil
}

// ListApprovalRules returns every rule by name: the order the approver takes the first match in.
func (s PG) ListApprovalRules(ctx context.Context) ([]types.ApprovalRule, error) {
	return collect(ctx, s.Pool, "list", "approval rules", `SELECT `+approvalRuleCols+` FROM approval_rules ORDER BY name`, nil, scanApprovalRule)
}

// GetApprovalRule returns the rule named name, or ErrNotFound.
func (s PG) GetApprovalRule(ctx context.Context, name string) (types.ApprovalRule, error) {
	return GetApprovalRuleQ(ctx, s.Pool, name)
}

// PutApprovalRule creates the rule at version 1 or replaces it at its version plus one.
func (s PG) PutApprovalRule(ctx context.Context, w types.ApprovalRuleWrite, by string) (types.ApprovalRule, error) {
	return PutApprovalRuleQ(ctx, s.Pool, w, by)
}

// DeleteApprovalRule removes the rule and returns it as it was, or ErrNotFound.
func (s PG) DeleteApprovalRule(ctx context.Context, name string) (types.ApprovalRule, error) {
	r, err := scanApprovalRule(s.Pool.QueryRow(ctx, `DELETE FROM approval_rules WHERE name = $1 RETURNING `+approvalRuleCols, name))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return types.ApprovalRule{}, fmt.Errorf("store: delete approval rule: %w", err)
	}
	return r, err
}

// GetApprovalRuleQ is GetApprovalRule on q.
func GetApprovalRuleQ(ctx context.Context, q Querier, name string) (types.ApprovalRule, error) {
	r, err := scanApprovalRule(q.QueryRow(ctx, `SELECT `+approvalRuleCols+` FROM approval_rules WHERE name = $1`, name))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return types.ApprovalRule{}, fmt.Errorf("store: get approval rule: %w", err)
	}
	return r, err
}

// PutApprovalRuleQ is PutApprovalRule on q.
func PutApprovalRuleQ(ctx context.Context, q Querier, w types.ApprovalRuleWrite, by string) (types.ApprovalRule, error) {
	spec, err := json.Marshal(w.ApprovalRuleSpec)
	if err != nil {
		return types.ApprovalRule{}, fmt.Errorf("store: marshal approval rule spec: %w", err)
	}
	r, err := scanApprovalRule(q.QueryRow(ctx, `
		INSERT INTO approval_rules (id, name, version, mode, spec, created_by, updated_by)
		VALUES ($1, $2, 1, $3, $4, $5, $5)
		ON CONFLICT (name) DO UPDATE
			SET version = approval_rules.version + 1, mode = EXCLUDED.mode, spec = EXCLUDED.spec,
			    updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING `+approvalRuleCols, uuid.New(), w.Name, string(w.Mode), spec, by))
	if err != nil {
		return types.ApprovalRule{}, fmt.Errorf("store: put approval rule: %w", err)
	}
	return r, nil
}
