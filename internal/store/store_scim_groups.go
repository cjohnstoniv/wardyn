// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The group removal ledger (migration 0118's group_remove kind): one set of rows per person and group, the
// target of every step being the group's id.
const (
	JobKindGroupRemove = "group_remove"

	GroupStepSessions = "sessions"
	GroupStepTokens   = "tokens"
	GroupStepAudit    = "audit"
)

// GroupRemovalSteps are the steps StartGroupRemoval opens, in the order they run.
var GroupRemovalSteps = []string{GroupStepSessions, GroupStepTokens, GroupStepAudit}

// SearchDisplayName is the Groups filter attribute that SearchExternalID shares the rest of with Users.
const SearchDisplayName = "displayName"

// ErrScimGroupExists is a create whose externalId already names a group.
var ErrScimGroupExists = errors.New("store: scim group exists")

// ScimGroup is a group the identity provider reported. ExternalID is the provider's id for it.
type ScimGroup struct {
	ID          uuid.UUID
	ExternalID  string
	DisplayName string
	CreatedAt   time.Time
}

// ScimGroupStore is optional, like LeaverStore: callers type-assert it.
type ScimGroupStore interface {
	CreateScimGroup(ctx context.Context, externalID, displayName string, now time.Time) (ScimGroup, error)
	GetScimGroup(ctx context.Context, id uuid.UUID) (ScimGroup, error)
	SearchScimGroups(ctx context.Context, attr, value string, limit int) ([]ScimGroup, error)
	RenameScimGroup(ctx context.Context, id uuid.UUID, displayName string) error
	ScimGroupMembers(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error)
	// AddScimGroupMembers records identities as members, skipping any id that names no identity.
	AddScimGroupMembers(ctx context.Context, id uuid.UUID, identities []uuid.UUID, now time.Time) error
	StartGroupRemoval(ctx context.Context, groupID, identityID uuid.UUID) error
	GroupRemovalIdentities(ctx context.Context, groupID uuid.UUID) ([]uuid.UUID, error)
	DeleteScimGroup(ctx context.Context, id uuid.UUID) error
}

var _ ScimGroupStore = PG{}

const scimGroupCols = `id, external_id, display_name, created_at`

func scanScimGroup(r pgx.Row) (ScimGroup, error) {
	var g ScimGroup
	err := r.Scan(&g.ID, &g.ExternalID, &g.DisplayName, &g.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ScimGroup{}, ErrNotFound
	}
	return g, err
}

// CreateScimGroup stores a group. An externalId any group already holds, in any case, is
// ErrScimGroupExists.
func (s PG) CreateScimGroup(ctx context.Context, externalID, displayName string, now time.Time) (ScimGroup, error) {
	g, err := scanScimGroup(s.Pool.QueryRow(ctx, `
		INSERT INTO scim_groups (external_id, display_name, created_at) VALUES ($1, $2, $3)
		RETURNING `+scimGroupCols, strings.TrimSpace(externalID), displayName, now))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ScimGroup{}, ErrScimGroupExists
	}
	if err != nil {
		return ScimGroup{}, fmt.Errorf("store: create scim group: %w", err)
	}
	return g, nil
}

// GetScimGroup is the group stored under id, or ErrNotFound.
func (s PG) GetScimGroup(ctx context.Context, id uuid.UUID) (ScimGroup, error) {
	return scanScimGroup(s.Pool.QueryRow(ctx, `SELECT `+scimGroupCols+` FROM scim_groups WHERE id = $1`, id))
}

// SearchScimGroups is the groups a SCIM filter `attr eq value` selects, at most limit. Both attributes
// match case-insensitively.
func (s PG) SearchScimGroups(ctx context.Context, attr, value string, limit int) ([]ScimGroup, error) {
	var where string
	switch attr {
	case SearchExternalID:
		where = `lower(btrim(external_id)) = $1`
	case SearchDisplayName:
		where = `lower(display_name) = $1`
	default:
		return nil, fmt.Errorf("store: search scim groups: unknown attribute %q", attr)
	}
	return collect(ctx, s.Pool, "search", "scim groups", `SELECT `+scimGroupCols+` FROM scim_groups
		WHERE `+where+` ORDER BY created_at, id LIMIT $2`,
		[]any{strings.ToLower(strings.TrimSpace(value)), limit}, scanScimGroup)
}

// RenameScimGroup changes a group's display name; its externalId never changes.
func (s PG) RenameScimGroup(ctx context.Context, id uuid.UUID, displayName string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE scim_groups SET display_name = $2 WHERE id = $1`, id, displayName)
	if err != nil {
		return fmt.Errorf("store: rename scim group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ScimGroupMembers is the identity ids recorded as members of the group, oldest first.
func (s PG) ScimGroupMembers(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	return collect(ctx, s.Pool, "list", "scim group members", `SELECT identity_id FROM scim_group_members
		WHERE group_id = $1 ORDER BY added_at, identity_id`, []any{id}, func(r pgx.Row) (uuid.UUID, error) {
		var v uuid.UUID
		return v, r.Scan(&v)
	})
}

// AddScimGroupMembers records each identity as a member, leaving one already recorded as it is and
// skipping an id that names no identity row.
func (s PG) AddScimGroupMembers(ctx context.Context, id uuid.UUID, identities []uuid.UUID, now time.Time) error {
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO scim_group_members (group_id, identity_id, added_at)
		SELECT $1, i.id, $3 FROM principal_identities i WHERE i.id = ANY($2::uuid[])
		ON CONFLICT DO NOTHING`, id, identities, now); err != nil {
		return fmt.Errorf("store: add scim group members: %w", err)
	}
	return nil
}

// StartGroupRemoval opens the ledger of one person leaving one group, in one transaction with the
// removal of the membership row. The three steps join as pending when the person was recorded a member
// (a new removal, so an earlier finished one is cleared) or when no removal of this pair was ever
// recorded (a member whose add never reached Wardyn is still removed). A pair whose last removal
// finished and who has not been added since is a repeat and changes nothing, so an identity provider
// that repeats the request costs nothing, and a removal still pending is resumed.
func (s PG) StartGroupRemoval(ctx context.Context, groupID, identityID uuid.UUID) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: start group removal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	target := groupID.String()
	tag, err := tx.Exec(ctx, `DELETE FROM scim_group_members WHERE group_id = $1 AND identity_id = $2`, groupID, identityID)
	if err != nil {
		return fmt.Errorf("store: remove group member: %w", err)
	}
	var existing int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND target = $3`,
		identityID, JobKindGroupRemove, target).Scan(&existing); err != nil {
		return fmt.Errorf("store: read group removal ledger: %w", err)
	}
	if tag.RowsAffected() > 0 && existing > 0 {
		if _, err = tx.Exec(ctx, `DELETE FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND target = $3`,
			identityID, JobKindGroupRemove, target); err != nil {
			return fmt.Errorf("store: reset group removal ledger: %w", err)
		}
		existing = 0
	}
	if existing == 0 {
		for _, step := range GroupRemovalSteps {
			if _, err = tx.Exec(ctx, `
				INSERT INTO deprovision_jobs (identity_id, kind, step, target) VALUES ($1, $2, $3, $4)
				ON CONFLICT (identity_id, kind, step, target) DO NOTHING`, identityID, JobKindGroupRemove, step, target); err != nil {
				return fmt.Errorf("store: open group removal ledger: %w", err)
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit group removal: %w", err)
	}
	return nil
}

// GroupRemovalIdentities is every identity a group's deletion must remove as a mover: the recorded
// members, and anyone whose removal from this group is still pending.
func (s PG) GroupRemovalIdentities(ctx context.Context, groupID uuid.UUID) ([]uuid.UUID, error) {
	return collect(ctx, s.Pool, "list", "group removal identities", `
		SELECT identity_id FROM scim_group_members WHERE group_id = $1
		UNION SELECT identity_id FROM deprovision_jobs WHERE kind = $2 AND target = $3 AND state = 'pending'
		ORDER BY 1`, []any{groupID, JobKindGroupRemove, groupID.String()}, func(r pgx.Row) (uuid.UUID, error) {
		var v uuid.UUID
		return v, r.Scan(&v)
	})
}

// DeleteScimGroup removes the group row and, by cascade, its members. Its ledger rows stay.
func (s PG) DeleteScimGroup(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM scim_groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete scim group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
