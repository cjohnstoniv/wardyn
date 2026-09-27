// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func lastUsedAt(t *testing.T, pool *pgxpool.Pool, owner, name string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT last_used_at FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&at); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	return at
}

// TestPG_MarkUsedWritesAtMostOnceAMinute: the first use stamps the row, a use
// inside the minute writes nothing, and a use after it stamps again. RED with
// the window dropped from MarkUsed's WHERE: the second use moves the stamp.
func TestPG_MarkUsedWritesAtMostOnceAMinute(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	alice := "alice-" + uuid.NewString()
	name := uniqueName("wardyn-provider-meta-key")
	st := s.For(alice)
	t.Cleanup(func() { _ = st.Delete(ctx, name) })
	if err := st.Put(ctx, name, []byte("sk-alice-meta-0123456789")); err != nil {
		t.Fatal(err)
	}
	if at := lastUsedAt(t, pool, alice, name); at != nil {
		t.Fatalf("a Put stamped last_used_at = %v; only a use may", at)
	}
	if err := s.For(alice).(*Store).MarkUsed(ctx, name); err != nil {
		t.Fatal(err)
	}
	first := lastUsedAt(t, pool, alice, name)
	if first == nil {
		t.Fatal("the first use did not stamp last_used_at")
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.For(alice).(*Store).MarkUsed(ctx, name); err != nil {
		t.Fatal(err)
	}
	if again := lastUsedAt(t, pool, alice, name); again == nil || !again.Equal(*first) {
		t.Fatalf("a use inside the minute re-wrote last_used_at: %v -> %v", first, again)
	}
	if _, err := pool.Exec(ctx, `UPDATE secrets SET last_used_at = last_used_at - interval '61 seconds'
		WHERE owned_by=$1 AND name=$2`, alice, name); err != nil {
		t.Fatal(err)
	}
	aged := lastUsedAt(t, pool, alice, name)
	if err := s.For(alice).(*Store).MarkUsed(ctx, name); err != nil {
		t.Fatal(err)
	}
	if later := lastUsedAt(t, pool, alice, name); later == nil || !later.After(*aged) {
		t.Fatalf("a use a minute later did not stamp again: %v -> %v", aged, later)
	}
	// Another view's use of the same name touches only its own (absent) row.
	stamped := lastUsedAt(t, pool, alice, name)
	if err := s.For("bob-"+uuid.NewString()).(*Store).MarkUsed(ctx, name); err != nil {
		t.Fatal(err)
	}
	if got := lastUsedAt(t, pool, alice, name); !got.Equal(*stamped) {
		t.Fatalf("bob's use stamped alice's row: %v -> %v", stamped, got)
	}
}

// TestPG_MetadataScopesAndCarriesNoValue: a view's Metadata is its own row
// only; MetadataEverywhere is every person's, never the operator's; neither
// carries the value; replacing a value keeps when it was added.
func TestPG_MetadataScopesAndCarriesNoValue(t *testing.T) {
	s, _, _ := newPGStore(t)
	ctx := context.Background()
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()
	name := uniqueName("wardyn-provider-meta-key")
	values := map[string]string{"": "sk-operator-meta-0123456789", alice: "sk-alice-meta-0123456789", bob: "sk-bob-meta-0123456789"}
	for owner, v := range values {
		t.Cleanup(func() { _ = s.For(owner).Delete(ctx, name) })
		if err := s.For(owner).Put(ctx, name, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}

	own, err := s.For(alice).(*Store).Metadata(ctx, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || own[0].Owner != alice || own[0].Store != "pg" || own[0].AddedAt.IsZero() || own[0].LastUsedAt != nil {
		t.Fatalf("alice's Metadata = %+v, want her one row only, stored in pg, never used", own)
	}
	all, err := s.For(bob).(*Store).MetadataEverywhere(ctx, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Owner == "" || all[1].Owner == "" {
		t.Fatalf("MetadataEverywhere = %+v, want alice's and bob's rows and never the operator's", all)
	}
	if dump := fmt.Sprintf("%+v %+v", own, all); strings.Contains(dump, "sk-") {
		t.Fatalf("metadata carries a value: %s", dump)
	}

	if err := s.For(alice).Put(ctx, name, []byte("sk-alice-meta-replaced-0123")); err != nil {
		t.Fatal(err)
	}
	again, err := s.For(alice).(*Store).Metadata(ctx, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || !again[0].AddedAt.Equal(own[0].AddedAt) {
		t.Fatalf("replacing the value moved added_at: %v -> %+v", own[0].AddedAt, again)
	}
}
