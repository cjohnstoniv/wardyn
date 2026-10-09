// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PeopleDirectoryStore lists the people a deployment knows: every identity that has signed in
// (principal_identities, migration 0113) and every person an admin set up beforehand (people,
// migration 0090). Optional, like PersonStore.
type PeopleDirectoryStore interface {
	// ListPeopleDirectory returns one page of the directory, ordered by principal.
	ListPeopleDirectory(ctx context.Context, f PeopleDirectoryFilter) (PeopleDirectoryPage, error)
}

var _ PeopleDirectoryStore = PG{}

// A Limit of zero is PeopleDirectoryDefaultLimit; one above PeopleDirectoryMaxLimit is clamped to it.
const (
	PeopleDirectoryDefaultLimit = 50
	PeopleDirectoryMaxLimit     = 200
)

// PeopleDirectoryActiveSessionWindow is how recent a sign-in must be to count as a live session.
// Sessions are stateless signed cookies the server keeps no row for, so the directory knows only
// when a person last signed in, not how many cookies are out: it counts one when that sign-in is
// inside this window and no revoke cutoff has passed it.
const PeopleDirectoryActiveSessionWindow = 24 * time.Hour

// PeopleDirectoryFilter narrows and pages ListPeopleDirectory.
type PeopleDirectoryFilter struct {
	// Query keeps people whose principal starts with it (case-sensitive) or whose email starts
	// with it (case-insensitive). Empty keeps everyone.
	Query string
	// State is "active" (not deactivated), "deactivated", or "" for both.
	State string
	// After is the last principal of the previous page; empty starts at the top.
	After string
	Limit int
}

// PersonListing is one directory row.
type PersonListing struct {
	Principal string
	Email     string
	// Entra is true for a person keyed by Entra tenant and object id.
	Entra      bool
	PreCreated bool
	// FirstSignInAt and LastSignInAt are nil for a person who has never signed in.
	FirstSignInAt *time.Time
	LastSignInAt  *time.Time
	DeactivatedAt *time.Time
	// ActiveSessions is 0 or 1, see PeopleDirectoryActiveSessionWindow.
	ActiveSessions int
	APITokens      int
	SSHKeys        int
	Credentials    int
	ActiveRuns     int
	// Groups, GroupsTruncated and GroupsVerified are the person's last verified login groups
	// (key_domain_login_groups, stamped at every SSO sign-in). GroupsVerified is false when no
	// snapshot exists; GroupsTruncated is the login's own bit that its groups claim was cut short.
	Groups          []string
	GroupsTruncated bool
	GroupsVerified  bool
}

// PeopleDirectoryPage is one page; Next is the principal to pass as After, or "" on the last page.
type PeopleDirectoryPage struct {
	People []PersonListing
	Next   string
}

// peopleDirectoryRows is the identity side (one row per principal, over every issuer it has
// signed in on) full-joined to the people table, so a pre-created person who never signed in and
// a sub-keyed user with no people row both appear. A person is deactivated when every identity
// row of theirs is or when their people row is (a suspension over SCIM before the first sign-in).
const peopleDirectoryRows = `
WITH ident AS (
    SELECT principal,
           bool_or(object_id <> '') AS entra,
           (array_agg(email_lower ORDER BY last_login_at DESC NULLS LAST, id) FILTER (WHERE email_lower <> ''))[1] AS email,
           min(created_at) AS first_seen,
           max(last_login_at) AS last_login_at,
           CASE WHEN bool_and(deactivated_at IS NOT NULL) THEN max(deactivated_at) END AS deactivated_at
      FROM principal_identities
     WHERE principal IS NOT NULL
     GROUP BY principal
), dir AS (
    SELECT COALESCE(i.principal, p.principal) AS principal,
           COALESCE(NULLIF(i.email, ''), NULLIF(p.email, ''), '') AS email,
           COALESCE(i.entra, p.object_id <> '') AS entra,
           p.principal IS NOT NULL AS pre_created,
           COALESCE(p.first_signed_in_at, i.first_seen) AS first_sign_in_at,
           i.last_login_at AS last_sign_in_at,
           COALESCE(i.deactivated_at, p.deactivated_at) AS deactivated_at
      FROM ident i
      FULL JOIN people p ON p.principal = i.principal
)
SELECT dir.principal, dir.email, dir.entra, dir.pre_created, dir.first_sign_in_at, dir.last_sign_in_at, dir.deactivated_at,
       COALESCE(g.groups, '[]'::jsonb), COALESCE(g.truncated, false), g.principal IS NOT NULL
  FROM dir
  LEFT JOIN key_domain_login_groups g ON g.principal = dir.principal
 WHERE ($1 = '' OR starts_with(dir.principal, $1) OR starts_with(lower(dir.email), lower($1)))
   AND ($2 = '' OR ($2 = 'active' AND dir.deactivated_at IS NULL) OR ($2 = 'deactivated' AND dir.deactivated_at IS NOT NULL))
   AND ($3 = '' OR dir.principal COLLATE "C" > $3 COLLATE "C")
 ORDER BY dir.principal COLLATE "C"
 LIMIT $4`

// peopleCounts are the grouped reads that fill a page's counts, one query each over the page's
// principals: never one query per person.
var peopleCounts = []struct {
	what, query string
	nonTerminal bool
	set         func(*PersonListing, int)
}{
	{"api tokens", `SELECT principal, count(*) FROM api_tokens
		WHERE principal = ANY($1) AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()) GROUP BY principal`,
		false, func(p *PersonListing, n int) { p.APITokens = n }},
	{"ssh keys", `SELECT principal, count(*) FROM ssh_public_keys WHERE principal = ANY($1) GROUP BY principal`,
		false, func(p *PersonListing, n int) { p.SSHKeys = n }},
	{"credentials", `SELECT owned_by, count(*) FROM secrets WHERE owned_by = ANY($1) GROUP BY owned_by`,
		false, func(p *PersonListing, n int) { p.Credentials = n }},
	{"runs", `SELECT created_by, count(*) FROM agent_runs WHERE created_by = ANY($1) AND state = ANY($2) GROUP BY created_by`,
		true, func(p *PersonListing, n int) { p.ActiveRuns = n }},
}

func (s PG) ListPeopleDirectory(ctx context.Context, f PeopleDirectoryFilter) (PeopleDirectoryPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = PeopleDirectoryDefaultLimit
	}
	limit = min(limit, PeopleDirectoryMaxLimit)
	rows, err := s.Pool.Query(ctx, peopleDirectoryRows, f.Query, f.State, f.After, limit+1)
	if err != nil {
		return PeopleDirectoryPage{}, fmt.Errorf("store: list people directory: %w", err)
	}
	people, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PersonListing, error) {
		var p PersonListing
		var groups []byte
		if err := r.Scan(&p.Principal, &p.Email, &p.Entra, &p.PreCreated, &p.FirstSignInAt, &p.LastSignInAt, &p.DeactivatedAt,
			&groups, &p.GroupsTruncated, &p.GroupsVerified); err != nil {
			return p, err
		}
		if err := json.Unmarshal(groups, &p.Groups); err != nil {
			return p, fmt.Errorf("the stored groups of %q are not a list: %w", p.Principal, err)
		}
		return p, nil
	})
	if err != nil {
		return PeopleDirectoryPage{}, fmt.Errorf("store: list people directory: %w", err)
	}
	var page PeopleDirectoryPage
	if len(people) > limit {
		people = people[:limit]
		page.Next = people[limit-1].Principal
	}
	page.People = people
	if len(people) == 0 {
		return page, nil
	}

	principals := make([]string, len(people))
	idx := make(map[string]int, len(people))
	emails := make([]string, 0, len(people))
	for i, p := range people {
		principals[i], idx[p.Principal] = p.Principal, i
		if p.Email != "" {
			emails = append(emails, strings.ToLower(p.Email))
		}
	}
	for _, c := range peopleCounts {
		args := []any{principals}
		if c.nonTerminal {
			args = append(args, nonTerminalStateNames())
		}
		counts, err := s.Pool.Query(ctx, c.query, args...)
		if err != nil {
			return PeopleDirectoryPage{}, fmt.Errorf("store: count people %s: %w", c.what, err)
		}
		for counts.Next() {
			var who string
			var n int
			if err := counts.Scan(&who, &n); err != nil {
				counts.Close()
				return PeopleDirectoryPage{}, fmt.Errorf("store: count people %s: %w", c.what, err)
			}
			if i, ok := idx[who]; ok {
				c.set(&page.People[i], n)
			}
		}
		counts.Close()
		if err := counts.Err(); err != nil {
			return PeopleDirectoryPage{}, fmt.Errorf("store: count people %s: %w", c.what, err)
		}
	}
	return page, s.countActiveSessions(ctx, page.People, principals, emails)
}

// countActiveSessions sets ActiveSessions from the last sign-in and the revoke cutoffs
// (oidc_session_revocations, and oidc_session_cuts where "Sign out everywhere" stamps a sessions-only cut).
// A cutoff names a sub exactly or an email case-insensitively, and the
// empty sub revokes everyone, the same match IsSessionRevoked makes.
func (s PG) countActiveSessions(ctx context.Context, people []PersonListing, principals, emails []string) error {
	rows, err := s.Pool.Query(ctx, `SELECT sub, revoked_at FROM oidc_session_revocations WHERE sub = ANY($1) OR lower(sub) = ANY($2)
		UNION ALL SELECT sub, cut_at FROM oidc_session_cuts WHERE sub = ANY($1) OR lower(sub) = ANY($2)`,
		append(principals, ""), emails)
	if err != nil {
		return fmt.Errorf("store: read session revocations: %w", err)
	}
	type cutoff struct {
		sub string
		at  time.Time
	}
	cutoffs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cutoff, error) {
		var c cutoff
		return c, r.Scan(&c.sub, &c.at)
	})
	if err != nil {
		return fmt.Errorf("store: read session revocations: %w", err)
	}
	now := s.now()
	for i := range people {
		p := &people[i]
		if p.LastSignInAt == nil || p.DeactivatedAt != nil || now.Sub(*p.LastSignInAt) > PeopleDirectoryActiveSessionWindow {
			continue
		}
		p.ActiveSessions = 1
		for _, c := range cutoffs {
			if (c.sub == "" || c.sub == p.Principal || (p.Email != "" && strings.EqualFold(c.sub, p.Email))) && !c.at.Before(*p.LastSignInAt) {
				p.ActiveSessions = 0
			}
		}
	}
	return nil
}
