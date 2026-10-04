// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keydomain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// The subject types of an assignment: the governance vocabulary
// (migration 0052's governance_assignments).
const (
	SubjectUser  = "user"
	SubjectGroup = "group"
	SubjectAll   = "all"
)

// ErrUnknownDomain is a domain the file does not declare.
var ErrUnknownDomain = errors.New("key domain is not declared in WARDYN_KEY_DOMAINS_FILE")

// ErrAmbiguous is a person whose groups are assigned to different domains, with
// no user assignment to settle it. The new generation is refused by name rather
// than placed by a guess.
var ErrAmbiguous = errors.New("key domain membership is ambiguous")

// ErrGroupsTruncated is a person whose last login lost groups while group
// assignments exist: which domain they belong to cannot be known.
var ErrGroupsTruncated = errors.New("key domain membership cannot be decided from a truncated group snapshot")

// Assignment is one row of key_domain_assignments.
type Assignment struct {
	SubjectType string    `json:"subject_type"`
	Subject     string    `json:"subject"`
	Domain      string    `json:"domain"`
	SetBy       string    `json:"set_by"`
	SetAt       time.Time `json:"set_at"`
}

// Usage is how many live principal keys sit in one domain.
type Usage struct {
	Domain   string `json:"domain"`
	Declared bool   `json:"declared"`
	LiveKeys int    `json:"live_keys"`
}

// Service resolves which domain a subject's next key generation goes to, and
// reads and writes the assignments that say so. It never declares a domain:
// declared is the file's names.
type Service struct {
	pool     *pgxpool.Pool
	declared map[string]bool
}

// NewService returns a Service over pool for the domains names declares.
func NewService(pool *pgxpool.Pool, declared []string) *Service {
	s := &Service{pool: pool, declared: map[string]bool{}}
	for _, n := range declared {
		s.declared[n] = true
	}
	return s
}

// Declared is the domains the file declares, sorted. It never includes Default.
func (s *Service) Declared() []string {
	out := make([]string, 0, len(s.declared))
	for n := range s.declared {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Has reports whether domain is Default or declared.
func (s *Service) Has(domain string) bool { return domain == Default || s.declared[domain] }

func unavailable(op string, err error) error {
	return fmt.Errorf("keydomain: %s: %w: %w", op, secretstore.ErrUnavailable, err)
}

// Domain is the domain owner's next principal-key generation is wrapped under:
// a user assignment, else the one domain their groups are assigned to, else an
// all assignment, else Default. Group facts are those of the person's last
// verified login (RecordLoginGroups).
//
// It fails closed: two groups assigned to different domains is ErrAmbiguous, a
// truncated snapshot beside group assignments is ErrGroupsTruncated, and a
// domain the file no longer declares is ErrUnknownDomain. A Postgres that does
// not answer is secretstore.ErrUnavailable.
func (s *Service) Domain(ctx context.Context, owner string) (string, error) {
	p, err := s.Place(ctx, owner)
	return p.Domain, err
}

// The ways a person's domain was decided (Placement.Source).
const (
	SourceUser    = "user"    // an assignment for the person
	SourceGroup   = "group"   // one group the person's last login carried
	SourceAll     = "all"     // the assignment for everyone
	SourceDefault = "default" // nothing matched
)

// Placement is where owner's next principal-key generation goes, and why.
type Placement struct {
	Domain string
	Source string
	// Group is the group that named the domain, when Source is SourceGroup.
	Group string
}

// Place is Domain with the reason: the same decision, the same refusals.
func (s *Service) Place(ctx context.Context, owner string) (Placement, error) {
	var groups []string
	var truncated bool
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT groups, truncated FROM key_domain_login_groups WHERE principal=$1`, owner).Scan(&raw, &truncated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Placement{}, unavailable("read the login's groups", err)
	default:
		if err := json.Unmarshal(raw, &groups); err != nil {
			return Placement{}, fmt.Errorf("keydomain: the stored groups of %q are not a list: %w", owner, err)
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT subject_type, subject, domain FROM key_domain_assignments
		WHERE (subject_type='user' AND subject=$1) OR subject_type='all' OR (subject_type='group' AND subject = ANY($2))`,
		owner, groups)
	if err != nil {
		return Placement{}, unavailable("read the assignments", err)
	}
	hits, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Type, Subject, Domain string }])
	if err != nil {
		return Placement{}, unavailable("read the assignments", err)
	}
	var user, all string
	byGroup := map[string][]string{} // domain -> the groups naming it
	for _, h := range hits {
		switch h.Type {
		case SubjectUser:
			user = h.Domain
		case SubjectAll:
			all = h.Domain
		case SubjectGroup:
			byGroup[h.Domain] = append(byGroup[h.Domain], h.Subject)
		}
	}
	place := Placement{Domain: Default, Source: SourceDefault}
	switch {
	case user != "":
		place = Placement{Domain: user, Source: SourceUser}
	case len(byGroup) > 1:
		var parts []string
		for d, gs := range byGroup {
			slices.Sort(gs)
			parts = append(parts, fmt.Sprintf("%s (%s)", d, strings.Join(gs, ", ")))
		}
		slices.Sort(parts)
		return Placement{}, fmt.Errorf("%w: %q is in groups assigned to %s; assign the person to one domain as a user", ErrAmbiguous, owner, strings.Join(parts, " and "))
	case len(byGroup) == 1:
		for d, gs := range byGroup {
			slices.Sort(gs)
			place = Placement{Domain: d, Source: SourceGroup, Group: gs[0]}
		}
	case all != "":
		place = Placement{Domain: all, Source: SourceAll}
	}
	if truncated && user == "" {
		var any bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM key_domain_assignments WHERE subject_type='group')`).Scan(&any); err != nil {
			return Placement{}, unavailable("read the assignments", err)
		}
		if any {
			return Placement{}, fmt.Errorf("%w: the last sign-in of %q lost groups; assign the person to a domain as a user, or have them sign in again", ErrGroupsTruncated, owner)
		}
	}
	if !s.Has(place.Domain) {
		return Placement{}, fmt.Errorf("%w: %q is assigned to %q", ErrUnknownDomain, owner, place.Domain)
	}
	return place, nil
}

// RecordLoginGroups stamps the groups of a verified login, which a group
// assignment reads. truncated is the login's own bit.
func (s *Service) RecordLoginGroups(ctx context.Context, principal string, groups []string, truncated bool) error {
	if principal == "" {
		return nil
	}
	if groups == nil {
		groups = []string{}
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO key_domain_login_groups (principal, groups, truncated) VALUES ($1, $2, $3)
		ON CONFLICT (principal) DO UPDATE SET groups=EXCLUDED.groups, truncated=EXCLUDED.truncated, verified_at=now()`,
		principal, b, truncated); err != nil {
		return unavailable("record the login's groups", err)
	}
	return nil
}

// List returns every assignment, by type, subject.
func (s *Service) List(ctx context.Context) ([]Assignment, error) {
	rows, err := s.pool.Query(ctx, `SELECT subject_type, subject, domain, set_by, set_at FROM key_domain_assignments ORDER BY subject_type, subject`)
	if err != nil {
		return nil, unavailable("list the assignments", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Assignment])
	if err != nil {
		return nil, unavailable("list the assignments", err)
	}
	if out == nil {
		out = []Assignment{}
	}
	return out, nil
}

// Usage counts the live principal keys per domain, declared domains first with
// zero for one that holds none, and any other domain a live key names.
func (s *Service) Usage(ctx context.Context) ([]Usage, error) {
	rows, err := s.pool.Query(ctx, `SELECT domain, count(*) FROM principal_keys WHERE destroyed_at IS NULL GROUP BY domain`)
	if err != nil {
		return nil, unavailable("count the live keys", err)
	}
	counts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Domain string
		N      int
	}])
	if err != nil {
		return nil, unavailable("count the live keys", err)
	}
	n := map[string]int{}
	for _, c := range counts {
		n[c.Domain] = c.N
	}
	names := append([]string{Default}, s.Declared()...)
	for d := range n {
		if !slices.Contains(names, d) {
			names = append(names, d)
		}
	}
	out := make([]Usage, 0, len(names))
	for _, d := range names {
		out = append(out, Usage{Domain: d, Declared: s.Has(d), LiveKeys: n[d]})
	}
	return out, nil
}

// Set writes the assignment for (a.SubjectType, a.Subject), and reports whether
// it created it. The domain must be declared (ErrUnknownDomain); Default is
// declared and says "back to the credential key".
func (s *Service) Set(ctx context.Context, a Assignment) (created bool, err error) {
	if !s.Has(a.Domain) {
		return false, fmt.Errorf("%w: %q", ErrUnknownDomain, a.Domain)
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO key_domain_assignments (subject_type, subject, domain, set_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (subject_type, subject) DO UPDATE SET domain=EXCLUDED.domain, set_by=EXCLUDED.set_by, set_at=now()
		RETURNING (xmax = 0)`, a.SubjectType, a.Subject, a.Domain, a.SetBy).Scan(&created)
	if err != nil {
		return false, unavailable("write the assignment", err)
	}
	return created, nil
}

// Get returns one assignment; false when there is none.
func (s *Service) Get(ctx context.Context, subjectType, subject string) (Assignment, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT subject_type, subject, domain, set_by, set_at FROM key_domain_assignments WHERE subject_type=$1 AND subject=$2`, subjectType, subject)
	if err != nil {
		return Assignment{}, false, unavailable("read the assignment", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Assignment])
	if err != nil {
		return Assignment{}, false, unavailable("read the assignment", err)
	}
	if len(got) == 0 {
		return Assignment{}, false, nil
	}
	return got[0], true, nil
}

// Delete removes one assignment and returns the row it removed; false when
// there was none. Nothing already written moves: the subject's next generation
// is placed by what is left.
func (s *Service) Delete(ctx context.Context, subjectType, subject string) (Assignment, bool, error) {
	rows, err := s.pool.Query(ctx, `DELETE FROM key_domain_assignments WHERE subject_type=$1 AND subject=$2
		RETURNING subject_type, subject, domain, set_by, set_at`, subjectType, subject)
	if err != nil {
		return Assignment{}, false, unavailable("delete the assignment", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Assignment])
	if err != nil {
		return Assignment{}, false, unavailable("delete the assignment", err)
	}
	if len(got) == 0 {
		return Assignment{}, false, nil
	}
	return got[0], true, nil
}

// AmbiguousIfGroup counts the people whose last login carries group and another
// group assigned to a domain other than domain, with no user assignment of
// their own: assigning group to domain would make them ambiguous.
func (s *Service) AmbiguousIfGroup(ctx context.Context, group, domain string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM key_domain_login_groups l
		WHERE l.groups @> jsonb_build_array($1::text)
		  AND NOT EXISTS (SELECT 1 FROM key_domain_assignments u WHERE u.subject_type='user' AND u.subject = l.principal)
		  AND EXISTS (SELECT 1 FROM key_domain_assignments g WHERE g.subject_type='group' AND g.subject <> $1 AND g.domain <> $2
		              AND l.groups @> jsonb_build_array(g.subject))`, group, domain).Scan(&n)
	if err != nil {
		return 0, unavailable("count the people this would make ambiguous", err)
	}
	return n, nil
}

// Changes is the assignment writes audited since a time.
type Changes struct {
	Count int
	// Latest and LatestBy describe the newest of them; the zero time when Count is 0.
	Latest   time.Time
	LatestBy string
}

// ChangesSince counts the key_domain.assignment.set and .delete audit rows
// recorded since since: the mitigation for a database writer moving where a
// person's next keys are made, which no assignment row can show once deleted.
func (s *Service) ChangesSince(ctx context.Context, since time.Time) (Changes, error) {
	var c Changes
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('key_domain.assignment.set','key_domain.assignment.delete') AND time >= $1`, since).Scan(&c.Count)
	if err != nil {
		return c, unavailable("count the assignment changes", err)
	}
	if c.Count == 0 {
		return c, nil
	}
	err = s.pool.QueryRow(ctx, `SELECT time, actor FROM audit_events WHERE action IN ('key_domain.assignment.set','key_domain.assignment.delete') AND time >= $1 ORDER BY time DESC LIMIT 1`, since).Scan(&c.Latest, &c.LatestBy)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c, unavailable("read the latest assignment change", err)
	}
	return c, nil
}

// RootKeyCredentials counts the stored credentials of people still sealed
// under the deployment's credential key (enc_version 1) rather than under their
// own principal key: what `wardynd -rewrap-principal-keys` moves.
func (s *Service) RootKeyCredentials(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE owned_by <> '' AND enc_version = 1`).Scan(&n); err != nil {
		return 0, unavailable("count the credentials under the credential key", err)
	}
	return n, nil
}
