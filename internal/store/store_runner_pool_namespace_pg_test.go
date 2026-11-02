// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func claimedOrgRunner(t *testing.T, pg store.PG) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	rn, err := pg.CreateRunner(ctx, newOrgRunner())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ClaimOrgRunner(ctx, rn.ID, rn.KeyFingerprint, "org", "sec-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	return rn.ID
}

func orgPool(t *testing.T, pg store.PG, name string) uuid.UUID {
	t.Helper()
	p, err := pg.CreateRunnerPool(context.Background(), types.RunnerPool{ID: uuid.New(), Name: name, Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// An org runner's one-pool rule is per runs namespace: the default namespace ("") overlaps every other,
// the same named namespace overlaps itself, and two different named namespaces sit in two pools.
func TestPG_OrgRunnerNamespaceUniquenessMatrix(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	p1, p2, p3 := orgPool(t, pg, "One"), orgPool(t, pg, "Two"), orgPool(t, pg, "Three")
	add := func(pool, runner uuid.UUID, ns string) (types.RunnerPoolMember, bool, error) {
		t.Helper()
		m, added, _, err := pg.AddOrgRunnerToPool(ctx, pool, runner, "org", ns)
		return m, added, err
	}

	named := claimedOrgRunner(t, pg)
	if m, added, err := add(p1, named, "team-a"); err != nil || !added || m.RunsNamespace != "team-a" {
		t.Fatalf("add in team-a = %+v, %v, %v", m, added, err)
	}
	if m, added, err := add(p2, named, "team-b"); err != nil || !added || m.RunsNamespace != "team-b" {
		t.Fatalf("add in a different namespace to another pool = %+v, %v, %v", m, added, err)
	}
	if _, _, err := add(p3, named, "team-a"); !errors.Is(err, store.ErrRunnerNamespaceTaken) {
		t.Errorf("the same namespace for a third pool = %v, want ErrRunnerNamespaceTaken", err)
	}
	if _, _, err := add(p3, named, ""); !errors.Is(err, store.ErrRunnerInOtherPool) {
		t.Errorf("the default namespace beside named ones = %v, want ErrRunnerInOtherPool", err)
	}
	if _, _, err := add(p3, named, "team-c"); err != nil {
		t.Errorf("a third distinct namespace = %v, want it added", err)
	}

	// A re-add is idempotent for the same namespace and refused for another: it is set at the add.
	if m, added, err := add(p1, named, "team-a"); err != nil || added || m.RunsNamespace != "team-a" {
		t.Errorf("a repeated add = %+v, %v, %v, want the existing member", m, added, err)
	}
	for _, ns := range []string{"team-z", ""} {
		if m, _, err := add(p1, named, ns); !errors.Is(err, store.ErrRunnerNamespaceFixed) || m.RunsNamespace != "team-a" {
			t.Errorf("a re-add in %q = %+v, %v, want ErrRunnerNamespaceFixed naming team-a", ns, m, err)
		}
	}

	// The members read answers the namespace.
	members, err := pg.ListRunnerPoolMembers(ctx, p2)
	if err != nil || len(members) != 1 || members[0].RunsNamespace != "team-b" {
		t.Errorf("members of Two = %+v, %v, want team-b", members, err)
	}

	// Removing the membership frees the namespace.
	if _, _, err := pg.RemoveOrgRunnerFromPool(ctx, p1, named, false); err != nil {
		t.Fatal(err)
	}
	if _, added, err := add(orgPool(t, pg, "Four"), named, "team-a"); err != nil || !added {
		t.Errorf("a freed namespace = %v, %v, want it added", added, err)
	}

	// A runner in the default namespace keeps today's rule: one pool, whatever the next namespace is.
	plain := claimedOrgRunner(t, pg)
	if _, added, err := add(p1, plain, ""); err != nil || !added {
		t.Fatalf("add in the default namespace = %v, %v", added, err)
	}
	for _, ns := range []string{"", "team-a"} {
		if _, _, err := add(p2, plain, ns); !errors.Is(err, store.ErrRunnerInOtherPool) {
			t.Errorf("a second pool in %q beside the default = %v, want ErrRunnerInOtherPool", ns, err)
		}
	}
}

// The column refuses a value that is not a namespace name, whatever the writer above it checked.
func TestPG_OrgRunnerNamespaceColumnRefusesANonName(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	pool, runner := orgPool(t, pg, "One"), claimedOrgRunner(t, pg)
	for _, ns := range []string{"Team_A", "-a", "a-", "has space"} {
		if _, _, _, err := pg.AddOrgRunnerToPool(context.Background(), pool, runner, "org", ns); err == nil {
			t.Errorf("namespace %q was stored", ns)
		}
	}
}
