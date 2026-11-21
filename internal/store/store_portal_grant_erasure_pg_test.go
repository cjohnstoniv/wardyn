// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mintDirect is one MintPortalGrant for an identity that already exists (it does not issue one).
func mintDirect(st store.PG, p issuerPortal, person string) error {
	_, _, err := st.MintPortalGrant(context.Background(), types.PortalGrant{
		DelegateID: p.Delegate.ID, Principal: types.PortalPrincipal(p.Key, person), UserType: "default",
		SubjectIssuedAt: time.Now().UTC(),
	}, "wdg_"+uuid.NewString(), time.Now().Add(time.Hour), 10*time.Minute, p.Delegate.AdmissionRev)
	return err
}

// pidOf is the backend process id of the session tx runs on.
func pidOf(t *testing.T, tx pgx.Tx) int {
	t.Helper()
	var pid int
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// untilBlockedBy returns once n sessions are each waiting on the session holder, as PostgreSQL itself reports
// (pg_blocking_pids), and gives up after ten seconds. It polls that wait, not a clock, so the test releases
// the holder only when every write it means to order is queued behind it.
func untilBlockedBy(t *testing.T, pool *pgxpool.Pool, holder, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		got = rowCount(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, holder)
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%d session(s) wait on the holder, want %d: the write does not take the lock the test holds", got, n)
}

// holdIdentity opens a transaction that holds the identity row of principal in the given mode (FOR SHARE or
// FOR UPDATE), standing for the other write that is in flight.
func holdIdentity(t *testing.T, pool *pgxpool.Pool, principal, mode string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM principal_identities WHERE principal = $1 `+mode, principal).Scan(&id); err != nil {
		t.Fatalf("hold the identity %s: %v", mode, err)
	}
	return tx
}

// holdPerson opens a transaction that holds the per-person lock the mint and the credential erase share (the
// key is the one portalPersonLockKey spells), whether or not the person has an identity row yet.
func holdPerson(t *testing.T, pool *pgxpool.Pool, principal string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('portal-person:' || $1::text, 0))`, principal); err != nil {
		t.Fatal(err)
	}
	return tx
}

// writeGrantAndHistory is what a mint in flight has written: a live grant for principal and the use of portal p
// recorded for identity id.
func writeGrantAndHistory(t *testing.T, tx pgx.Tx, p issuerPortal, id uuid.UUID, principal string) {
	t.Helper()
	ctx := context.Background()
	if _, err := tx.Exec(ctx, `INSERT INTO portal_grants (id, delegate_id, token_sha256, principal, user_type, chain_id, subject_issued_at, created_at, expires_at)
		VALUES ($1, $2, $3, $4, 'default', $1, now(), now(), now() + interval '10 minutes')`, uuid.New(), p.Delegate.ID, sha256Hex(uuid.NewString()), principal); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO portal_person_portals (identity_id, delegate_id) VALUES ($1, $2)
		ON CONFLICT (identity_id, delegate_id) DO UPDATE SET last_used_at = clock_timestamp()`, id, p.Delegate.ID); err != nil {
		t.Fatal(err)
	}
}

type portalState struct{ grants, history int }

func stateOf(t *testing.T, pool *pgxpool.Pool, principal string) portalState {
	t.Helper()
	return portalState{
		grants: rowCount(t, pool, `SELECT count(*) FROM portal_grants WHERE principal = $1`, principal),
		history: rowCount(t, pool, `SELECT count(*) FROM portal_person_portals u JOIN principal_identities i ON i.id = u.identity_id
			WHERE i.principal = $1`, principal),
	}
}

// A person whose purge commits after the exchange validated them but before it mints is refused at the mint:
// the mint reads the identity row under the lock the purge holds, finds it purged, and writes no grant and no
// history. Lock order (mint): the issuer binding FOR KEY SHARE, the portal's row FOR SHARE, the per-person
// advisory lock, the identity row FOR SHARE. Purge: the identity row FOR UPDATE only, so the two cannot wait on
// each other in a cycle.
func TestPortalGrant_MintAfterPurgeRefusedWritesNothing_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	issueFor(t, st, p, "alice")
	principal := types.PortalPrincipal(p.Key, "alice")

	// The purge is in flight: the row is locked, tombstoned and its history deleted, and not yet committed.
	purge, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = purge.Rollback(ctx) })
	for _, stmt := range []string{
		`SELECT set_config('wardyn.portal_lifecycle', 'v1', true)`,
		`SELECT 1 FROM principal_identities WHERE principal = '` + principal + `' FOR UPDATE`,
		`UPDATE principal_identities SET purged_at = clock_timestamp(), display_email = '' WHERE principal = '` + principal + `'`,
		`DELETE FROM portal_person_portals WHERE identity_id IN (SELECT id FROM principal_identities WHERE principal = '` + principal + `')`,
	} {
		if _, err := purge.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- mintDirect(st, p, "alice") }()
	untilBlockedBy(t, pool, pidOf(t, purge), 1)
	if err := purge.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrPortalPersonPurged) {
		t.Fatalf("a mint that waited for the purge = %v, want ErrPortalPersonPurged", err)
	}
	if got := stateOf(t, pool, principal); got != (portalState{}) {
		t.Errorf("after the refused mint: %+v, want no grant and no history", got)
	}
	if err := mintDirect(st, p, "alice"); !errors.Is(err, store.ErrPortalPersonPurged) {
		t.Errorf("a mint after the purge = %v, want ErrPortalPersonPurged", err)
	}
	if got := portalsOf(t, st, p, "alice"); len(got) != 0 {
		t.Errorf("a purged person lists portals %+v", got)
	}
}

// A mint that commits before the purge: the purge waits for the mint's share lock, then deletes the history it
// finds, and the credential erase the purge runs deletes the grant. Nothing of the person is left.
func TestPortalGrant_MintBeforePurgeLeavesNoHistory_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	id, _ := issueFor(t, st, p, "alice")
	principal := types.PortalPrincipal(p.Key, "alice")
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: id, Principal: principal, Principals: []string{principal}, PortalLifecycle: true}); err != nil {
		t.Fatal(err)
	}

	// The mint is in flight: it holds the identity row FOR SHARE and has written its grant and history.
	mint := holdIdentity(t, pool, principal, "FOR SHARE")
	writeGrantAndHistory(t, mint, p, id, principal)
	type result struct {
		marked bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		marked, err := st.MarkPortalIdentityPurged(ctx, id, store.PortalPurgeExplicit)
		done <- result{marked, err}
	}()
	untilBlockedBy(t, pool, pidOf(t, mint), 1)
	if err := mint.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || !r.marked {
		t.Fatalf("purge = %v, %v, want it marked", r.marked, r.err)
	}
	if got := stateOf(t, pool, principal); got.history != 0 {
		t.Errorf("after the purge: %+v, want no history", got)
	}
	if _, err := st.DeleteDelegatedTokensOf(ctx, []string{principal}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, pool, principal); got != (portalState{}) {
		t.Errorf("after the purge and the credential erase: %+v, want nothing", got)
	}
	if err := mintDirect(st, p, "alice"); !errors.Is(err, store.ErrPortalPersonPurged) {
		t.Errorf("a mint after the purge = %v, want ErrPortalPersonPurged", err)
	}
}

// A credential erase that holds the person's lock first makes every concurrent mint wait, through the same
// portal or another portal of the issuer, and the mints then write their grants and their histories after it.
// Lock order (erase): the per-person locks in lock-key order, then the deletes; it takes no binding, portal,
// advisory-per-portal or identity lock, so it cannot wait on a mint that waits on it.
func TestPortalGrant_EraseBeforeMintKeepsGrantAndHistoryTogether_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	other := newIssuerPortalOn(t, st, p)
	issueFor(t, st, p, "alice")
	principal := types.PortalPrincipal(p.Key, "alice")
	if err := mintDirect(st, p, "alice"); err != nil {
		t.Fatal(err)
	}

	erase := holdPerson(t, pool, principal)
	for _, stmt := range []string{
		`DELETE FROM portal_grants WHERE principal = '` + principal + `'`,
		`DELETE FROM portal_person_portals WHERE identity_id IN (SELECT id FROM principal_identities WHERE principal = '` + principal + `')`,
	} {
		if _, err := erase.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 2)
	go func() { done <- mintDirect(st, p, "alice") }()
	go func() { done <- mintDirect(st, other, "alice") }()
	untilBlockedBy(t, pool, pidOf(t, erase), 2) // both mints, one per portal, wait on the erase
	if err := erase.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("a mint that waited for the erase = %v", err)
		}
	}
	if got := stateOf(t, pool, principal); got != (portalState{grants: 2, history: 2}) {
		t.Errorf("after the erase and two mints: %+v, want 2 grants and 2 history rows", got)
	}
}

// A first introduction: the erase has looked and found no identity row, and deleted nothing, when the person's
// first exchange creates the identity and mints. The mint waits for the erase's lock and then writes the grant
// with its history; the person ends with both, never a grant without its history.
func TestPortalGrant_FirstIntroductionAfterEraseKeepsGrantAndHistory_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	principal := types.PortalPrincipal(p.Key, "alice")
	if n := rowCount(t, pool, `SELECT count(*) FROM principal_identities WHERE principal = $1`, principal); n != 0 {
		t.Fatalf("%d identity rows before the first exchange", n)
	}

	erase := holdPerson(t, pool, principal)
	if _, err := erase.Exec(ctx, `DELETE FROM portal_grants WHERE principal = $1`, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := erase.Exec(ctx, `DELETE FROM portal_person_portals WHERE identity_id IN (SELECT id FROM principal_identities WHERE principal = $1)`, principal); err != nil {
		t.Fatal(err)
	}
	issueFor(t, st, p, "alice") // the first exchange creates the identity after the erase looked
	done := make(chan error, 1)
	go func() { done <- mintDirect(st, p, "alice") }()
	untilBlockedBy(t, pool, pidOf(t, erase), 1)
	if err := erase.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the first mint behind an erase = %v", err)
	}
	if got := stateOf(t, pool, principal); got != (portalState{grants: 1, history: 1}) {
		t.Errorf("after the erase and the first mint: %+v, want one grant and its history", got)
	}
}

// A mint that holds the person's lock first makes a concurrent erase wait, and the erase then deletes the grant
// and the history it wrote: either both survive or neither does. It holds for a first introduction (the erase
// started when no identity row existed) and for a person who has used the portal before (the history row is
// updated in place).
func TestPortalGrant_MintBeforeEraseLeavesNeither_PG(t *testing.T) {
	for _, c := range []struct {
		name  string
		first bool
		want  int64
	}{{"first introduction", true, 1}, {"repeat use", false, 2}} {
		t.Run(c.name, func(t *testing.T) {
			st, pool := isolatedStore(t)
			ctx := context.Background()
			p := newIssuerPortal(t, st, "")
			principal := types.PortalPrincipal(p.Key, "alice")
			if !c.first {
				issueFor(t, st, p, "alice")
				if err := mintDirect(st, p, "alice"); err != nil {
					t.Fatal(err)
				}
			}

			mint := holdPerson(t, pool, principal)
			type result struct {
				n   int64
				err error
			}
			done := make(chan result, 1)
			go func() {
				n, err := st.DeleteDelegatedTokensOf(ctx, []string{principal})
				done <- result{n, err}
			}()
			untilBlockedBy(t, pool, pidOf(t, mint), 1)
			id, _ := issueFor(t, st, p, "alice") // a first exchange creates the identity while the erase waits
			writeGrantAndHistory(t, mint, p, id, principal)
			if err := mint.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if r := <-done; r.err != nil || r.n != c.want {
				t.Fatalf("erase = %d, %v, want %d grants", r.n, r.err, c.want)
			}
			if got := stateOf(t, pool, principal); got != (portalState{}) {
				t.Errorf("after the mint and the erase: %+v, want neither a grant nor history", got)
			}
		})
	}
}
