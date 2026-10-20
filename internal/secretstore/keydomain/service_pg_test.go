// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keydomain_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

func set(t *testing.T, s *keydomain.Service, typ, subject, domain string) {
	t.Helper()
	if _, err := s.Set(t.Context(), keydomain.Assignment{SubjectType: typ, Subject: subject, Domain: domain, SetBy: "admin"}); err != nil {
		t.Fatalf("Set(%s %s -> %s): %v", typ, subject, domain, err)
	}
}

func domainOf(t *testing.T, s *keydomain.Service, owner string) string {
	t.Helper()
	d, err := s.Domain(t.Context(), owner)
	if err != nil {
		t.Fatalf("Domain(%s): %v", owner, err)
	}
	return d
}

func login(t *testing.T, s *keydomain.Service, owner string, truncated bool, groups ...string) {
	t.Helper()
	if err := s.RecordLoginGroups(t.Context(), owner, groups, truncated); err != nil {
		t.Fatal(err)
	}
}

// Resolution is user > group > all > default, and an assignment to a domain the
// file does not declare is refused.
func TestService_ResolutionOrder(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a", "b", "c", "d"})

	if got := domainOf(t, s, "nobody"); got != keydomain.Default {
		t.Fatalf("no assignment: domain = %q, want default", got)
	}
	set(t, s, "all", "", "a")
	if got := domainOf(t, s, "nobody"); got != "a" {
		t.Fatalf("an all assignment alone: domain = %q, want a", got)
	}
	login(t, s, "gina", false, "eng", "staff")
	set(t, s, "group", "eng", "b")
	if got := domainOf(t, s, "gina"); got != "b" {
		t.Fatalf("a group beats all: domain = %q, want b", got)
	}
	if got := domainOf(t, s, "nobody"); got != "a" {
		t.Fatalf("a person in no assigned group: domain = %q, want a (all)", got)
	}
	set(t, s, "user", "gina", "c")
	if got := domainOf(t, s, "gina"); got != "c" {
		t.Fatalf("a user beats a group: domain = %q, want c", got)
	}
	// "default" is declared: it sends a user back to the credential key,
	// beating the group and all.
	set(t, s, "user", "gina", keydomain.Default)
	if got := domainOf(t, s, "gina"); got != keydomain.Default {
		t.Fatalf("a user assigned to default: domain = %q, want default", got)
	}
	if _, _, err := s.Delete(t.Context(), "user", "gina"); err != nil {
		t.Fatal(err)
	}
	if got := domainOf(t, s, "gina"); got != "b" {
		t.Fatalf("after the user assignment is removed: domain = %q, want b (group)", got)
	}

	if _, err := s.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: "x", Domain: "ghost", SetBy: "admin"}); !errors.Is(err, keydomain.ErrUnknownDomain) {
		t.Fatalf("Set to an undeclared domain = %v, want ErrUnknownDomain", err)
	}
}

// Two groups assigned to different domains refuse by name; the same domain does
// not; a user assignment settles it.
func TestService_AmbiguousGroups(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a", "b"})
	login(t, s, "gina", false, "eng", "ops")
	set(t, s, "group", "eng", "a")
	set(t, s, "group", "ops", "a")
	if got := domainOf(t, s, "gina"); got != "a" {
		t.Fatalf("two groups, one domain: %q, want a", got)
	}
	set(t, s, "group", "ops", "b")
	_, err := s.Domain(t.Context(), "gina")
	if !errors.Is(err, keydomain.ErrAmbiguous) || !strings.Contains(err.Error(), "eng") || !strings.Contains(err.Error(), "ops") {
		t.Fatalf("two groups, two domains = %v; want ErrAmbiguous naming both groups", err)
	}
	set(t, s, "user", "gina", "b")
	if got := domainOf(t, s, "gina"); got != "b" {
		t.Fatalf("a user assignment settles it: %q, want b", got)
	}
}

// The API's guard counts who a group assignment would leave ambiguous.
func TestService_AmbiguousIfGroup(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a", "b"})
	login(t, s, "gina", false, "eng", "ops")
	login(t, s, "hal", false, "eng", "ops")
	login(t, s, "ivy", false, "eng")
	set(t, s, "group", "eng", "a")
	set(t, s, "user", "hal", "a")
	n, err := s.AmbiguousIfGroup(t.Context(), "ops", "b")
	if err != nil || n != 1 {
		t.Fatalf("AmbiguousIfGroup(ops -> b) = (%d, %v); want 1: gina (hal has a user assignment, ivy is not in ops)", n, err)
	}
	if n, err := s.AmbiguousIfGroup(t.Context(), "ops", "a"); err != nil || n != 0 {
		t.Fatalf("AmbiguousIfGroup(ops -> a) = (%d, %v); want 0, the same domain", n, err)
	}
}

// A user assignment whose subject spells a group name is a person, not that group: it grants the
// group's members nothing, and it is not a second group domain for them.
func TestService_UserAssignmentNamedLikeAGroupIsNotTheGroup(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a", "b"})
	login(t, s, "gina", false, "eng")
	set(t, s, "user", "eng", "a")
	if got := domainOf(t, s, "gina"); got != keydomain.Default {
		t.Fatalf("a user named eng assigned to a: gina in group eng is in %q, want default", got)
	}
	set(t, s, "group", "eng", "b")
	if got := domainOf(t, s, "gina"); got != "b" {
		t.Fatalf("group eng assigned to b: gina is in %q, want b", got)
	}
	if two, err := s.InTwoGroupDomains(t.Context(), "gina"); err != nil || two {
		t.Fatalf("InTwoGroupDomains = (%v, %v); want false: the user named eng is not a group domain", two, err)
	}
}

// The refusal guards read group assignments only: a user assignment whose subject spells a group
// name is neither a second group domain for the people in that group nor a group assignment that
// makes a truncated login unplaceable.
func TestService_GuardsIgnoreUserAssignmentsNamedLikeGroups(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a", "b"})
	login(t, s, "gina", false, "eng", "ops")
	login(t, s, "hal", true, "eng")
	set(t, s, "user", "ops", "b")
	if n, err := s.AmbiguousIfGroup(t.Context(), "eng", "a"); err != nil || n != 0 {
		t.Fatalf("AmbiguousIfGroup = (%d, %v); want 0: the user named ops is not a group assigned to another domain", n, err)
	}
	if yes, err := s.TruncatedWithGroups(t.Context(), "hal"); err != nil || yes {
		t.Fatalf("TruncatedWithGroups = (%v, %v); want false: no group is assigned, only a user", yes, err)
	}
}

// The API's guard reads only group assignments: a user assignment whose subject is spelled like one of
// the person's groups is not a second group domain.
func TestService_InTwoGroupDomains(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a", "b"})
	login(t, s, "gina", false, "eng", "ops")
	set(t, s, "group", "eng", "a")
	set(t, s, "user", "ops", "b")
	if two, err := s.InTwoGroupDomains(t.Context(), "gina"); err != nil || two {
		t.Fatalf("one group domain and a user assignment spelled like her other group = (%v, %v); want false", two, err)
	}
	set(t, s, "group", "ops", "b")
	if two, err := s.InTwoGroupDomains(t.Context(), "gina"); err != nil || !two {
		t.Fatalf("two group domains = (%v, %v); want true", two, err)
	}
}

// The API's guard counts who a group assignment would lock out: people whose last login lost
// groups and who have no user assignment.
func TestService_TruncatedUnassigned(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a"})
	login(t, s, "gina", true, "eng")
	login(t, s, "hal", true, "eng")
	login(t, s, "ivy", false, "eng")
	set(t, s, "user", "hal", "a")
	n, _, err := s.TruncatedUnassigned(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("TruncatedUnassigned = (%d, %v); want 1: gina (hal has a user assignment, ivy's login was complete)", n, err)
	}
}

// A login that lost groups cannot place a person while group assignments exist.
func TestService_TruncatedGroups(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a"})
	login(t, s, "gina", true, "eng")
	if got := domainOf(t, s, "gina"); got != keydomain.Default {
		t.Fatalf("truncated, no group assignments: %q, want default", got)
	}
	set(t, s, "group", "other", "a")
	if _, err := s.Domain(t.Context(), "gina"); !errors.Is(err, keydomain.ErrGroupsTruncated) {
		t.Fatalf("truncated beside a group assignment = %v, want ErrGroupsTruncated", err)
	} else if strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("the refusal %q offers a remedy an overage login never delivers", err)
	}
	set(t, s, "user", "gina", "a")
	if got := domainOf(t, s, "gina"); got != "a" {
		t.Fatalf("a user assignment needs no groups: %q, want a", got)
	}
	// A fresh complete login clears it.
	login(t, s, "hal", true, "x")
	login(t, s, "hal", false, "x")
	if got := domainOf(t, s, "hal"); got != keydomain.Default {
		t.Fatalf("after a complete login: %q, want default", got)
	}
}

// oldPlace is Place as it was before PlaceMany took it over (410aeb773), kept to hold PlaceMany to what Place did.
func oldPlace(ctx context.Context, pool *pgxpool.Pool, s *keydomain.Service, owner string) (keydomain.Placement, error) {
	var groups []string
	var truncated bool
	var raw []byte
	err := pool.QueryRow(ctx, `SELECT groups, truncated FROM key_domain_login_groups WHERE principal=$1`, owner).Scan(&raw, &truncated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return keydomain.Placement{}, oldUnavailable("read the login's groups", err)
	default:
		if err := json.Unmarshal(raw, &groups); err != nil {
			return keydomain.Placement{}, fmt.Errorf("keydomain: the stored groups of %q are not a list: %w", owner, err)
		}
	}
	rows, err := pool.Query(ctx, `
		SELECT subject_type, subject, domain FROM key_domain_assignments
		WHERE (subject_type='user' AND subject=$1) OR subject_type='all' OR (subject_type='group' AND subject = ANY($2))`,
		owner, groups)
	if err != nil {
		return keydomain.Placement{}, oldUnavailable("read the assignments", err)
	}
	hits, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Type, Subject, Domain string }])
	if err != nil {
		return keydomain.Placement{}, oldUnavailable("read the assignments", err)
	}
	var user, all string
	byGroup := map[string][]string{} // domain -> the groups naming it
	for _, h := range hits {
		switch h.Type {
		case keydomain.SubjectUser:
			user = h.Domain
		case keydomain.SubjectAll:
			all = h.Domain
		case keydomain.SubjectGroup:
			byGroup[h.Domain] = append(byGroup[h.Domain], h.Subject)
		}
	}
	place := keydomain.Placement{Domain: keydomain.Default, Source: keydomain.SourceDefault}
	switch {
	case user != "":
		place = keydomain.Placement{Domain: user, Source: keydomain.SourceUser}
	case len(byGroup) > 1:
		var parts []string
		for d, gs := range byGroup {
			slices.Sort(gs)
			parts = append(parts, fmt.Sprintf("%s (%s)", d, strings.Join(gs, ", ")))
		}
		slices.Sort(parts)
		return keydomain.Placement{}, fmt.Errorf("%w: %q is in groups assigned to %s; assign the person to one domain as a user", keydomain.ErrAmbiguous, owner, strings.Join(parts, " and "))
	case len(byGroup) == 1:
		for d, gs := range byGroup {
			slices.Sort(gs)
			place = keydomain.Placement{Domain: d, Source: keydomain.SourceGroup, Group: gs[0]}
		}
	case all != "":
		place = keydomain.Placement{Domain: all, Source: keydomain.SourceAll}
	}
	if truncated && user == "" {
		var any bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM key_domain_assignments WHERE subject_type='group')`).Scan(&any); err != nil {
			return keydomain.Placement{}, oldUnavailable("read the assignments", err)
		}
		if any {
			return keydomain.Placement{}, fmt.Errorf("%w: the last sign-in of %q lost groups; assign the person to a domain as a user", keydomain.ErrGroupsTruncated, owner)
		}
	}
	if !s.Has(place.Domain) {
		return keydomain.Placement{}, fmt.Errorf("%w: %q is assigned to %q", keydomain.ErrUnknownDomain, owner, place.Domain)
	}
	return place, nil
}

func oldUnavailable(op string, err error) error {
	return fmt.Errorf("keydomain: %s: %w: %w", op, secretstore.ErrUnavailable, err)
}

// PlaceMany places a corpus of people in one call exactly as the one-person Place did before it:
// the same domain and source, and the same refusals.
func TestService_PlaceManyMatchesPlace(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a", "b", "c"})
	login(t, s, "gina", false, "eng", "staff")
	login(t, s, "hal", false, "eng", "ops")
	login(t, s, "ivy", false, "ops")
	login(t, s, "jon", true, "eng")
	login(t, s, "kim", true, "other")
	set(t, s, "all", "", "c")
	set(t, s, "group", "eng", "a")
	set(t, s, "group", "ops", "b")
	set(t, s, "user", "ivy", "c")
	set(t, s, "user", "kim", "a")
	people := []string{"gina", "hal", "ivy", "jon", "kim", "nobody", "gina"}
	got := s.PlaceMany(t.Context(), people)
	for _, p := range people {
		want, werr := oldPlace(t.Context(), pool, s, p)
		if got[p].Placement != want || (got[p].Err == nil) != (werr == nil) || (werr != nil && got[p].Err.Error() != werr.Error()) {
			t.Errorf("PlaceMany[%s] = (%+v, %v), oldPlace = (%+v, %v)", p, got[p].Placement, got[p].Err, want, werr)
		}
	}
	if got["gina"].Domain != "a" || got["gina"].Source != keydomain.SourceGroup || got["gina"].Group != "eng" {
		t.Errorf("gina = %+v, want group eng -> a", got["gina"])
	}
	if !errors.Is(got["hal"].Err, keydomain.ErrAmbiguous) || !errors.Is(got["jon"].Err, keydomain.ErrGroupsTruncated) {
		t.Errorf("hal = %v, jon = %v; want ErrAmbiguous and ErrGroupsTruncated", got["hal"].Err, got["jon"].Err)
	}
	if got["ivy"].Source != keydomain.SourceUser || got["kim"].Domain != "a" || got["nobody"].Source != keydomain.SourceAll {
		t.Errorf("ivy = %+v, kim = %+v, nobody = %+v", got["ivy"], got["kim"], got["nobody"])
	}
}

// An assignment naming a domain the file no longer declares fails closed.
func TestService_UndeclaredDomainFailsClosed(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	before := keydomain.NewService(pool, []string{"a"})
	set(t, before, "user", "gina", "a")
	after := keydomain.NewService(pool, nil)
	if _, err := after.Domain(t.Context(), "gina"); !errors.Is(err, keydomain.ErrUnknownDomain) {
		t.Fatalf("Domain after the file lost the domain = %v, want ErrUnknownDomain", err)
	}
}

// Undeclared lists each domain the assignments name that the file lost, with
// how many assignments name it; a declared domain and the default are not listed.
func TestService_UndeclaredListsTheAssignmentsTheFileLost(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	before := keydomain.NewService(pool, []string{"a", "b"})
	set(t, before, "user", "gina", "a")
	set(t, before, "user", "hal", "a")
	set(t, before, "group", "eng", "b")
	set(t, before, "user", "ivy", keydomain.Default)
	after := keydomain.NewService(pool, []string{"b"})
	got, err := after.Undeclared(t.Context())
	if want := []keydomain.UndeclaredDomain{{Domain: "a", Assignments: 2}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("Undeclared = (%v, %v), want %v", got, err, want)
	}
}

func TestService_ListUsageDelete(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"b", "a"})
	if got := s.Declared(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Declared = %v", got)
	}
	created, err := s.Set(t.Context(), keydomain.Assignment{SubjectType: "group", Subject: "eng", Domain: "a", SetBy: "x"})
	if err != nil || !created {
		t.Fatalf("first Set = (%v, %v), want created", created, err)
	}
	if created, err = s.Set(t.Context(), keydomain.Assignment{SubjectType: "group", Subject: "eng", Domain: "b", SetBy: "y"}); err != nil || created {
		t.Fatalf("second Set = (%v, %v), want an update", created, err)
	}
	got, ok, err := s.Get(t.Context(), "group", "eng")
	if err != nil || !ok || got.Domain != "b" || got.SetBy != "y" {
		t.Fatalf("Get = (%+v, %v, %v)", got, ok, err)
	}
	list, err := s.List(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("List = (%v, %v)", list, err)
	}
	usage, err := s.Usage(t.Context())
	if err != nil || len(usage) != 3 || usage[0].Domain != keydomain.Default || !usage[0].Declared {
		t.Fatalf("Usage = (%+v, %v); want default, a, b", usage, err)
	}
	prev, found, err := s.Delete(t.Context(), "group", "eng")
	if err != nil || !found || prev.Domain != "b" {
		t.Fatalf("Delete = (%+v, %v, %v)", prev, found, err)
	}
	if _, found, _ := s.Delete(t.Context(), "group", "eng"); found {
		t.Fatal("a second Delete found a row")
	}
}

// Recording a login's groups serializes with every assignment write: a write that holds the lock
// counted the people it would make ambiguous from the groups as they were.
func TestRecordLoginGroups_WaitsForAnAssignmentWrite(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	s := keydomain.NewService(pool, []string{"a"})
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if err := keydomain.LockAssignments(ctx, tx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.RecordLoginGroups(ctx, "gina", []string{"eng"}, false) }()
	select {
	case err := <-done:
		t.Fatalf("RecordLoginGroups returned (%v) while an assignment write held the lock", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RecordLoginGroups after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecordLoginGroups did not return after the lock was released")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM key_domain_login_groups WHERE principal = 'gina' AND groups = '["eng"]'::jsonb`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the login's groups were not written: %d rows, %v", n, err)
	}
}
