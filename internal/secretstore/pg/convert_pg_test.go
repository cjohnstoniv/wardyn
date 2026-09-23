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
	"sync"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"
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

// TestPG_ConvertV0_IsSingleWriter: two replicas booting at once convert each
// row exactly once between them — the loser waits on the advisory lock, then
// finds nothing left.
func TestPG_ConvertV0_IsSingleWriter(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id := mustIdentity(t)
	for _, f := range v0Fixture {
		seedV0(t, pool, id, f.owner, f.name, f.value)
	}
	var wg sync.WaitGroup
	counts := make([]int, 2)
	errs := make([]error, 2)
	for i := range counts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _ := New(pool, id)
			counts[i], errs[i] = s.ConvertV0(ctx, id)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ConvertV0: %v", err)
		}
	}
	if counts[0]+counts[1] != len(v0Fixture) || (counts[0] != 0 && counts[1] != 0) {
		t.Fatalf("concurrent conversions converted %v rows, want all %d by exactly one", counts, len(v0Fixture))
	}
	s, _ := New(pool, id)
	for _, f := range v0Fixture {
		if got, err := s.For(f.owner).Get(ctx, f.name); err != nil || string(got) != f.value {
			t.Errorf("Get %s = (%q, %v)", rowRef(f.owner, f.name), got, err)
		}
	}
}
