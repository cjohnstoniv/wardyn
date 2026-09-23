// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The boot conversion of legacy (v0, age) rows to envelope v1 — rule 11:
// single-writer, all-or-nothing, aborting on an undecryptable row. On a
// throwaway database (rekeyDatabase): ConvertV0 rewrites EVERY v0 row, so on
// the shared one it would trip over sibling packages' fixture rows.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// seedV0 writes a row the way every wardynd before envelope v1 did: an age
// payload to the recipient, with the envelope columns left at their defaults.
func seedV0(t *testing.T, pool *pgxpool.Pool, to *age.X25519Identity, owner, name, value string) {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, to.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(value)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO secrets (owned_by, name, ciphertext) VALUES ($1, $2, $3)`, owner, name, buf.Bytes()); err != nil {
		t.Fatalf("seed v0 row: %v", err)
	}
}

type rawRow struct {
	version     int16
	kekID       string
	wrapped, ct []byte
}

func rawRows(t *testing.T, pool *pgxpool.Pool) map[string]rawRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext FROM secrets`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]rawRow{}
	for rows.Next() {
		var owner, name string
		var r rawRow
		if err := rows.Scan(&owner, &name, &r.version, &r.kekID, &r.wrapped, &r.ct); err != nil {
			t.Fatal(err)
		}
		out[rowRef(owner, name)] = r
	}
	return out
}

var v0Fixture = []struct{ owner, name, value string }{
	{"", "wardyn-signing-key", "-----BEGIN EC PRIVATE KEY-----\nfixture\n-----END EC PRIVATE KEY-----\n"},
	{"", "anthropic-api-key", "sk-ant-operator"},
	{"alice@corp.example", "anthropic-api-key", "sk-ant-alice"},
}

// TestPG_ConvertV0_ConvertsEveryRowOnceThenIsANoOp seeds v0 rows with the real
// age code, converts, reads each back as v1, and proves a second boot's
// conversion touches nothing.
func TestPG_ConvertV0_ConvertsEveryRowOnceThenIsANoOp(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id := mustIdentity(t)
	for _, f := range v0Fixture {
		seedV0(t, pool, id, f.owner, f.name, f.value)
	}
	s, err := New(pool, id)
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.ConvertV0(ctx, id)
	if err != nil {
		t.Fatalf("ConvertV0: %v", err)
	}
	if n != len(v0Fixture) {
		t.Fatalf("converted %d rows, want %d", n, len(v0Fixture))
	}
	after := rawRows(t, pool)
	for _, f := range v0Fixture {
		r := after[rowRef(f.owner, f.name)]
		if r.version != encVersion || r.kekID != s.kek.ID() || len(r.wrapped) == 0 || bytes.HasPrefix(r.ct, []byte("age-encryption.org")) {
			t.Errorf("%s after conversion: enc_version=%d kek_id=%q — not a v1 envelope", rowRef(f.owner, f.name), r.version, r.kekID)
		}
		got, err := s.For(f.owner).Get(ctx, f.name)
		if err != nil || string(got) != f.value {
			t.Errorf("Get %s after conversion = (%q, %v), want the original value", rowRef(f.owner, f.name), got, err)
		}
	}

	again, err := s.ConvertV0(ctx, id)
	if err != nil || again != 0 {
		t.Fatalf("second ConvertV0 = (%d, %v), want (0, nil)", again, err)
	}
	for k, r := range rawRows(t, pool) {
		was := after[k]
		if r.kekID != was.kekID || !bytes.Equal(r.wrapped, was.wrapped) || !bytes.Equal(r.ct, was.ct) {
			t.Errorf("%s changed on the second conversion; it must be a no-op", k)
		}
	}
}

// TestPG_ConvertV0_AbortsOnAnUndecryptableRowAndCommitsNothing: one v0 row the
// key cannot read aborts the whole conversion, names the row, and leaves every
// row exactly as it was — still v0, still readable by the older binary.
func TestPG_ConvertV0_AbortsOnAnUndecryptableRowAndCommitsNothing(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id, stray := mustIdentity(t), mustIdentity(t)
	for _, f := range v0Fixture {
		seedV0(t, pool, id, f.owner, f.name, f.value)
	}
	seedV0(t, pool, stray, "bob@corp.example", "openai-api-key", "sk-bob-under-another-key")
	before := rawRows(t, pool)
	s, _ := New(pool, id)

	n, err := s.ConvertV0(ctx, id)
	if err == nil {
		t.Fatal("ConvertV0 succeeded over a row the key cannot decrypt")
	}
	if n != 0 {
		t.Errorf("aborted conversion returned %d", n)
	}
	for _, want := range []string{"ABORTED", rowRef("bob@corp.example", "openai-api-key"), "nothing committed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "sk-bob") {
		t.Error("abort error carries a value")
	}
	for k, r := range rawRows(t, pool) {
		was := before[k]
		if r.version != 0 || !bytes.Equal(r.ct, was.ct) {
			t.Errorf("%s changed although the conversion aborted", k)
		}
	}
}

// TestPG_ConvertV0_IsSingleWriter pins the advisory lock itself, not just
// the outcome (row FOR UPDATE alone would already convert each row once): while
// another session holds db.SecretConvertLockKey, a conversion must be seen
// WAITING on that lock in pg_locks, must not have converted anything, and must
// finish only once the lock is released.
func TestPG_ConvertV0_IsSingleWriter(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id := mustIdentity(t)
	for _, f := range v0Fixture {
		seedV0(t, pool, id, f.owner, f.name, f.value)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.SecretConvertLockKey); err != nil {
		t.Fatal(err)
	}

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, _ := New(pool, id)
		n, err := s.ConvertV0(ctx, id)
		done <- result{n, err}
	}()

	waiting := func() bool {
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_locks
			 WHERE locktype='advisory' AND NOT granted AND objsubid=1
			   AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
			   AND classid::bigint=$1 AND objid::bigint=$2`,
			db.SecretConvertLockKey>>32, db.SecretConvertLockKey&0xffffffff).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	deadline := time.Now().Add(10 * time.Second)
	for !waiting() {
		if time.Now().After(deadline) {
			t.Fatal("ConvertV0 never waited on db.SecretConvertLockKey — the conversion is not single-writer")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("ConvertV0 returned (%d, %v) while another session held the conversion lock", r.n, r.err)
	default:
	}
	for k, r := range rawRows(t, pool) {
		if r.version != 0 {
			t.Fatalf("%s converted while the lock was held elsewhere", k)
		}
	}

	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.n != len(v0Fixture) {
			t.Fatalf("ConvertV0 after the lock was released = (%d, %v), want (%d, nil)", r.n, r.err, len(v0Fixture))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ConvertV0 did not finish after the lock was released")
	}
}

// TestPG_OlderWardyndWritingAfterConversion covers the no-rolling-upgrade
// window, with the exact statement a pre-envelope wardynd runs. A NEW name it
// inserts lands as v0 and the next boot converts it. A name it REPLACES keeps
// its v1 columns around an age payload (its upsert sets ciphertext alone),
// which no conversion revisits: the read refuses it by name and says to set
// the secret again — and setting it again is the fix.
func TestPG_OlderWardyndWritingAfterConversion(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id := mustIdentity(t)
	s, _ := New(pool, id)
	if err := s.Put(ctx, "replaced", []byte("v1-value")); err != nil {
		t.Fatal(err)
	}
	oldPut := func(name, value string) {
		var buf bytes.Buffer
		w, err := age.Encrypt(&buf, id.Recipient())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(value))
		_ = w.Close()
		if _, err := pool.Exec(ctx, `
			INSERT INTO secrets (owned_by, name, ciphertext)
			VALUES ($1, $2, $3)
			ON CONFLICT (owned_by, name) DO UPDATE
				SET ciphertext=$3, updated_at=now()`, "", name, buf.Bytes()); err != nil {
			t.Fatalf("older wardynd's Put: %v", err)
		}
	}
	oldPut("fresh", "old-binary-new-name")
	oldPut("replaced", "old-binary-replacement")

	if n, err := s.ConvertV0(ctx, id); err != nil || n != 1 {
		t.Fatalf("restart's ConvertV0 = (%d, %v), want the one new name converted", n, err)
	}
	if got, err := s.Get(ctx, "fresh"); err != nil || string(got) != "old-binary-new-name" {
		t.Errorf("a new name an older wardynd wrote = (%q, %v) after a restart, want it converted and readable", got, err)
	}

	got, err := s.Get(ctx, "replaced")
	if err == nil {
		t.Fatalf("a v1 row an older wardynd overwrote in place opened as %q", got)
	}
	for _, want := range []string{rowRef("", "replaced"), "an older wardynd is still writing", "set this secret again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "old-binary-replacement") || strings.Contains(err.Error(), "v1-value") {
		t.Error("error carries a value")
	}
	if err := s.Put(ctx, "replaced", []byte("set-again")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, "replaced"); err != nil || string(got) != "set-again" {
		t.Errorf("after setting it again = (%q, %v)", got, err)
	}
}

// TestPG_UnknownEncVersionIsRefusedEverywhere: a row written in a format this
// binary predates (a newer wardynd's enc_version 2, seen during a mixed-version
// window or after a rollback) fails closed on every path, by name. Two rows make
// the two wrong fall-throughs observable: one whose columns are a perfectly
// good v1 envelope (a reader that treated "not 0" as v1 would open it) and one
// holding an age payload (a reader that fell back to age would open that).
func TestPG_UnknownEncVersionIsRefusedEverywhere(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id := mustIdentity(t)
	s, _ := New(pool, id)
	if err := s.Put(ctx, "v1-shaped", []byte("v1-shaped-value")); err != nil {
		t.Fatal(err)
	}
	seedV0(t, pool, id, "", "age-shaped", "age-shaped-value")
	if _, err := pool.Exec(ctx, `UPDATE secrets SET enc_version=2 WHERE owned_by='' AND name IN ('v1-shaped', 'age-shaped')`); err != nil {
		t.Fatal(err)
	}
	before := rawRows(t, pool)
	const want = "has enc_version 2 which this wardynd does not understand; upgrade wardynd"

	for _, name := range []string{"v1-shaped", "age-shaped"} {
		got, err := s.Get(ctx, name)
		assertRefused(t, "Get of an enc_version 2 row", got, err, "", name, name+"-value")
		if err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("Get %s: error %q does not say %q", name, err, want)
		}
		got, err = s.For("alice").Get(ctx, name)
		assertRefused(t, "operator-fallback Get of an enc_version 2 row", got, err, "", name, name+"-value")
	}

	if n, err := s.ConvertV0(ctx, id); err != nil || n != 0 {
		t.Fatalf("ConvertV0 = (%d, %v); an enc_version 2 row is not a v0 row and must be left alone", n, err)
	}
	n, err := Rekey(ctx, pool, id, mustIdentity(t))
	if err == nil || n != 0 || !strings.Contains(err.Error(), want) {
		t.Fatalf("Rekey over an enc_version 2 row = (%d, %v), want an abort that says %q", n, err, want)
	}
	for k, r := range rawRows(t, pool) {
		was := before[k]
		if r.version != was.version || r.kekID != was.kekID || !bytes.Equal(r.wrapped, was.wrapped) || !bytes.Equal(r.ct, was.ct) {
			t.Errorf("%s changed; no path may rewrite a row it does not understand", k)
		}
	}
}
