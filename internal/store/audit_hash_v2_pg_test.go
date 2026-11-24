// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Live tests for audit row hash format 2 (migrations 0410 and 0412): rows written before them keep verifying,
// every new row is salted, and a tampered row, a tampered salt or an unknown format fails the sweep.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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

// receiptRowsOf reads rows lo..hi as a receipt carries them: through AuditReceiptRows, the read a receipt is
// built from.
func receiptRowsOf(t *testing.T, pool *pgxpool.Pool, lo, hi int64) []auditreceipt.Row {
	t.Helper()
	ctx := context.Background()
	rs, err := pool.Query(ctx, `SELECT id FROM audit_events WHERE seq BETWEEN $1 AND $2 ORDER BY seq`, lo, hi)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for rs.Next() {
		var id uuid.UUID
		if err := rs.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	stored, err := store.NewPG(pool).AuditReceiptRows(ctx, ids, nil, 0, len(ids)+1)
	if err != nil || len(stored) != len(ids) {
		t.Fatalf("AuditReceiptRows = %d rows, %v; want %d", len(stored), err, len(ids))
	}
	out := make([]auditreceipt.Row, len(stored))
	for i, r := range stored {
		out[i] = receiptRow(r)
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
	if first, last := rows[0], rows[len(rows)-1]; first.HashFormat > 1 || first.HashSalt != nil || last.HashFormat != 2 || last.HashSalt == nil {
		t.Fatalf("AuditReceiptRows lost the hash format or salt: first format %d, last format %d", first.HashFormat, last.HashFormat)
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
		SignedAt: time.Unix(0, 0).UTC().Format(time.RFC3339)}
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

// v2Claim is a device's format 2 claim: a genuine hash over exactly what it says, made with salt.
func v2Claim(t *testing.T, pool *pgxpool.Pool, prev string, seq int64, action, salt string) types.FederatedAuditRow {
	t.Helper()
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem,
		Actor: "device-forwarder", Action: action, Outcome: "success", SourceIP: "10.9.9.9", PrevHash: prev,
		Data: json.RawMessage(fmt.Sprintf(`{"seq": %d}`, seq))}
	if err := pool.QueryRow(context.Background(), `SELECT audit_row_hash_v2($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb)`,
		prev, salt, ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action, ev.Target, ev.Outcome, ev.SourceIP, []byte(ev.Data)).Scan(&ev.RowHash); err != nil {
		t.Fatalf("hash a format 2 claim: %v", err)
	}
	return types.FederatedAuditRow{FederatedAuditEvent: types.FederatedAuditEvent{AuditEvent: ev, Seq: seq}, HashFormat: 2, HashSalt: salt}
}

type storedClaim struct {
	format        int16
	salt          string
	claim         *string
	originFormat  *string
	claimRechecks bool
}

func readStoredClaim(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) storedClaim {
	t.Helper()
	var c storedClaim
	if err := pool.QueryRow(context.Background(), `SELECT e.hash_format, e.hash_salt, e.claim_salt, e.data->'device_origin'->>'hash_format',
		`+store.FederatedClaimHashSQL+` = e.data->'device_origin'->>'row_hash' FROM audit_events e WHERE e.id = $1`, id).
		Scan(&c.format, &c.salt, &c.claim, &c.originFormat, &c.claimRechecks); err != nil {
		t.Fatalf("read the stored claim: %v", err)
	}
	return c
}

func TestIngestDeviceAudit_V2ClaimVerifies_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	saltA, saltB := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	r1 := v2Claim(t, pool, "", 1, "dev.v2.1", saltA)
	r2 := v2Claim(t, pool, r1.RowHash, 2, "dev.v2.2", saltB)
	res, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditRow{r1, r2})
	if err != nil || res.Accepted != 2 {
		t.Fatalf("ingest of two format 2 claims: %+v, %v", res, err)
	}
	wantVerifies(t, pool, "after a device's format 2 rows")
}

func TestIngestDeviceAudit_V2ClaimTamperedRefused_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	salt := strings.Repeat("a1", 32)
	r := v2Claim(t, pool, "", 1, "dev.v2.tamper", salt)
	edited := r
	edited.Action = "dev.v2.edited"
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditRow{edited}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("an edited format 2 claim: err = %v, want ErrConflict", err)
	}
	wrongSalt := r
	wrongSalt.HashSalt = strings.Repeat("c3", 32)
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditRow{wrongSalt}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a format 2 claim with another salt: err = %v, want ErrConflict", err)
	}
	asV1 := r
	asV1.HashFormat, asV1.HashSalt = 0, ""
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditRow{asV1}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a format 2 claim passed off as format 1: err = %v, want ErrConflict", err)
	}
	if n := scalar[int64](t, pool, `SELECT count(*) FROM audit_events WHERE action = 'dev.v2.tamper' OR action = 'dev.v2.edited'`); n != 0 {
		t.Fatalf("%d rows of a refused claim were stored", n)
	}
}

func TestIngestDeviceAudit_V1DeviceStillAccepted_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	r := chainedRow(t, pool, "", 1, "dev.v1.still")
	if res, err := ingestEvents(ctx, st, d.ID, testPeer, []types.FederatedAuditEvent{r}); err != nil || res.Accepted != 1 {
		t.Fatalf("a format 1 device row with no format field: %+v, %v", res, err)
	}
	c := readStoredClaim(t, pool, r.ID)
	if c.originFormat != nil || c.claim != nil || !c.claimRechecks || c.format != 2 {
		t.Fatalf("stored claim = %+v, want no hash_format in device_origin, no claim_salt, a claim that re-checks, the organisation's own format 2", c)
	}
}

// TestIngestDeviceAudit_ControlPlaneRowTakesFreshSalt_PG: the stored row's salt is the control plane's own,
// never the device's; the device's goes to claim_salt, and both the row and the claim re-check.
func TestIngestDeviceAudit_ControlPlaneRowTakesFreshSalt_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	claimed := strings.Repeat("a1", 32)
	r := v2Claim(t, pool, "", 1, "dev.v2.fresh", claimed)
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditRow{r}); err != nil {
		t.Fatal(err)
	}
	c := readStoredClaim(t, pool, r.ID)
	if c.format != 2 || !salt64.MatchString(c.salt) || c.salt == claimed {
		t.Errorf("stored row: format %d, a 64-hex salt %v, the device's own salt %v; want format 2 and the control plane's salt", c.format, salt64.MatchString(c.salt), c.salt == claimed)
	}
	if c.claim == nil || *c.claim != claimed {
		t.Errorf("claim_salt is not the device's salt")
	}
	if c.originFormat == nil || *c.originFormat != "2" || !c.claimRechecks {
		t.Errorf("device_origin hash_format %v, claim re-checks %v; want 2 and true", c.originFormat, c.claimRechecks)
	}
	wantVerifies(t, pool, "after a device's format 2 row")
}

func TestIngestDeviceAudit_BadFormatOrSaltIsInvalidRow_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	good := strings.Repeat("a1", 32)
	for name, mutate := range map[string]func(*types.FederatedAuditRow){
		"format 3":            func(r *types.FederatedAuditRow) { r.HashFormat = 3 },
		"a 63-character salt": func(r *types.FederatedAuditRow) { r.HashSalt = good[:63] },
		"an upper-case salt":  func(r *types.FederatedAuditRow) { r.HashSalt = strings.ToUpper(good) },
		"a salt on format 1":  func(r *types.FederatedAuditRow) { r.HashFormat = 1 },
	} {
		action := "dev.v2.bad." + uuid.NewString()
		r := v2Claim(t, pool, "", 1, action, good)
		mutate(&r)
		_, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditRow{r})
		if !errors.Is(err, store.ErrFederatedRowInvalid) {
			t.Errorf("%s: err = %v, want ErrFederatedRowInvalid", name, err)
		}
		if err != nil && strings.Contains(err.Error(), good) {
			t.Errorf("%s: the error names the salt", name)
		}
		if n := scalar[int64](t, pool, `SELECT count(*) FROM audit_events WHERE action = $1`, action); n != 0 {
			t.Errorf("%s: %d rows were inserted", name, n)
		}
	}
}

func TestFederatedClaimHash_V2RecheckableFromStoredRow_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	r1 := v2Claim(t, pool, "", 1, "dev.v2.recheck.1", strings.Repeat("a1", 32))
	r2 := v2Claim(t, pool, r1.RowHash, 2, "dev.v2.recheck.2", strings.Repeat("b2", 32))
	old := chainedRow(t, pool, r2.RowHash, 3, "dev.v1.recheck")
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditRow{r1, r2, {FederatedAuditEvent: old}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{r1.ID, r2.ID, old.ID} {
		if c := readStoredClaim(t, pool, id); !c.claimRechecks {
			t.Errorf("row %s: the claim does not re-check from the stored row (%+v)", id, c)
		}
	}
	// The claim salt is part of the re-check: change it and the claim no longer holds, with no chain verdict moved.
	tamperAudit(t, pool, `UPDATE audit_events SET claim_salt = repeat('f', 64) WHERE id = $1`, r1.ID)
	if c := readStoredClaim(t, pool, r1.ID); c.claimRechecks {
		t.Errorf("a rewritten claim_salt still re-checks")
	}
	wantVerifies(t, pool, "after a claim_salt edit, which the control plane's own hash does not cover")
}

func TestAuditAppend_ClaimSaltMalformedRaises_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	before := scalar[int64](t, pool, `SELECT count(*) FROM audit_events`)
	for _, bad := range []string{"xyz", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `SELECT pg_catalog.set_config('wardyn.audit_claim_salt', $1, true)`, bad)
		if err == nil {
			_, err = tx.Exec(ctx, `SELECT seq FROM audit_append(gen_random_uuid(), now(), NULL, 'system', 'probe', 'claim.bad', '', 'success', '', NULL)`)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Errorf("claim salt %q: err = %v, want SQLSTATE 22023", bad, err)
		}
		if err != nil && strings.Contains(err.Error(), bad) && len(bad) > 3 {
			t.Errorf("the error names the value")
		}
		_ = tx.Rollback(ctx)
	}
	if after := scalar[int64](t, pool, `SELECT count(*) FROM audit_events`); after != before {
		t.Fatalf("%d rows were written by a refused append", after-before)
	}
}

func TestAuditAppend_ClaimSaltClearedAfterUse_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	claim := strings.Repeat("a1", 32)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('wardyn.audit_claim_salt', $1, true)`, claim); err != nil {
		t.Fatal(err)
	}
	var first, second int64
	q := `SELECT seq FROM audit_append(gen_random_uuid(), now(), NULL, 'system', 'probe', 'claim.cleared', '', 'success', '', NULL)`
	if err := tx.QueryRow(ctx, q).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, q).Scan(&second); err != nil {
		t.Fatal(err)
	}
	var c1, c2 *string
	if err := tx.QueryRow(ctx, `SELECT (SELECT claim_salt FROM audit_events WHERE seq = $1), (SELECT claim_salt FROM audit_events WHERE seq = $2)`, first, second).Scan(&c1, &c2); err != nil {
		t.Fatal(err)
	}
	if c1 == nil || *c1 != claim || c2 != nil {
		t.Fatalf("claim_salt of the two rows = %v, %v; want the device's salt, then NULL", c1, c2)
	}
}

// TestListAuditEventsAfterSeq_NoSalt_PG: the acknowledged sink's read marshals no salt; the forwarder's read
// returns each row's format and salt.
func TestListAuditEventsAfterSeq_NoSalt_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	base := maxAuditSeq(t, pool)
	one := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "v2-probe", Action: "list.salt", Outcome: "success"}
	if err := store.InsertAuditEvent(ctx, pool, &one); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListAuditEventsAfterSeq(ctx, base, 1000)
	if err != nil || len(events) == 0 {
		t.Fatalf("list: %d rows, %v", len(events), err)
	}
	b, _ := json.Marshal(events)
	for _, key := range []string{"hash_salt", "claim_salt", "hash_format"} {
		if strings.Contains(string(b), key) {
			t.Errorf("ListAuditEventsAfterSeq marshals %q", key)
		}
	}
	rows, err := st.ListFederatedAuditRowsAfterSeq(ctx, base, 1000)
	if err != nil || len(rows) != len(events) {
		t.Fatalf("forwarder read: %d rows, %v", len(rows), err)
	}
	for _, r := range rows {
		want := scalar[string](t, pool, `SELECT hash_salt FROM audit_events WHERE seq = $1`, r.Seq)
		if r.HashFormat != 2 || r.HashSalt != want {
			t.Errorf("seq %d: format %d, salt matches %v; want 2 and the stored salt", r.Seq, r.HashFormat, r.HashSalt == want)
		}
	}
}
