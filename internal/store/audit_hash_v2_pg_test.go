// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Live tests for audit row hash format 2 (migrations 0410 and 0412): rows written before them keep verifying,
// every new row is salted, and a tampered row, a tampered salt or an unknown format fails the sweep.

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	hashV2Migration   = "0410_audit_hash_v2.sql"
	hashV2ChainSwitch = "0412_audit_chain_v2.sql"
)

var salt64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// appendViaFunction writes one row through audit_append, the one way a row enters the log.
func appendViaFunction(t *testing.T, pool *pgxpool.Pool, action string) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(), `SELECT seq FROM audit_append(gen_random_uuid(), now(), NULL,
		'system', 'hash-v2-probe', $1, '', 'success', '', '{"fixture": true}')`, action).Scan(&seq); err != nil {
		t.Fatalf("audit_append %s: %v", action, err)
	}
	return seq
}

// v1ThenV2 is a database that held format 1 rows when the migrations ran: three rows written by the chain
// before them, then the migrations, then three rows written after.
func v1ThenV2(t *testing.T) (pool *pgxpool.Pool, v1, v2 []int64) {
	t.Helper()
	pool = databaseBefore(t, hashV2Migration)
	for i := 0; i < 3; i++ {
		v1 = append(v1, appendViaFunction(t, pool, fmt.Sprintf("hash.v1.%d", i)))
	}
	for _, f := range []string{hashV2Migration, hashV2ChainSwitch} {
		execMigrationFile(t, pool, f)
	}
	for i := 0; i < 3; i++ {
		v2 = append(v2, appendViaFunction(t, pool, fmt.Sprintf("hash.v2.%d", i)))
	}
	return pool, v1, v2
}

func wantVerifies(t *testing.T, pool *pgxpool.Pool, what string) {
	t.Helper()
	st, err := store.NewPG(pool).VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatalf("%s: verify: %v", what, err)
	}
	if !st.OK {
		t.Fatalf("%s: the chain does not verify: seq %d, %s", what, st.BrokenSeq, st.Reason)
	}
}

func wantBreaksAt(t *testing.T, pool *pgxpool.Pool, seq int64, what string) {
	t.Helper()
	st, err := store.NewPG(pool).VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatalf("%s: verify: %v", what, err)
	}
	if st.OK || st.BrokenSeq != seq {
		t.Fatalf("%s: verify = %+v, want a break at seq %d", what, st, seq)
	}
}

func TestVerify_V1RowsUnchanged_PG(t *testing.T) {
	pool := databaseBefore(t, hashV2Migration)
	ctx := context.Background()
	var v1 []int64
	for i := 0; i < 3; i++ {
		v1 = append(v1, appendViaFunction(t, pool, fmt.Sprintf("hash.v1.%d", i)))
	}
	hashes := func() string {
		return scalar[string](t, pool, `SELECT string_agg(row_hash, ',' ORDER BY seq) FROM audit_events WHERE seq = ANY($1)`, v1)
	}
	before := hashes()
	for _, f := range []string{hashV2Migration, hashV2ChainSwitch} {
		execMigrationFile(t, pool, f)
	}
	if after := hashes(); after != before {
		t.Fatalf("the migration changed a stored row hash:\nbefore %s\nafter  %s", before, after)
	}
	wantVerifies(t, pool, "format 1 rows after the migration")
	if n := scalar[int64](t, pool, `SELECT count(*) FROM audit_events WHERE seq = ANY($1) AND hash_format = 1 AND hash_salt IS NULL AND claim_salt IS NULL`, v1); n != 3 {
		t.Fatalf("%d of 3 old rows read format 1 with no salt", n)
	}
	v2 := appendViaFunction(t, pool, "hash.v2.after")
	wantVerifies(t, pool, "format 1 then format 2 rows")
	var f int16
	var salt string
	if err := pool.QueryRow(ctx, `SELECT hash_format, hash_salt FROM audit_events WHERE seq = $1`, v2).Scan(&f, &salt); err != nil {
		t.Fatal(err)
	}
	if f != 2 || !salt64.MatchString(salt) {
		t.Fatalf("new row: format %d, salt %q, want 2 and 64 lowercase hex", f, len(salt))
	}
}

func TestVerify_TamperedV1RowFails_PG(t *testing.T) {
	pool, v1, _ := v1ThenV2(t)
	wantVerifies(t, pool, "before the edit")
	tamperAudit(t, pool, `UPDATE audit_events SET actor = 'forged' WHERE seq = $1`, v1[1])
	wantBreaksAt(t, pool, v1[1], "an edited format 1 row")
}

func TestVerify_TamperedV2RowFails_PG(t *testing.T) {
	pool, _, v2 := v1ThenV2(t)
	tamperAudit(t, pool, `UPDATE audit_events SET actor = 'forged' WHERE seq = $1`, v2[1])
	wantBreaksAt(t, pool, v2[1], "an edited format 2 row")
}

func TestVerify_TamperedSaltFails_PG(t *testing.T) {
	pool, _, v2 := v1ThenV2(t)
	tamperAudit(t, pool, `UPDATE audit_events SET hash_salt = repeat('a', 64) WHERE seq = $1`, v2[1])
	wantBreaksAt(t, pool, v2[1], "a rewritten salt")
}

// TestVerify_UnknownFormatFails_PG: the owner role sets a row's format to 3; the sweep names that seq.
func TestVerify_UnknownFormatFails_PG(t *testing.T) {
	pool, _, v2 := v1ThenV2(t)
	tamperAudit(t, pool, `UPDATE audit_events SET hash_format = 3 WHERE seq = $1`, v2[1])
	wantBreaksAt(t, pool, v2[1], "an unknown format")
}

// TestAuditChainTrigger_RefusesUnknownFormat_PG: the chain trigger, reached as audit_append reaches it,
// refuses a format outside 1 and 2 and a format 2 row with no usable salt.
func TestAuditChainTrigger_RefusesUnknownFormat_PG(t *testing.T) {
	pool, _, _ := v1ThenV2(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		format int
		salt   any
		want   string
	}{
		"format 3":        {3, strings.Repeat("a", 64), "unknown hash_format"},
		"format 2, none":  {2, nil, "64 lowercase hex"},
		"format 2, short": {2, "abc", "64 lowercase hex"},
		"format 2, upper": {2, strings.Repeat("A", 64), "64 lowercase hex"},
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var seq int64
		if err := tx.QueryRow(ctx, `SELECT nextval('audit_events_seq')`).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `SELECT pg_catalog.set_config('wardyn.audit_seq', $1::text, true)`, seq)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO audit_events (seq, id, "time", actor_type, actor, action, outcome, recorded_at, hash_format, hash_salt)
				SELECT $1, gen_random_uuid(), now(), 'system', 'probe', 'hash.bad', 'success',
				       greatest(clock_timestamp(), m.hw_recorded_at, m.cutover), $2, $3 FROM audit_partition_meta m`, seq, c.format, c.salt)
		}
		_ = tx.Rollback(ctx)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, c.want)
		}
	}
}

// TestVerify_V2RowNeverNullHash_PG: the trigger never leaves a format 2 row's hash NULL, which is why the
// digest sites that hash only NULL-hash rows may stay on format 1.
func TestVerify_V2RowNeverNullHash_PG(t *testing.T) {
	pool, _, _ := v1ThenV2(t)
	if n := scalar[int64](t, pool, `SELECT count(*) FROM audit_events WHERE hash_format = 2 AND row_hash IS NULL`); n != 0 {
		t.Fatalf("%d format 2 rows have no row_hash", n)
	}
	if n := scalar[int64](t, pool, `SELECT count(*) FROM audit_events WHERE hash_format = 2`); n < 3 {
		t.Fatalf("only %d format 2 rows; the fixture wrote 3", n)
	}
}

// TestAuditAppend_EveryCallerWritesV2_PG: the single insert, the batch and every other writer in the database
// (the boot canary, the seed rows) write format 2 with their own 64-hex salt; no two salts are equal and
// claim_salt is NULL on all of them.
func TestAuditAppend_EveryCallerWritesV2_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	one := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "v2-probe", Action: "hash.single", Outcome: "success"}
	if err := store.InsertAuditEvent(ctx, pool, &one); err != nil {
		t.Fatal(err)
	}
	batch := []*types.AuditEvent{
		{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "v2-probe", Action: "hash.batch.1", Outcome: "success"},
		{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "v2-probe", Action: "hash.batch.2", Outcome: "success"},
	}
	if _, err := store.InsertAuditEvents(ctx, pool, batch); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT seq, hash_format, hash_salt, claim_salt FROM audit_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen, n := map[string]bool{}, 0
	for rows.Next() {
		var seq int64
		var f int16
		var salt, claim *string
		if err := rows.Scan(&seq, &f, &salt, &claim); err != nil {
			t.Fatal(err)
		}
		n++
		if f != 2 || salt == nil || !salt64.MatchString(*salt) || claim != nil {
			t.Errorf("seq %d: format %d, a salt that fits %v, claim_salt set %v; want format 2, a 64-hex salt, no claim salt", seq, f, salt != nil && salt64.MatchString(*salt), claim != nil)
			continue
		}
		if seen[*salt] {
			t.Errorf("seq %d repeats a salt", seq)
		}
		seen[*salt] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("%d rows in the log, want at least the 3 this test wrote", n)
	}
	wantVerifies(t, pool, "rows of every writer")
}

// TestAuditSplit_CarriesHashColumns_PG: the legacy split copies a row's format and salt, because it reads its
// column list from the catalog.
func TestAuditSplit_CarriesHashColumns_PG(t *testing.T) {
	f := newSplitFixture(t)
	ctx := context.Background()
	for _, name := range []string{hashV2Migration, hashV2ChainSwitch} {
		execMigrationFile(t, f.pool, name)
	}
	v2 := appendViaFunction(t, f.pool, "split.v2")
	var salt string
	if err := f.pool.QueryRow(ctx, `SELECT hash_salt FROM audit_events WHERE seq = $1`, v2).Scan(&salt); err != nil {
		t.Fatal(err)
	}
	wantVerifies(t, f.pool, "before the split")
	res, err := split(ctx, f.pool)
	if err != nil {
		t.Fatalf("SplitLegacyAudit: %v", err)
	}
	for _, r := range res.Ranges {
		n := scalar[int64](t, f.pool, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = $1 AND column_name IN ('hash_format', 'hash_salt', 'claim_salt')`, r.Name)
		if n != 3 {
			t.Errorf("%s has %d of the 3 hash columns", r.Name, n)
		}
	}
	if got := scalar[string](t, f.pool, `SELECT hash_salt FROM audit_events WHERE seq = $1`, v2); got != salt {
		t.Errorf("the format 2 row's salt changed through the split")
	}
	if n := scalar[int64](t, f.pool, `SELECT count(*) FROM audit_events WHERE hash_format = 1 AND row_hash IS NOT NULL`); n == 0 {
		t.Errorf("no format 1 row survived the split")
	}
	wantVerifies(t, f.pool, "after the split")
}

// receiptRowsOf reads rows lo..hi as a receipt carries them.
func receiptRowsOf(t *testing.T, pool *pgxpool.Pool, lo, hi int64) []auditreceipt.Row {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT seq, id, (extract(epoch FROM time) * 1000000)::bigint, run_id, actor_type, actor,
		action, target, outcome, source_ip, data::text, prev_hash, row_hash, hash_format, hash_salt
		FROM audit_events WHERE seq BETWEEN $1 AND $2 ORDER BY seq`, lo, hi)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditreceipt.Row
	for rows.Next() {
		var r auditreceipt.Row
		if err := rows.Scan(&r.Seq, &r.ID, &r.TimeUS, &r.RunID, &r.ActorType, &r.Actor, &r.Action, &r.Target, &r.Outcome,
			&r.SourceIP, &r.Data, &r.PrevHash, &r.RowHash, &r.HashFormat, &r.HashSalt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHashInput_V2MatchesSQL_PG: the Go formula and audit_row_hash_any agree on rows of both formats.
func TestHashInput_V2MatchesSQL_PG(t *testing.T) {
	pool, v1, v2 := v1ThenV2(t)
	for _, r := range receiptRowsOf(t, pool, v1[0], v2[len(v2)-1]) {
		got, err := auditreceipt.RowHashOf(r)
		if err != nil || r.RowHash == nil || got != *r.RowHash {
			t.Errorf("seq %d (format %d): Go hash %q (%v), stored %v", r.Seq, r.HashFormat, got, err, r.RowHash)
		}
	}
	var viaSQL string
	if err := pool.QueryRow(context.Background(), `SELECT audit_row_hash_any(hash_format, hash_salt, prev_hash, id, time, run_id, actor_type, actor,
		action, target, outcome, source_ip, data) FROM audit_events WHERE seq = $1`, v2[0]).Scan(&viaSQL); err != nil {
		t.Fatal(err)
	}
	if want := scalar[string](t, pool, `SELECT row_hash FROM audit_events WHERE seq = $1`, v2[0]); viaSQL != want {
		t.Errorf("audit_row_hash_any = %s, stored %s", viaSQL, want)
	}
	if scalar[*string](t, pool, `SELECT audit_row_hash_any(3::smallint, NULL, NULL, gen_random_uuid(), now(), NULL, 'a', 'b', 'c', 'd', 'e', 'f', NULL)`) != nil {
		t.Error("an unknown format must hash to NULL")
	}
}

// TestAuditReceipt_V2VerifiesOffline_PG: rows of both formats, read as an unprojected receipt carries them,
// verify under a signed checkpoint with no database.
func TestAuditReceipt_V2VerifiesOffline_PG(t *testing.T) {
	pool, v1, v2 := v1ThenV2(t)
	rows := receiptRowsOf(t, pool, v1[0], v2[len(v2)-1])
	if int64(len(rows)) != v2[len(v2)-1]-v1[0]+1 {
		t.Fatalf("seqs %d..%d are not contiguous in an isolated database", v1[0], v2[len(v2)-1])
	}
	hashes := make([]string, len(rows))
	seqs := make([]int64, len(rows))
	for i, r := range rows {
		hashes[i], seqs[i] = *r.RowHash, r.Seq
	}
	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	last := len(rows) - 1
	st := auditreceipt.Statement{Format: auditreceipt.CheckpointFormat, Kid: auditreceipt.KeyID(pub), SeqLo: rows[0].Seq, SeqHi: rows[last].Seq,
		Rows: int64(len(rows)), HeadHash: hashes[last], Digest: auditreceipt.Fold("", hashes), SeqDigest: auditreceipt.SeqDigest(seqs, hashes),
		SignedAt: "2026-11-24T10:00:00Z"}
	text := string(st.Bytes())
	cp := auditreceipt.Checkpoint{Statement: text, Signature: ed25519.Sign(priv, []byte(text)), Seqs: seqs, RowHashes: hashes}
	for i, r := range rows {
		cp.Events = append(cp.Events, auditreceipt.Event{Index: i, Row: r})
	}
	keys := auditreceipt.Keys{Format: auditreceipt.KeysFormat, Keys: []auditreceipt.PublicKey{{Kid: auditreceipt.KeyID(pub), PublicKey: pub,
		Fingerprint: auditreceipt.Fingerprint(pub), CreatedAt: time.Unix(0, 0).UTC()}}}
	rep := auditreceipt.VerifyBundle(auditreceipt.Bundle{Format: auditreceipt.ReceiptFormat, Checkpoints: []auditreceipt.Checkpoint{cp}}, keys)
	if !rep.OK() || !rep.Complete() {
		t.Fatalf("receipt of format 1 and 2 rows: failures %+v", rep.Failures)
	}
}
