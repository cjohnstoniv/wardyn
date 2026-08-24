// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Capability grants and the per-kind enforcement switch (migration 0042).
// Kept out of store.go on purpose, mirroring store_sshkeys.go's split.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const capabilityGrantCols = `id, subject_type, subject, capability, value, effect, created_at, created_by`

// UpsertCapabilityGrant writes one grant, keyed on the natural
// (subject_type, subject, capability, value) UNIQUE: re-granting the same
// triple FLIPS the effect in place rather than leaving two contradictory rows
// behind (two rows would resolve as a permanent deny — deny beats allow — and
// be near-impossible for an admin to explain, let alone undo).
//
// The returned row carries the row's real id, which on a conflict is the
// EXISTING one, not g.ID: the caller needs the id the DELETE route will be
// given, and an admin re-submitting the same grant must not be handed an id
// that names no row.
func (s PG) UpsertCapabilityGrant(ctx context.Context, g types.CapabilityGrant) (types.CapabilityGrant, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	const q = `
		INSERT INTO capability_grants (id, subject_type, subject, capability, value, effect, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (subject_type, subject, capability, value) DO UPDATE
			SET effect = EXCLUDED.effect, created_by = EXCLUDED.created_by
		RETURNING ` + capabilityGrantCols
	return scanCapabilityGrant(s.Pool.QueryRow(ctx, q,
		g.ID, g.SubjectType, g.Subject, g.Capability, g.Value, g.Effect, g.CreatedBy))
}

// DeleteCapabilityGrant removes one grant by id. Returns ErrNotFound when no
// row matched — this is an admin-only surface, so there is no principal to
// scope the delete to and no existence oracle to worry about.
func (s PG) DeleteCapabilityGrant(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM capability_grants WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete capability grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCapabilityGrants returns every grant, oldest first — the admin
// Permissions screen's whole table in one read.
func (s PG) ListCapabilityGrants(ctx context.Context) ([]types.CapabilityGrant, error) {
	const q = `SELECT ` + capabilityGrantCols + ` FROM capability_grants
		ORDER BY capability, subject_type, subject, value`
	return collect(ctx, s.Pool, "list", "capability grants", q, nil, scanCapabilityGrant)
}

// ListCapabilityGrantsFor returns every grant that could apply to one caller:
// the `all` rows, plus `user` rows naming any of users (the caller's lowercased
// sub AND email — a grant on either hits), plus `group` rows naming any of
// groups (the login-time claim snapshot).
//
// Deliberately NOT filtered by capability: the resolver needs one kind and
// GET /me/capabilities needs all four, and a deployment's grant list is small
// enough that one round trip serving both beats two indexes and two queries.
// The per-kind and per-value matching (wildcards, host suffixes) then happens
// in Go, where the one host matcher already lives.
//
// ponytail: no cache. This runs per request, on the (subject_type, subject)
// index; a process-local cache is the HA blocker OPERATIONS already names for
// other state, and a stale permission cache is a security bug, not a slow page.
// Add one only behind a shared invalidation channel.
func (s PG) ListCapabilityGrantsFor(ctx context.Context, users, groups []string) ([]types.CapabilityGrant, error) {
	// A nil Go slice binds as SQL NULL, and `x = ANY(NULL)` is NULL, not false —
	// harmless here (it fails closed) but it makes the query's behavior depend on
	// a driver detail. Normalize so the predicate is always a real empty array.
	if users == nil {
		users = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	const q = `SELECT ` + capabilityGrantCols + ` FROM capability_grants
		WHERE subject_type = 'all'
		   OR (subject_type = 'user'  AND subject = ANY($1::text[]))
		   OR (subject_type = 'group' AND subject = ANY($2::text[]))
		ORDER BY capability, subject_type, subject, value`
	return collect(ctx, s.Pool, "list", "capability grants for subject", q,
		[]any{users, groups}, scanCapabilityGrant)
}

// GetCapabilityEnforcement returns the per-kind switch map. An ABSENT row means
// NOT ENFORCED, so the returned map is sparse by design and a missing key reads
// as false — that default is the whole zero-config back-compat story (see the
// migration). Never nil.
func (s PG) GetCapabilityEnforcement(ctx context.Context) (map[string]bool, error) {
	rows, err := s.Pool.Query(ctx, `SELECT capability, enabled FROM capability_enforcement`)
	if err != nil {
		return nil, fmt.Errorf("store: get capability enforcement: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		var enabled bool
		if err := rows.Scan(&k, &enabled); err != nil {
			return nil, fmt.Errorf("store: scan capability enforcement: %w", err)
		}
		out[k] = enabled
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get capability enforcement: %w", err)
	}
	return out, nil
}

// PutCapabilityEnforcement replaces the WHOLE switch map: any capability not
// named in enabled loses its row (absent == not enforced, so dropping the row
// and writing false mean the same thing, and dropping keeps a kind the Go side
// no longer defines from lingering).
//
// One statement, not a transaction: the DELETE and the INSERT run against the
// same snapshot inside a single CTE, they touch disjoint capability sets, and
// the pair is therefore already atomic without a round trip to BEGIN.
func (s PG) PutCapabilityEnforcement(ctx context.Context, enabled map[string]bool) (map[string]bool, error) {
	caps := make([]string, 0, len(enabled))
	vals := make([]bool, 0, len(enabled))
	for k, v := range enabled {
		caps = append(caps, k)
		vals = append(vals, v)
	}
	const q = `
		WITH incoming AS (SELECT k, v FROM unnest($1::text[], $2::bool[]) AS t(k, v)),
		     pruned AS (DELETE FROM capability_enforcement
		                WHERE capability NOT IN (SELECT k FROM incoming))
		INSERT INTO capability_enforcement (capability, enabled, updated_at)
		SELECT k, v, now() FROM incoming
		ON CONFLICT (capability) DO UPDATE
			SET enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at`
	if _, err := s.Pool.Exec(ctx, q, caps, vals); err != nil {
		return nil, fmt.Errorf("store: put capability enforcement: %w", err)
	}
	return s.GetCapabilityEnforcement(ctx)
}

func scanCapabilityGrant(row pgx.Row) (types.CapabilityGrant, error) {
	var g types.CapabilityGrant
	err := row.Scan(&g.ID, &g.SubjectType, &g.Subject, &g.Capability, &g.Value,
		&g.Effect, &g.CreatedAt, &g.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.CapabilityGrant{}, ErrNotFound
	}
	if err != nil {
		return types.CapabilityGrant{}, fmt.Errorf("store: scan capability grant: %w", err)
	}
	return g, nil
}
