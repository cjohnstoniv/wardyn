// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"strings"
	"testing"
)

// TestMigrate_RevocationOrderBackfill (0176): cut rows that exist before the migration get a seq in
// revoked_at order, below the sequence's value afterwards, so a credential that records the sequence at
// its issue is not ended by a cut older than itself, and one that records nothing is still compared on
// revoked_at; the trigger draws the seq for every later insert and update; the migration is replay-safe.
func TestMigrate_RevocationOrderBackfill(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// The state a database is in before the migration: cut rows with no seq, no trigger, a sequence never drawn.
	exec(`DROP TRIGGER oidc_session_revocations_stamp ON oidc_session_revocations`)
	exec(`DROP TRIGGER oidc_session_cuts_stamp ON oidc_session_cuts`)
	exec(`ALTER SEQUENCE oidc_revocation_seq RESTART WITH 1`)
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at, seq) VALUES
		('late', now() - interval '1 hour', 0), ('early', now() - interval '3 hours', 0), ('', now() - interval '2 hours', 0)`)
	exec(`INSERT INTO oidc_session_cuts (sub, cut_at, seq) VALUES ('cut', now() - interval '90 minutes', 0)`)

	sql, err := migrationFS.ReadFile("migrations/0176_revocation_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // replayed: nothing changes the second time
		exec(string(sql))
		seqOf := func(table, sub string) int64 {
			var n int64
			if err := pool.QueryRow(ctx, `SELECT seq FROM `+table+` WHERE sub = $1`, sub).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		early, global, cut, late := seqOf("oidc_session_revocations", "early"), seqOf("oidc_session_revocations", ""),
			seqOf("oidc_session_cuts", "cut"), seqOf("oidc_session_revocations", "late")
		if early < 1 || global < 1 || cut < 1 || late < 1 {
			t.Fatalf("pass %d: a cut row kept seq 0: early=%d global=%d cut=%d late=%d", i, early, global, cut, late)
		}
		var last int64
		if err := pool.QueryRow(ctx, `SELECT last_value FROM oidc_revocation_seq`).Scan(&last); err != nil {
			t.Fatal(err)
		}
		for _, n := range []int64{early, global, cut, late} {
			if n > last {
				t.Fatalf("pass %d: a backfilled seq %d is above the sequence's value %d, so a credential issued now would be ended by it", i, n, last)
			}
		}
		if i == 0 && !(early < global && global < late) {
			t.Fatalf("backfill is not in revoked_at order: early=%d global=%d late=%d", early, global, late)
		}
	}

	// The trigger draws the seq for an insert that names none and for an update.
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ('fresh', now())`)
	var first, second, last int64
	if err := pool.QueryRow(ctx, `SELECT seq FROM oidc_session_revocations WHERE sub = 'fresh'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE oidc_session_revocations SET revoked_at = now() WHERE sub = 'fresh'`)
	if err := pool.QueryRow(ctx, `SELECT seq FROM oidc_session_revocations WHERE sub = 'fresh'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT last_value FROM oidc_revocation_seq`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if second <= first || first <= 0 || second != last {
		t.Fatalf("trigger seqs: insert %d, update %d, sequence %d; want each draw greater than the last", first, second, last)
	}
}

// TestMigrate_RevocationOrderLeavesTheSequenceCalled (0176): on a sequence never drawn from, the first
// value a credential records and the first value a cut draws must differ. A fresh sequence reports
// last_value 1 with is_called false, so the first nextval would return the value an issuer already read.
func TestMigrate_RevocationOrderLeavesTheSequenceCalled(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`DROP TRIGGER oidc_session_revocations_stamp ON oidc_session_revocations`)
	exec(`DROP TRIGGER oidc_session_cuts_stamp ON oidc_session_cuts`)
	exec(`DELETE FROM oidc_session_revocations`)
	exec(`DELETE FROM oidc_session_cuts`)
	exec(`ALTER SEQUENCE oidc_revocation_seq RESTART WITH 1`)
	sql, err := migrationFS.ReadFile("migrations/0176_revocation_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(sql))

	var last int64
	var called bool
	if err := pool.QueryRow(ctx, `SELECT last_value, is_called FROM oidc_revocation_seq`).Scan(&last, &called); err != nil {
		t.Fatal(err)
	}
	if !called || last < 1 {
		t.Fatalf("sequence after the migration: last_value %d, is_called %v; want a called sequence, so the value an issuer reads is not the one the next cut draws", last, called)
	}
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ('first', now())`)
	var cut int64
	if err := pool.QueryRow(ctx, `SELECT seq FROM oidc_session_revocations WHERE sub = 'first'`).Scan(&cut); err != nil {
		t.Fatal(err)
	}
	if cut <= last {
		t.Fatalf("the first cut drew %d, not above the %d a credential issued at migration time records", cut, last)
	}
}

// TestMigrate_RevocationOrderFreezesLegacyTokensAndKeys (0176): an API token or SSH key that exists
// at the migration, and can outlive any session, is given its place among the cuts once: the seq of
// the latest cut that predates it, so a cut made after it ends it and a cut made before does not. The
// stored order does not move when a clock does.
func TestMigrate_RevocationOrderFreezesLegacyTokensAndKeys(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`DROP TRIGGER oidc_session_revocations_stamp ON oidc_session_revocations`)
	exec(`DROP TRIGGER oidc_session_cuts_stamp ON oidc_session_cuts`)
	exec(`ALTER SEQUENCE oidc_revocation_seq RESTART WITH 1`)
	// alice was cut 2h ago; the global cut, 30m ago, ends everything created before it.
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at, seq) VALUES
		('alice', now() - interval '2 hours', 0), ('', now() - interval '30 minutes', 0)`)
	type cred struct{ name, principal, email, created string }
	creds := []cred{
		{"before-alice-cut", "alice", "", "3 hours"}, // older than both cuts
		{"between-cuts", "alice", "", "1 hour"},      // after alice's cut, before the global one
		{"after-both", "alice", "", "10 minutes"},    // after both
		{"by-email", "someone", "alice", "3 hours"},  // matched by email, older than alice's cut
		{"unrelated-before-global", "bob", "", "1 hour"},
		{"unrelated-after-global", "bob", "", "10 minutes"},
	}
	for _, c := range creds {
		exec(`INSERT INTO api_tokens (id, principal, email, name, token_sha256, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $3, now() - $4::interval)`, c.principal, c.email, c.name, c.created)
		exec(`INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, created_at)
			VALUES ($2, $1, $2, 'k', now() - $3::interval)`, c.principal, c.name, c.created)
	}
	// carol's token and key were created at the very instant of the global cut: the time check refuses at
	// equality, so the order must end them too.
	exec(`INSERT INTO api_tokens (id, principal, email, name, token_sha256, created_at)
		SELECT gen_random_uuid(), 'carol', '', 'at-global-cut', 'at-global-cut', revoked_at FROM oidc_session_revocations WHERE sub = ''`)
	exec(`INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, created_at)
		SELECT 'at-global-cut', 'carol', 'at-global-cut', 'k', revoked_at FROM oidc_session_revocations WHERE sub = ''`)
	sql, err := migrationFS.ReadFile("migrations/0176_revocation_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(sql))

	// revokedBy reports whether a cut that matches the credential drew a greater seq than the one it
	// holds, which is how sessionStatus orders a credential that recorded one. match is the cut's
	// predicate for the table: a token also reads the cuts naming its email, a key does not.
	revokedBy := func(table, name, match string) bool {
		t.Helper()
		var rev, cut int64
		q := `SELECT rev_seq, COALESCE((SELECT max(r.seq) FROM oidc_session_revocations r
			WHERE r.sub = t.principal OR r.sub = ''` + match + `), 0) FROM ` + table + ` t WHERE t.name = $1`
		if err := pool.QueryRow(ctx, q, name).Scan(&rev, &cut); err != nil {
			t.Fatal(err)
		}
		if rev < 1 {
			t.Fatalf("%s %s kept rev_seq %d: still compared on time", table, name, rev)
		}
		return cut > rev
	}
	want := map[string]bool{
		"before-alice-cut": true, "between-cuts": true, "after-both": false, "by-email": true,
		"unrelated-before-global": true, "unrelated-after-global": false, "at-global-cut": true,
	}
	check := func(label string) {
		t.Helper()
		for name, revoked := range want {
			if got := revokedBy("api_tokens", name, ` OR lower(r.sub) = lower(t.email)`); got != revoked {
				t.Errorf("%s: api token %s revoked = %v, want %v", label, name, got, revoked)
			}
			if got := revokedBy("ssh_public_keys", name, ""); got != revoked {
				t.Errorf("%s: ssh key %s revoked = %v, want %v", label, name, got, revoked)
			}
		}
	}
	check("after the migration")
	// A clock that steps now moves every created_at; the stored order does not move with it.
	exec(`UPDATE api_tokens SET created_at = created_at - interval '5 hours'`)
	exec(`UPDATE ssh_public_keys SET created_at = created_at + interval '5 hours'`)
	check("after a clock step")
}

// TestMigrate_RevocationOrderOrdersServiceTokensBySponsor (0176): a service identity's token is read against
// its current sponsor's cuts and the global one, so the backfill orders it against those. A token created
// after its sponsor's cut stays live, one created before a sponsor or global cut is ended, and after a
// sponsor change the token is ordered against the new sponsor while one older than the change stays refused
// by sponsor_since.
func TestMigrate_RevocationOrderOrdersServiceTokensBySponsor(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`DROP TRIGGER oidc_session_revocations_stamp ON oidc_session_revocations`)
	exec(`DROP TRIGGER oidc_session_cuts_stamp ON oidc_session_cuts`)
	exec(`ALTER SEQUENCE oidc_revocation_seq RESTART WITH 1`)
	// The global cut 4h ago, svc-alpha's sponsor cut 2h ago. svc-beta's sponsor moved from spon-old to
	// spon-new 90m ago: spon-new was cut 3h ago, spon-old 20m ago.
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at, seq) VALUES
		('', now() - interval '4 hours', 0), ('spon-alpha', now() - interval '2 hours', 0),
		('spon-new', now() - interval '3 hours', 0), ('spon-old', now() - interval '20 minutes', 0)`)
	exec(`INSERT INTO service_identities (name, sponsor, sponsor_since, created_by) VALUES
		('svc-alpha', 'spon-alpha', now() - interval '6 hours', 'admin'),
		('svc-beta', 'spon-new', now() - interval '90 minutes', 'admin')`)
	for _, c := range []struct{ name, service, created string }{
		{"before-global", "svc-alpha", "5 hours"},
		{"before-sponsor-cut", "svc-alpha", "3 hours"},
		{"after-sponsor-cut", "svc-alpha", "1 hour"},
		{"before-sponsor-change", "svc-beta", "2 hours"},
		{"after-sponsor-change", "svc-beta", "1 hour"},
	} {
		exec(`INSERT INTO api_tokens (id, principal, email, name, token_sha256, created_at)
			VALUES (gen_random_uuid(), 'service:' || $1, '', $2, $2, now() - $3::interval)`, c.service, c.name, c.created)
	}
	sql, err := migrationFS.ReadFile("migrations/0176_revocation_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(sql))

	// refused is what service-token authentication answers: not after sponsor_since, or a cut of the current
	// sponsor or the global one that drew a greater seq than the token holds.
	refused := func(name string) bool {
		t.Helper()
		var rev, cut int64
		var afterSince bool
		if err := pool.QueryRow(ctx, `SELECT t.rev_seq, COALESCE((SELECT max(r.seq) FROM oidc_session_revocations r
				WHERE r.sub = s.sponsor OR r.sub = ''), 0), t.created_at > s.sponsor_since
			FROM api_tokens t JOIN service_identities s ON t.principal = 'service:' || s.name WHERE t.name = $1`, name).Scan(&rev, &cut, &afterSince); err != nil {
			t.Fatal(err)
		}
		if rev < 1 {
			t.Fatalf("service token %s kept rev_seq %d: still compared on time", name, rev)
		}
		return !afterSince || cut > rev
	}
	for name, want := range map[string]bool{
		"before-global": true, "before-sponsor-cut": true, "after-sponsor-cut": false,
		"before-sponsor-change": true, "after-sponsor-change": false,
	} {
		if got := refused(name); got != want {
			t.Errorf("service token %s refused = %v, want %v", name, got, want)
		}
	}
}

// TestMigrate_RevocationOrderOrdersServiceTokensByTheirOwnPrincipal (0176): a service token is also read
// against the cuts of its own principal (the queued start's owner check), so a replacement token minted after
// a revoke of service:<name> stays live after the backfill, and one minted before it stays refused.
func TestMigrate_RevocationOrderOrdersServiceTokensByTheirOwnPrincipal(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`DROP TRIGGER oidc_session_revocations_stamp ON oidc_session_revocations`)
	exec(`DROP TRIGGER oidc_session_cuts_stamp ON oidc_session_cuts`)
	exec(`ALTER SEQUENCE oidc_revocation_seq RESTART WITH 1`)
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at, seq) VALUES ('service:svc-own', now() - interval '2 hours', 0)`)
	exec(`INSERT INTO service_identities (name, sponsor, sponsor_since, created_by) VALUES ('svc-own', 'spon-own', now() - interval '6 hours', 'admin')`)
	for _, c := range []struct{ name, created string }{{"own-before-revoke", "3 hours"}, {"own-after-revoke", "1 hour"}} {
		exec(`INSERT INTO api_tokens (id, principal, email, name, token_sha256, created_at)
			VALUES (gen_random_uuid(), 'service:svc-own', '', $1, $1, now() - $2::interval)`, c.name, c.created)
	}
	sql, err := migrationFS.ReadFile("migrations/0176_revocation_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(sql))
	// refused is what the queued start's owner check and the sponsor check answer together: a cut of the
	// token's own principal, its sponsor or the global one that drew a greater seq than the token holds.
	for name, want := range map[string]bool{"own-before-revoke": true, "own-after-revoke": false} {
		var rev, own, spon int64
		if err := pool.QueryRow(ctx, `SELECT t.rev_seq,
			COALESCE((SELECT max(r.seq) FROM oidc_session_revocations r WHERE r.sub = t.principal OR r.sub = ''), 0),
			COALESCE((SELECT max(r.seq) FROM oidc_session_revocations r WHERE r.sub = 'spon-own' OR r.sub = ''), 0)
			FROM api_tokens t WHERE t.name = $1`, name).Scan(&rev, &own, &spon); err != nil {
			t.Fatal(err)
		}
		if got := own > rev || spon > rev; got != want {
			t.Errorf("service token %s refused = %v (rev_seq %d, own-principal cut %d, sponsor cut %d), want %v", name, got, rev, own, spon, want)
		}
	}
}

// TestMigrate_RevocationOrderTokenBeforeTheOldestCut (0176): a token or key created before the oldest cut
// has no earlier cut to take a seq from. It takes the value reserved below every cut, so the oldest cut,
// which draws the first seq the migration hands out, still ends it.
func TestMigrate_RevocationOrderTokenBeforeTheOldestCut(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`DROP TRIGGER oidc_session_revocations_stamp ON oidc_session_revocations`)
	exec(`DROP TRIGGER oidc_session_cuts_stamp ON oidc_session_cuts`)
	exec(`ALTER SEQUENCE oidc_revocation_seq RESTART WITH 1`)
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at, seq) VALUES ('dave', now() - interval '4 hours', 0)`)
	exec(`INSERT INTO api_tokens (id, principal, name, token_sha256, created_at) VALUES (gen_random_uuid(), 'dave', 't', 't', now() - interval '5 hours')`)
	exec(`INSERT INTO ssh_public_keys (fingerprint, principal, name, public_key, created_at) VALUES ('k', 'dave', 'k', 'k', now() - interval '5 hours')`)
	sql, err := migrationFS.ReadFile("migrations/0176_revocation_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(sql))
	for _, table := range []string{"api_tokens", "ssh_public_keys"} {
		var rev, cut int64
		if err := pool.QueryRow(ctx, `SELECT (SELECT rev_seq FROM `+table+` WHERE principal = 'dave'),
			(SELECT seq FROM oidc_session_revocations WHERE sub = 'dave')`).Scan(&rev, &cut); err != nil {
			t.Fatal(err)
		}
		if rev < 1 || cut <= rev {
			t.Errorf("%s created before the oldest cut: rev_seq %d, the cut's seq %d; want the cut to end it", table, rev, cut)
		}
	}
}

// TestMigrate_RevocationOrderIndexesTokensByLowerEmail (0176): the per-person token revocation matches a
// name against principal and lower(email); both columns have an index, so it does not read the table.
func TestMigrate_RevocationOrderIndexesTokensByLowerEmail(t *testing.T) {
	pool := pgPoolIsolated(t)
	for idx, col := range map[string]string{"api_tokens_principal_idx": "principal", "api_tokens_lower_email_idx": "lower(email)"} {
		var def string
		if err := pool.QueryRow(context.Background(),
			`SELECT indexdef FROM pg_indexes WHERE tablename = 'api_tokens' AND indexname = $1`, idx).Scan(&def); err != nil {
			t.Fatalf("index %s: %v", idx, err)
		}
		if !strings.Contains(def, "("+col+")") {
			t.Errorf("index %s = %q, want it on %s", idx, def, col)
		}
	}
}
