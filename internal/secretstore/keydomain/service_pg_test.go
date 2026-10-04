// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keydomain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

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
