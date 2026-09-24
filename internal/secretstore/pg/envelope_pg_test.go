// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// Envelope v1 conformance against a real Postgres (credential-storage design
// §2.2 and §5's fail-closed rules): what a DATABASE WRITER can do to a row —
// move it, forge it, plant a pre-envelope one — is refused by name, and a
// refusal is never a not-found. Guarded by WARDYN_TEST_PG via newPGStore.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// assertRefused checks a Get failed closed: an error that names the row, does
// not carry the value, and is not a not-found (rule 9 — otherwise
// loadOrCreateSecret would mint a fresh boot key over the tampered row).
func assertRefused(t *testing.T, label string, got []byte, err error, owner, name, value string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: Get succeeded with %q; the row must be refused", label, got)
		return
	}
	if errors.Is(err, secretstore.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("%s: refusal surfaced as not-found: %v", label, err)
	}
	if !strings.Contains(err.Error(), rowRef(owner, name)) {
		t.Errorf("%s: error %q does not name the row %s", label, err, rowRef(owner, name))
	}
	if value != "" && strings.Contains(err.Error(), value) {
		t.Errorf("%s: error carries the value", label)
	}
}

// swapEnvelopes exchanges the sealed columns of two rows, as a database writer
// could.
func swapEnvelopes(t *testing.T, pool *pgxpool.Pool, ownerA, nameA, ownerB, nameB string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE secrets AS d SET kek_id=s.kek_id, wrapped_dek=s.wrapped_dek, ciphertext=s.ciphertext
		  FROM (SELECT owned_by, name, kek_id, wrapped_dek, ciphertext FROM secrets
		         WHERE (owned_by=$1 AND name=$2) OR (owned_by=$3 AND name=$4)) AS s
		 WHERE (d.owned_by, d.name) <> (s.owned_by, s.name)
		   AND ((d.owned_by=$1 AND d.name=$2) OR (d.owned_by=$3 AND d.name=$4))`,
		ownerA, nameA, ownerB, nameB); err != nil {
		t.Fatalf("swap rows: %v", err)
	}
}

// TestPG_SwappedRowIsRefused closes F1: a ciphertext moved between two owners,
// or between two names of one owner, no longer opens.
func TestPG_SwappedRowIsRefused(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()
	name, name2 := uniqueName("anthropic-api-key"), uniqueName("openai-api-key")
	t.Cleanup(func() {
		_ = s.For(alice).Delete(ctx, name)
		_ = s.For(alice).Delete(ctx, name2)
		_ = s.For(bob).Delete(ctx, name)
	})
	for _, p := range []struct{ owner, name, v string }{{alice, name, "alice-v"}, {alice, name2, "alice-v2"}, {bob, name, "bob-v"}} {
		if err := s.For(p.owner).Put(ctx, p.name, []byte(p.v)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	swapEnvelopes(t, pool, alice, name, bob, name)
	got, err := s.For(alice).Get(ctx, name)
	assertRefused(t, "alice's row holding bob's envelope", got, err, alice, name, "bob-v")
	got, err = s.For(bob).Get(ctx, name)
	assertRefused(t, "bob's row holding alice's envelope", got, err, bob, name, "alice-v")

	if got, err := s.For(alice).Get(ctx, name2); err != nil || string(got) != "alice-v2" {
		t.Fatalf("an untouched row = (%q, %v); the refusal must be per row", got, err)
	}
	swapEnvelopes(t, pool, alice, name2, bob, name) // bob's slot now holds alice's original
	got, err = s.For(alice).Get(ctx, name2)
	assertRefused(t, "a name holding another name's envelope", got, err, alice, name2, "")
}

// TestPG_ForgedRowIsRefused closes F2: whoever holds only public material —
// the age recipient, the kek_id on every row — cannot write a row that opens.
func TestPG_ForgedRowIsRefused(t *testing.T) {
	s, pool, id := newPGStore(t)
	ctx := context.Background()
	x := id.(*age.X25519Identity)
	recipient := x.Recipient()

	// 1. The pre-envelope forgery: an age payload to the public recipient,
	//    inserted the way an older wardynd writes (enc_version defaults to 0).
	v0Name := uniqueName("forged-v0")
	t.Cleanup(func() { _ = s.Delete(ctx, v0Name) })
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("planted-v0"))
	_ = w.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO secrets (owned_by, name, ciphertext) VALUES ('', $1, $2)`, v0Name, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, v0Name)
	assertRefused(t, "an age payload to the public recipient", got, err, "", v0Name, "planted-v0")

	// 2. A v1 row carrying the deployment's (public) kek_id but wrapped under a
	//    KEK the forger holds, and 3. one whose wrapped_dek is a bare DEK.
	forgerID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	forgerKEK, err := kek.NewLocal(forgerID)
	if err != nil {
		t.Fatal(err)
	}
	for label, forge := range map[string]func(owner, name string) ([]byte, []byte){
		"wrapped under the forger's own KEK": func(owner, name string) ([]byte, []byte) {
			w, ct, err := seal(ctx, forgerKEK, owner, name, []byte("planted-v1"))
			if err != nil {
				t.Fatal(err)
			}
			return w, ct
		},
		"an unwrapped DEK in wrapped_dek": func(owner, name string) ([]byte, []byte) {
			dek := make([]byte, kek.DEKSize)
			ct, err := kek.Seal(dek, []byte("planted-v1"), secretAAD(owner, name))
			if err != nil {
				t.Fatal(err)
			}
			return dek, ct
		},
	} {
		name := uniqueName("forged-v1")
		t.Cleanup(func() { _ = s.Delete(ctx, name) })
		wrapped, ct := forge("", name)
		if _, err := pool.Exec(ctx,
			`INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES ('', $1, 1, $2, $3, $4)`,
			name, s.kek.ID(), wrapped, ct); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, name)
		assertRefused(t, label, got, err, "", name, "planted-v1")
	}
}

// TestPG_OperatorFallbackBindingHolds: a member with no row of their own reads
// the operator's row, opened against the ROW's owner (""). Copying the
// operator's envelope into a member's slot — so the member would read it under
// their own name — is refused, and so is the reverse.
func TestPG_OperatorFallbackBindingHolds(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	name := uniqueName("anthropic-api-key")
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()
	t.Cleanup(func() {
		_ = s.Delete(ctx, name)
		_ = s.For(alice).Delete(ctx, name)
		_ = s.For(bob).Delete(ctx, name)
	})
	if err := s.Put(ctx, name, []byte("operator-v")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.For(alice).Get(ctx, name); err != nil || string(got) != "operator-v" {
		t.Fatalf("fallback read = (%q, %v), want the operator's value", got, err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext)
		SELECT $2, name, enc_version, kek_id, wrapped_dek, ciphertext FROM secrets WHERE owned_by='' AND name=$1`,
		name, alice); err != nil {
		t.Fatal(err)
	}
	got, err := s.For(alice).Get(ctx, name)
	assertRefused(t, "the operator's envelope copied into a member's slot", got, err, alice, name, "operator-v")

	if err := s.For(bob).Put(ctx, name, []byte("bob-v")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE secrets SET kek_id=b.kek_id, wrapped_dek=b.wrapped_dek, ciphertext=b.ciphertext
		  FROM (SELECT kek_id, wrapped_dek, ciphertext FROM secrets WHERE owned_by=$2 AND name=$1) AS b
		 WHERE secrets.owned_by='' AND secrets.name=$1`, name, bob); err != nil {
		t.Fatal(err)
	}
	carol := "carol-" + uuid.NewString()
	got, err = s.For(carol).Get(ctx, name)
	assertRefused(t, "a member's envelope copied into the operator's slot, read by fallback", got, err, "", name, "bob-v")
}

// failingKEK refuses every wrap: a KMS that is down or denies the call.
type failingKEK struct{ kek.KEK }

func (failingKEK) Wrap(context.Context, []byte, map[string]string) ([]byte, error) {
	return nil, errors.New("key service unavailable")
}

// TestPG_PutWhoseWrapFailsWritesNoRow is rule 12: a Put that cannot wrap its
// DEK writes nothing — no new row, and an existing row keeps its old envelope.
func TestPG_PutWhoseWrapFailsWritesNoRow(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	broken := &Store{pool: pool, kek: failingKEK{s.kek}}

	fresh := uniqueName("never-written")
	if err := broken.Put(ctx, fresh, []byte("v")); err == nil {
		t.Fatal("Put succeeded with a failing KEK")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE name=$1`, fresh).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows for %s after a failed wrap = %d (%v), want 0", fresh, n, err)
	}

	existing := uniqueName("kept")
	t.Cleanup(func() { _ = s.Delete(ctx, existing) })
	if err := s.Put(ctx, existing, []byte("old-v")); err != nil {
		t.Fatal(err)
	}
	if err := broken.Put(ctx, existing, []byte("new-v")); err == nil {
		t.Fatal("replace succeeded with a failing KEK")
	}
	if got, err := s.Get(ctx, existing); err != nil || string(got) != "old-v" {
		t.Fatalf("after a failed replace = (%q, %v), want the old value intact", got, err)
	}
}

// TestPG_NoncesNeverRepeatAcrossWrites reads every nonce the store actually
// persisted — the value's and the wrap's, across replaces of one row and writes
// of many — and fails on any repeat.
func TestPG_NoncesNeverRepeatAcrossWrites(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	seen := map[string]string{}
	record := func(name, where string) {
		var wrapped, ct []byte
		if err := pool.QueryRow(ctx, `SELECT wrapped_dek, ciphertext FROM secrets WHERE owned_by='' AND name=$1`, name).
			Scan(&wrapped, &ct); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{string(wrapped[:12]), string(ct[:12])} {
			if prev, dup := seen[n]; dup {
				t.Fatalf("nonce %x repeated: %s and %s", n, prev, where)
			}
			seen[n] = where
		}
	}
	same := uniqueName("replaced")
	t.Cleanup(func() { _ = s.Delete(ctx, same) })
	for i := 0; i < 100; i++ {
		if err := s.Put(ctx, same, []byte("same value every time")); err != nil {
			t.Fatal(err)
		}
		record(same, "replace")
	}
	for i := 0; i < 100; i++ {
		n := uniqueName("many")
		t.Cleanup(func() { _ = s.Delete(ctx, n) })
		if err := s.Put(ctx, n, []byte("same value every time")); err != nil {
			t.Fatal(err)
		}
		record(n, n)
	}
	if len(seen) != 400 {
		t.Fatalf("recorded %d nonces, want 400", len(seen))
	}
}

// TestPG_V0RowAfterConversionIsRefused is rule 6: a pre-envelope row that
// appears after conversion — an older wardynd still writing — is refused by
// name, never read as age.
func TestPG_V0RowAfterConversionIsRefused(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	name := uniqueName("late-v0")
	t.Cleanup(func() { _ = s.Delete(ctx, name) })
	if _, err := pool.Exec(ctx, `INSERT INTO secrets (owned_by, name, ciphertext) VALUES ('', $1, '\x00'::bytea)`, name); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, name)
	assertRefused(t, "a late v0 row", got, err, "", name, "")
	if err != nil && !strings.Contains(err.Error(), "an older wardynd is still writing") {
		t.Errorf("error %q does not name the cause", err)
	}
}
