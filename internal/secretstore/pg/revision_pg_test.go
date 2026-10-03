// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The compare-and-set a refresh's final Put runs under: it lands only while the
// row is still the one the holder read, so a holder that lost its lock cannot
// overwrite a newer pair. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

func TestPut_IfRevisionWritesOnlyWhileTheRowIsUnchanged(t *testing.T) {
	s, _, _ := newPGStore(t)
	ctx := context.Background()
	view := s.For("revision-" + uuid.NewString())
	name := "wardyn-revision-test"

	rev0, err := view.(secretstore.Revisioned).Revision(ctx, name)
	if err != nil || rev0 != "" {
		t.Fatalf("revision of an absent row = %q, %v; want empty", rev0, err)
	}
	// rev "" requires an absent row.
	if err := view.Put(secretstore.WithIfRevision(ctx, ""), name, []byte("first")); err != nil {
		t.Fatalf("guarded create of an absent row: %v", err)
	}
	if err := view.Put(secretstore.WithIfRevision(ctx, ""), name, []byte("clobber")); !errors.Is(err, secretstore.ErrRevisionChanged) {
		t.Fatalf("guarded create over an existing row = %v, want ErrRevisionChanged", err)
	}
	rev1, err := view.(secretstore.Revisioned).Revision(ctx, name)
	if err != nil || rev1 == "" {
		t.Fatalf("revision = %q, %v", rev1, err)
	}
	// A guarded replace of the row as read lands, and moves the revision.
	if err := view.Put(secretstore.WithIfRevision(ctx, rev1), name, []byte("second")); err != nil {
		t.Fatalf("guarded replace of the row as read: %v", err)
	}
	rev2, _ := view.(secretstore.Revisioned).Revision(ctx, name)
	if rev2 == rev1 {
		t.Fatal("a write did not move the revision")
	}
	// A holder still on the old read is refused and writes nothing.
	if err := view.Put(secretstore.WithIfRevision(ctx, rev1), name, []byte("stale")); !errors.Is(err, secretstore.ErrRevisionChanged) {
		t.Fatalf("guarded replace of a row that changed = %v, want ErrRevisionChanged", err)
	}
	if got := mustGet(t, view, name); got != "second" {
		t.Fatalf("stored = %q after a refused stale write, want %q", got, "second")
	}
	// A row deleted since the read is not recreated by the holder.
	if err := view.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := view.Put(secretstore.WithIfRevision(ctx, rev2), name, []byte("zombie")); !errors.Is(err, secretstore.ErrRevisionChanged) {
		t.Fatalf("guarded replace of a deleted row = %v, want ErrRevisionChanged", err)
	}
	if rev, _ := view.(secretstore.Revisioned).Revision(ctx, name); rev != "" {
		t.Fatal("a refused guarded write recreated the deleted row")
	}
	// An unguarded Put is unchanged.
	if err := view.Put(ctx, name, []byte("plain")); err != nil {
		t.Fatalf("unguarded put: %v", err)
	}
}

// Another owner's row of the same name is a different row: its writes do not
// move this view's revision.
func TestRevision_IsPerOwnerRow(t *testing.T) {
	s, _, _ := newPGStore(t)
	ctx := context.Background()
	alice, bob := s.For("alice-"+uuid.NewString()), s.For("bob-"+uuid.NewString())
	name := "wardyn-revision-owner-test"
	if err := alice.Put(ctx, name, []byte("a")); err != nil {
		t.Fatal(err)
	}
	before, _ := alice.(secretstore.Revisioned).Revision(ctx, name)
	if err := bob.Put(ctx, name, []byte("b")); err != nil {
		t.Fatal(err)
	}
	if after, _ := alice.(secretstore.Revisioned).Revision(ctx, name); after != before {
		t.Fatal("another owner's write moved this owner's revision")
	}
}

// The audited wrapper the daemon serves with forwards the revision and the
// guard: wrapping must not turn the compare-and-set off.
func TestAudited_ForwardsRevisionAndGuard(t *testing.T) {
	s, _, _ := newPGStore(t)
	ctx := context.Background()
	view := secretstore.Audited(s, &auditLog{}).For("audited-" + uuid.NewString())
	name := "wardyn-revision-audited-test"
	if err := view.Put(ctx, name, []byte("one")); err != nil {
		t.Fatal(err)
	}
	rev, ok, err := secretstore.RevisionOf(ctx, view, name)
	if err != nil || !ok || rev == "" {
		t.Fatalf("RevisionOf through Audited = %q, ok %v, %v", rev, ok, err)
	}
	if err := view.Put(ctx, name, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := view.Put(secretstore.WithIfRevision(ctx, rev), name, []byte("stale")); !errors.Is(err, secretstore.ErrRevisionChanged) {
		t.Fatalf("guarded Put through Audited = %v, want ErrRevisionChanged", err)
	}
}

func mustGet(t *testing.T, st secretstore.Store, name string) string {
	t.Helper()
	b, err := st.Get(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
