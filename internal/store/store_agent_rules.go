// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrAgentRulesVersion is a write or delete that named a version other than the stored one, or a create
// over a harness that already has rules.
var ErrAgentRulesVersion = errors.New("store: the agent rules are at another version")

// AgentRulesStore persists an organisation's rules for each agent harness (migration 0400_agent_rules). Spec
// and inner walls are validated at the API and stored as given; a stored row that no longer decodes is an
// error, never read as narrower than it is. Bodies in store_agent_rules.go.
type AgentRulesStore interface {
	// ListAgentRules returns every harness's rules, sorted by harness; empty when none.
	ListAgentRules(ctx context.Context) ([]types.AgentRules, error)
	// PutAgentRules writes r (Harness, Spec, InnerWalls, UpdatedBy) and returns the stored row. expected 0
	// creates the harness's rules at version 1; any other value must be the stored version, which the write
	// bumps. ErrAgentRulesVersion otherwise.
	PutAgentRules(ctx context.Context, r types.AgentRules, expected int) (types.AgentRules, error)
	// DeleteAgentRules removes a harness's rules at the stored version and returns the row removed.
	// ErrNotFound when there is none, ErrAgentRulesVersion when the version differs.
	DeleteAgentRules(ctx context.Context, harness string, expected int) (types.AgentRules, error)
}

// Compile-time assertion: PG satisfies AgentRulesStore.
var _ AgentRulesStore = PG{}

const agentRulesCols = `harness, spec, inner_walls, version, updated_by, updated_at`

func scanAgentRules(row pgx.Row) (types.AgentRules, error) {
	var r types.AgentRules
	var spec, inner []byte
	err := row.Scan(&r.Harness, &spec, &inner, &r.Version, &r.UpdatedBy, &r.UpdatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return types.AgentRules{}, ErrNotFound
	case err != nil:
		return types.AgentRules{}, err
	}
	if err := decodeStrictJSON(spec, &r.Spec); err != nil {
		return types.AgentRules{}, fmt.Errorf("store: read agent rules %s: %w", r.Harness, err)
	}
	if inner != nil {
		r.InnerWalls = &types.HarnessInnerWalls{}
		if err := decodeStrictJSON(inner, r.InnerWalls); err != nil {
			return types.AgentRules{}, fmt.Errorf("store: read agent rules %s inner walls: %w", r.Harness, err)
		}
	}
	return r, nil
}

// ListAgentRules — see AgentRulesStore.
func (s PG) ListAgentRules(ctx context.Context) ([]types.AgentRules, error) {
	return collect(ctx, s.Pool, "list", "agent rules", `SELECT `+agentRulesCols+` FROM agent_rules ORDER BY harness`, nil, scanAgentRules)
}

// PutAgentRules — see AgentRulesStore.
func (s PG) PutAgentRules(ctx context.Context, r types.AgentRules, expected int) (types.AgentRules, error) {
	return PutAgentRulesQ(ctx, s.Pool, r, expected)
}

// DeleteAgentRules — see AgentRulesStore.
func (s PG) DeleteAgentRules(ctx context.Context, harness string, expected int) (types.AgentRules, error) {
	return DeleteAgentRulesQ(ctx, s.Pool, harness, expected)
}

// GetAgentRulesQ is one harness's rules on q. ErrNotFound when none.
func GetAgentRulesQ(ctx context.Context, q Querier, harness string) (types.AgentRules, error) {
	return scanAgentRules(q.QueryRow(ctx, `SELECT `+agentRulesCols+` FROM agent_rules WHERE harness = $1`, harness))
}

// PutAgentRulesQ is PutAgentRules on q, for a held change applied inside its decision transaction.
func PutAgentRulesQ(ctx context.Context, q Querier, r types.AgentRules, expected int) (types.AgentRules, error) {
	if r.Harness == "" || r.UpdatedBy == "" || expected < 0 {
		return types.AgentRules{}, errors.New("store: write agent rules: a harness, a writer and a version of 0 or more are required")
	}
	spec, err := json.Marshal(r.Spec)
	if err != nil {
		return types.AgentRules{}, fmt.Errorf("store: write agent rules: %w", err)
	}
	var inner []byte
	if r.InnerWalls != nil {
		if inner, err = json.Marshal(r.InnerWalls); err != nil {
			return types.AgentRules{}, fmt.Errorf("store: write agent rules: %w", err)
		}
	}
	var row pgx.Row
	if expected == 0 {
		row = q.QueryRow(ctx, `INSERT INTO agent_rules (harness, spec, inner_walls, version, updated_by, updated_at)
			VALUES ($1, $2, $3, 1, $4, now())
			ON CONFLICT (harness) DO NOTHING
			RETURNING `+agentRulesCols, r.Harness, spec, inner, r.UpdatedBy)
	} else {
		row = q.QueryRow(ctx, `UPDATE agent_rules
			SET spec = $2, inner_walls = $3, updated_by = $4, updated_at = now(), version = version + 1
			WHERE harness = $1 AND version = $5
			RETURNING `+agentRulesCols, r.Harness, spec, inner, r.UpdatedBy, expected)
	}
	out, err := scanAgentRules(row)
	if errors.Is(err, ErrNotFound) {
		return types.AgentRules{}, ErrAgentRulesVersion
	}
	return out, err
}

// DeleteAgentRulesQ is DeleteAgentRules on q, for a held change applied inside its decision transaction.
func DeleteAgentRulesQ(ctx context.Context, q Querier, harness string, expected int) (types.AgentRules, error) {
	cur, err := GetAgentRulesQ(ctx, q, harness)
	if err != nil {
		return types.AgentRules{}, err
	}
	if cur.Version != expected {
		return types.AgentRules{}, ErrAgentRulesVersion
	}
	out, err := scanAgentRules(q.QueryRow(ctx, `DELETE FROM agent_rules WHERE harness = $1 AND version = $2 RETURNING `+agentRulesCols, harness, expected))
	if errors.Is(err, ErrNotFound) {
		return types.AgentRules{}, ErrAgentRulesVersion
	}
	return out, err
}
