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
	// person's principal through the same directory as PrincipalsNamed, people an
	// admin created included. A name that resolves to none, or to more than one,
	// is returned as it is.
	PrincipalForName(ctx context.Context, name string) (string, error)
	// PrincipalAliases lists the names the identity directory knows principal
	// by: the emails it signed in with (lowercased) and its entra:<tenant>:<object>
	// form. A row sealed under a name the directory did not yet know is under
	// that name's own key, so an erasure destroys these too.
	// Its email values are the claim text a sign-in sent, never checked for an @, so they can
	// spell another person's principal or entra form: match on them, never destroy by them.
	PrincipalAliases(ctx context.Context, principal string) ([]string, error)
	// PrincipalEntraNames lists the entra:<tenant>:<object> forms of principal's own sign-in
	// rows, built from their tenant and object ids and never from an alias value.
	PrincipalEntraNames(ctx context.Context, principal string) ([]string, error)
	// EraseGovernanceChangePersonalFields clears the proposer and decider (and the decider's reason) of
	// every governance change recorded under one of names (a principal, or an email matched
	// case-folded) and moves a pending change whose proposer is one of them to expired. It returns the
	// rows it touched.
	EraseGovernanceChangePersonalFields(ctx context.Context, names []string) (int, error)
	// SubjectKeyDestroyedSince reports whether any generation of owner's key
	// for purpose has a destroyed_at after since.
	SubjectKeyDestroyedSince(ctx context.Context, owner, purpose string, since time.Time) (bool, error)
}

var _ PersonErasureStore = PG{}

// PrincipalDirectoryStore answers who a name an admin typed is, from the
// identity directory (principal_identities and its aliases, and the people an
// admin pre-created): the people the People page lists, whether or not they
// ever minted an API token or created a workspace. Optional like
// PersonErasureStore.
type PrincipalDirectoryStore interface {
	// PrincipalsNamed returns the distinct principals name stands for, a person created by
	// object id who has signed in under a pairwise subject counting as that subject. The
	// first of these that matches decides: the exact principal, the
	// entra:<tenant>:<object> form, then an email (case-folded) the person
	// signed in with or was created under. More than one is ambiguous; none is
	// a name the directory does not know.
	PrincipalsNamed(ctx context.Context, name string) ([]string, error)
}

var _ PrincipalDirectoryStore = PG{}

// signedInAs wraps a query of principals so that a person an admin created by object id
// (a people row entra:<tenant>:<object>) who then signed in under a pairwise subject is the subject,
// the principal their credentials, runs and audit rows carry. The link is the one the sign-in itself
// makes (samePersonKey): the same issuer, tenant and object id. The people row's own namespace is
// empty, and erasing it would report success over a person's data left behind.
func signedInAs(query string) string {
	return `SELECT DISTINCT COALESCE(i.principal, h.principal) FROM (` + query + `) h(principal)
	  LEFT JOIN people p ON p.principal = h.principal AND p.object_id <> ''
	  LEFT JOIN principal_identities i ON i.issuer = p.issuer AND i.tenant_id = p.tenant_id
	   AND i.object_id = p.object_id AND i.principal IS NOT NULL`
}

func (s PG) PrincipalsNamed(ctx context.Context, name string) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	type tier struct {
		query string
		args  []any
	}
	tiers := []tier{
		{`SELECT principal FROM principal_identities WHERE principal = $1
		  UNION SELECT principal FROM people WHERE principal = $1`, []any{name}},
	}
	if rest, ok := strings.CutPrefix(name, "entra:"); ok {
		if tid, oid, ok := strings.Cut(rest, ":"); ok && tid != "" && oid != "" {
			tiers = append(tiers, tier{`SELECT principal FROM principal_identities WHERE tenant_id = $1 AND object_id = $2 AND principal IS NOT NULL
			   UNION SELECT principal FROM people WHERE tenant_id = $1 AND object_id = $2`, []any{tid, oid}})
		}
	}
	if strings.Contains(name, "@") {
		tiers = append(tiers, tier{`SELECT i.principal FROM principal_identity_aliases a
		     JOIN principal_identities i ON i.id = a.identity_id
		    WHERE a.kind = 'email' AND a.value_lower = lower($1) AND i.principal IS NOT NULL
		   UNION SELECT principal FROM principal_identities WHERE email_lower = lower($1) AND principal IS NOT NULL
		   UNION SELECT principal FROM people WHERE email <> '' AND lower(email) = lower($1)`, []any{name}})
	}
	for _, t := range tiers {
		rows, err := s.Pool.Query(ctx, signedInAs(t.query)+` ORDER BY 1`, t.args...)
		if err != nil {
			return nil, fmt.Errorf("store: resolve a name to principals: %w", err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("store: resolve a name to principals: %w", err)
		}
		if len(found) > 0 {
			return found, nil
		}
	}
	return nil, nil
}

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
	found, err := s.PrincipalsNamed(ctx, name)
	if err != nil || len(found) != 1 {
		return name, err
	}
	return found[0], nil
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

func (s PG) PrincipalEntraNames(ctx context.Context, principal string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT 'entra:' || tenant_id || ':' || object_id FROM principal_identities
		 WHERE principal = $1 AND object_id <> '' ORDER BY 1`, principal)
	if err != nil {
		return nil, fmt.Errorf("store: list a principal's entra names: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: list a principal's entra names: %w", err)
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
