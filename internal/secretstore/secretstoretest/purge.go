// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstoretest

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// RunPurgeConformance holds a store to the two deletes that span rows: rule
// 8's purge of one model provider's credentials in every namespace
// (DeleteEverywhere over its wardyn-provider-<uid>-* names) and CS-5's erase
// of one person's namespace (EraseOwner). drift, when not nil, is how many
// values the backend holds that no row points to plus rows whose value is gone
// — store mode's `wardynd -reconcile` — and must not grow across either: a delete
// that drops the row but leaves the value in the organisation's Vault or Key
// Vault tells the console a credential is gone while it is not.
func RunPurgeConformance(t *testing.T, newStore func(t *testing.T) secretstore.Store, drift func(t *testing.T) int) {
	// noDrift takes the drift before a case and returns the check that it did
	// not grow, so one case's leftovers never fail the next.
	noDrift := func(t *testing.T) func() {
		if drift == nil {
			return func() {}
		}
		before := drift(t)
		return func() {
			t.Helper()
			if n := drift(t); n != before {
				t.Fatalf("-reconcile reports %d orphaned or dangling values after the delete, want %d as before it", n, before)
			}
		}
	}
	t.Run("purge_by_uid_reaches_every_namespace_and_only_that_uid", func(t *testing.T) {
		op := newStore(t)
		check := noDrift(t)
		testPurgeByUID(t, op)
		check()
	})
	t.Run("erase_by_owner_leaves_no_value_behind", func(t *testing.T) {
		op := newStore(t)
		check := noDrift(t)
		testEraseByOwner(t, op)
		check()
	})
}

func providerName(uid, part string) string { return "wardyn-provider-" + uid + "-" + part }

// testPurgeByUID: DeleteEverywhere over one UID's names, called on one
// person's view, removes them from the operator's and every person's
// namespace, and leaves another UID's credentials and an operator secret.
func testPurgeByUID(t *testing.T, op secretstore.Store) {
	ctx := context.Background()
	uid, keep := uuid.NewString(), uuid.NewString()
	a, b := "owner-a-"+uuid.NewString(), "owner-b-"+uuid.NewString()
	purged := []string{providerName(uid, "key"), providerName(uid, "oauth"), providerName(uid, "sso")}
	kept := map[string]string{a: providerName(keep, "key"), b: providerName(keep, "sso"), "": "conformance-operator-" + uuid.NewString()}
	t.Cleanup(func() {
		for owner, name := range kept {
			_ = op.For(owner).Delete(ctx, name)
		}
	})
	seed := map[string][]string{"": {purged[0]}, a: purged, b: {purged[0], purged[2]}}
	for owner, names := range seed {
		for _, n := range names {
			mustPut(t, op, owner, n, "v-"+owner)
		}
	}
	for owner, n := range kept {
		mustPut(t, op, owner, n, "kept-"+owner)
	}

	got, err := op.For(a).DeleteEverywhere(ctx, purged)
	if err != nil || got != 6 {
		t.Fatalf("DeleteEverywhere(uid) = (%d, %v), want 6 rows removed", got, err)
	}
	for owner, names := range seed {
		left, err := op.For(owner).List(ctx)
		if err != nil || slices.ContainsFunc(names, func(n string) bool { return slices.Contains(left, n) }) {
			t.Fatalf("namespace %q after the purge lists %v (%v), want none of %v", owner, left, err, names)
		}
	}
	for owner, n := range kept {
		if v, err := op.For(owner).Get(ctx, n); err != nil || string(v) != "kept-"+owner {
			t.Fatalf("%q/%q after purging another UID = (%q, %v), want it untouched", owner, n, v, err)
		}
	}
}

// testEraseByOwner: EraseOwner removes one person's provider credential and
// PAT, and leaves the same names in the operator's and another person's
// namespace.
func testEraseByOwner(t *testing.T, op secretstore.Store) {
	ctx := context.Background()
	a, b := "owner-a-"+uuid.NewString(), "owner-b-"+uuid.NewString()
	names := []string{providerName(uuid.NewString(), "key"), "conformance-pat-" + uuid.NewString()}
	t.Cleanup(func() {
		for _, n := range names {
			_ = op.Delete(ctx, n)
			_ = op.For(b).Delete(ctx, n)
		}
	})
	for _, owner := range []string{"", a, b} {
		for _, n := range names {
			mustPut(t, op, owner, n, "v-"+owner)
		}
	}

	rep, err := secretstore.EraseOwner(ctx, op, a)
	if err != nil || rep.Count != len(names) {
		t.Fatalf("EraseOwner(a) = (%+v, %v), want %d erased", rep, err, len(names))
	}
	if left, err := op.For(a).List(ctx); err != nil || len(left) != 0 {
		t.Fatalf("A after the erase lists %v (%v), want nothing", left, err)
	}
	for _, owner := range []string{"", b} {
		for _, n := range names {
			if v, err := op.For(owner).Get(ctx, n); err != nil || string(v) != "v-"+owner {
				t.Fatalf("%q/%q after erasing A = (%q, %v), want it untouched", owner, n, v, err)
			}
		}
	}
}

func mustPut(t *testing.T, st secretstore.Store, owner, name, value string) {
	t.Helper()
	if err := st.For(owner).Put(context.Background(), name, []byte(value)); err != nil {
		t.Fatalf("seed %q/%q: %v", owner, name, err)
	}
}
