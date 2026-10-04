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
)

// PersonErasureStore is what erasing a person reads and writes beyond the
// stores that own a scope: the person's runs, and the identity directory that
// maps the names an audit row carries onto one principal. Optional like
// PrincipalIdentityStore: callers type-assert and refuse a scope the store
// cannot serve rather than report it erased.
type PersonErasureStore interface {
	// RunIDsCreatedBy lists every run created_by createdBy.
	RunIDsCreatedBy(ctx context.Context, createdBy string) ([]uuid.UUID, error)
	// BlankRunTasks empties the task text of every run created_by createdBy and
	// returns how many it changed. It never touches updated_at: the idle reaper
	// reads it.
	BlankRunTasks(ctx context.Context, createdBy string) (int, error)
	// PrincipalForName maps a name an event carries (a principal, an
	// entra:<tenant>:<object> form, an email the person signed in with) to the
	// person's principal. A name that resolves to none, or to more than one, is
	// returned as it is.
	PrincipalForName(ctx context.Context, name string) (string, error)
	// PrincipalAliases lists the names the identity directory knows principal
	// by: the emails it signed in with (lowercased) and its entra:<tenant>:<object>
	// form. A row sealed under a name the directory did not yet know is under
	// that name's own key, so an erasure destroys these too.
	PrincipalAliases(ctx context.Context, principal string) ([]string, error)
	// EraseGovernanceChangePersonalFields clears the proposer and decider of every governance change
	// recorded under one of names (a principal, or an email matched case-folded) and moves a pending
	// change whose proposer is one of them to expired. It returns the rows it touched.
	EraseGovernanceChangePersonalFields(ctx context.Context, names []string) (int, error)
	// SubjectKeyDestroyedSince reports whether any generation of owner's key
	// for purpose has a destroyed_at after since.
	SubjectKeyDestroyedSince(ctx context.Context, owner, purpose string, since time.Time) (bool, error)
}

var _ PersonErasureStore = PG{}

func (s PG) RunIDsCreatedBy(ctx context.Context, createdBy string) ([]uuid.UUID, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM agent_runs WHERE created_by = $1 ORDER BY created_at, id`, createdBy)
	if err != nil {
		return nil, fmt.Errorf("store: list a person's runs: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("store: list a person's runs: %w", err)
	}
	return ids, nil
}

func (s PG) BlankRunTasks(ctx context.Context, createdBy string) (int, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET task = '' WHERE created_by = $1 AND task <> ''`, createdBy)
	if err != nil {
		return 0, fmt.Errorf("store: blank a person's run tasks: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s PG) PrincipalForName(ctx context.Context, name string) (string, error) {
	if name == "" {
		return name, nil
	}
	one := func(q string, args ...any) (string, bool, error) {
		rows, err := s.Pool.Query(ctx, q, args...)
		if err != nil {
			return "", false, fmt.Errorf("store: resolve a principal: %w", err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return "", false, fmt.Errorf("store: resolve a principal: %w", err)
		}
		if len(found) != 1 {
			return "", false, nil
		}
		return found[0], true, nil
	}
	if p, ok, err := one(`SELECT DISTINCT principal FROM principal_identities WHERE principal = $1`, name); err != nil || ok {
		return p, err
	}
	if rest, ok := strings.CutPrefix(name, "entra:"); ok {
		if tid, oid, ok := strings.Cut(rest, ":"); ok && tid != "" && oid != "" {
			if p, ok, err := one(`SELECT DISTINCT principal FROM principal_identities
				WHERE tenant_id = $1 AND object_id = $2 AND principal IS NOT NULL`, tid, oid); err != nil || ok {
				return p, err
			}
		}
	}
	if strings.Contains(name, "@") {
		if p, ok, err := one(`SELECT DISTINCT i.principal FROM principal_identity_aliases a
			JOIN principal_identities i ON i.id = a.identity_id
			WHERE a.kind = 'email' AND a.value_lower = lower($1) AND i.principal IS NOT NULL`, name); err != nil || ok {
			return p, err
		}
	}
	return name, nil
}

func (s PG) PrincipalAliases(ctx context.Context, principal string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT a.value_lower FROM principal_identity_aliases a
		  JOIN principal_identities i ON i.id = a.identity_id WHERE i.principal = $1
		UNION
		SELECT 'entra:' || tenant_id || ':' || object_id FROM principal_identities WHERE principal = $1 AND object_id <> ''
		ORDER BY 1`, principal)
	if err != nil {
		return nil, fmt.Errorf("store: list a principal's aliases: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: list a principal's aliases: %w", err)
	}
	return names, nil
}

func (s PG) SubjectKeyDestroyedSince(ctx context.Context, owner, purpose string, since time.Time) (bool, error) {
	var gone bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principal_keys WHERE owner = $1 AND purpose = $2 AND destroyed_at > $3)`,
		owner, purpose, since).Scan(&gone)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("store: read a subject key's destruction: %w", err)
	}
	return gone, nil
}
