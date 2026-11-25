// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// deactivatedPerson returns a portal person's identity id with the row deactivated (a write the trigger allows
// unmarked).
func deactivatedPerson(t *testing.T, st store.PG, p issuerPortal, person string) uuid.UUID {
	t.Helper()
	id, _ := issueFor(t, st, p, person)
	if _, err := st.Pool.Exec(context.Background(), `UPDATE principal_identities SET deactivated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("an unmarked deactivation was refused: %v", err)
	}
	return id
}

// Without the marker, a portal person's purge deadline, reactivation and tombstone are refused, whatever
// SQL sends them; the deactivation itself is not.
func TestPortalLifecycleTrigger_RefusesUnmarkedDeadlineAndTombstone_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	id := deactivatedPerson(t, st, p, "alice")

	for name, sql := range map[string]string{
		"set a deadline":    `UPDATE principal_identities SET purge_after = now() + interval '1 day' WHERE id = $1`,
		"reactivate":        `UPDATE principal_identities SET deactivated_at = NULL WHERE id = $1`,
		"write a tombstone": `UPDATE principal_identities SET purged_at = now() WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, id); !isCheckViolation(err) {
			t.Errorf("unmarked %s = %v, want a check violation", name, err)
		}
	}
	var deactivated bool
	if err := pool.QueryRow(ctx, `SELECT deactivated_at IS NOT NULL AND purge_after IS NULL AND purged_at IS NULL FROM principal_identities WHERE id = $1`, id).Scan(&deactivated); err != nil || !deactivated {
		t.Fatalf("the refused writes changed the row: %v, %v", deactivated, err)
	}
}

// A purge blanks the display email in the same statement, the blank holds against a later write and against
// the migration running again, and a purged person is not introduced again.
func TestPortalPurge_DisplayEmailBlankAfterPurgeAndReupgrade_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	id := deactivatedPerson(t, st, p, "alice")
	var shown string
	if err := pool.QueryRow(ctx, `SELECT display_email FROM principal_identities WHERE id = $1`, id).Scan(&shown); err != nil || shown == "" {
		t.Fatalf("display email before the purge = %q, %v", shown, err)
	}
	if marked, err := st.MarkPortalIdentityPurged(ctx, id, store.PortalPurgeExplicit); err != nil || !marked {
		t.Fatalf("purge = %v, %v", marked, err)
	}
	blank := func(when string) {
		t.Helper()
		var email string
		var purged bool
		if err := pool.QueryRow(ctx, `SELECT display_email, purged_at IS NOT NULL FROM principal_identities WHERE id = $1`, id).Scan(&email, &purged); err != nil {
			t.Fatal(err)
		}
		if email != "" || !purged {
			t.Fatalf("%s: display_email = %q, purged = %v; want blank and purged", when, email, purged)
		}
	}
	blank("after the purge")

	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET display_email = 'back@example.com' WHERE id = $1`, id); !isCheckViolation(err) {
		t.Fatalf("writing an email to a tombstone = %v, want a check violation", err)
	}
	if err := markedExec(t, pool, `UPDATE principal_identities SET display_email = 'back@example.com' WHERE id = $1`, id); !isCheckViolation(err) {
		t.Fatalf("writing an email to a tombstone under the marker = %v, want a check violation", err)
	}
	if err := markedExec(t, pool, `UPDATE principal_identities SET purged_at = NULL WHERE id = $1`, id); !isCheckViolation(err) {
		t.Fatalf("clearing a tombstone under the marker = %v, want a check violation", err)
	}
	blank("after the refused writes")

	sql, err := os.ReadFile("../db/migrations/0294_portal_issuer.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("running the migration again: %v", err)
	}
	blank("after the migration ran again")
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET purged_at = NULL WHERE id = $1`, id); !isCheckViolation(err) {
		t.Fatalf("clearing a tombstone after the migration ran again = %v, want a check violation", err)
	}
	if _, _, err := st.IssuePortalIdentity(ctx, types.PortalPrincipal(p.Key, "alice"), p.Key, "again@example.com", p.Issuer, p.Delegate.ID, p.Delegate.AdmissionRev); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Fatalf("introducing a purged person = %v, want ErrIdentityDeactivated", err)
	}
	blank("after the refused introduction")
}

// The trigger's rule for each write, with the marker absent and set, on rows that start with the portal
// issuer prefix, including the INSERT of a row; a workforce row is never touched by it.
func TestPortalLifecycleTrigger_MarkerAbsentAndSet_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	key := "cccccccccccccccccccccccccc"
	insert := func(marked bool) error {
		sql := `INSERT INTO principal_identities (principal, issuer) VALUES ($1, $2)`
		args := []any{"delegate:person:" + key + ":" + uuid.NewString(), "delegate:person:" + key}
		if marked {
			return markedExec(t, pool, sql, args...)
		}
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	// Inserts: the row starts clean, marked or not.
	for _, marked := range []bool{false, true} {
		if err := insert(marked); err != nil {
			t.Errorf("marked=%v: a clean insert = %v", marked, err)
		}
	}
	for name, cols := range map[string]string{
		"deactivated": `, deactivated_at) VALUES ($1, $2, now()`,
		"deadline":    `, purge_after) VALUES ($1, $2, now()`,
		"tombstone":   `, purged_at) VALUES ($1, $2, now()`,
		"email_lower": `, email_lower) VALUES ($1, $2, 'a@example.com'`,
	} {
		for _, marked := range []bool{false, true} {
			sql := `INSERT INTO principal_identities (principal, issuer` + cols + `)`
			args := []any{"delegate:person:" + key + ":" + uuid.NewString(), "delegate:person:" + key}
			var err error
			if marked {
				err = markedExec(t, pool, sql, args...)
			} else {
				_, err = pool.Exec(ctx, sql, args...)
			}
			if !isCheckViolation(err) {
				t.Errorf("marked=%v: an insert with %s = %v, want a check violation", marked, name, err)
			}
		}
	}
	// A workforce row may start deactivated and be updated freely.
	wf := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO principal_identities (principal, issuer, deactivated_at, email_lower) VALUES ($1, 'https://workforce.example.com', now(), 'w@example.com')`, wf); err != nil {
		t.Errorf("a workforce insert = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET purge_after = now(), deactivated_at = NULL WHERE principal = $1`, wf); err != nil {
		t.Errorf("an unmarked workforce update = %v", err)
	}

	// Updates of a live portal person.
	p := newIssuerPortal(t, st, "")
	id, _ := issueFor(t, st, p, "alice")
	type update struct{ name, sql string }
	for _, u := range []update{
		{"set a deadline", `UPDATE principal_identities SET deactivated_at = now(), purge_after = now() + interval '1 day' WHERE id = $1`},
	} {
		if _, err := pool.Exec(ctx, u.sql, id); !isCheckViolation(err) {
			t.Errorf("unmarked %s = %v, want a check violation", u.name, err)
		}
		if err := markedExec(t, pool, u.sql, id); err != nil {
			t.Errorf("marked %s = %v, want it allowed", u.name, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET email_lower = 'x@example.com' WHERE id = $1`, id); !isCheckViolation(err) {
		t.Errorf("unmarked email_lower = %v, want a check violation", err)
	}
	if err := markedExec(t, pool, `UPDATE principal_identities SET email_lower = 'x@example.com' WHERE id = $1`, id); !isCheckViolation(err) {
		t.Errorf("marked email_lower = %v, want a check violation", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = NULL WHERE id = $1`, id); !isCheckViolation(err) {
		t.Errorf("unmarked reactivation = %v, want a check violation", err)
	}
	if err := markedExec(t, pool, `UPDATE principal_identities SET deactivated_at = NULL, purge_after = NULL WHERE id = $1`, id); err != nil {
		t.Errorf("marked reactivation = %v, want it allowed", err)
	}
	// An update that leaves the guarded columns alone needs no marker.
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET last_login_at = now() WHERE id = $1`, id); err != nil {
		t.Errorf("an unrelated unmarked update = %v", err)
	}
}

// An admin purge that waited behind a reactivation finds the row active under its lock and purges nothing.
func TestPortalPurge_ExplicitRefusedAfterConcurrentReactivate_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	id := deactivatedPerson(t, st, p, "alice")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('wardyn.portal_lifecycle', 'v1', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE principal_identities SET deactivated_at = NULL, purge_after = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	type result struct {
		marked bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		marked, err := st.MarkPortalIdentityPurged(ctx, id, store.PortalPurgeExplicit)
		done <- result{marked, err}
	}()
	untilBlockedBy(t, pool, pidOf(t, tx), 1) // the purge waits on the reactivation's row lock
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.marked || !errors.Is(r.err, store.ErrPersonNotSuspended) {
		t.Fatalf("purge after a reactivation = %v, %v; want false, ErrPersonNotSuspended", r.marked, r.err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM principal_identities WHERE id = $1 AND purged_at IS NULL AND deactivated_at IS NULL`, id); n != 1 {
		t.Fatal("the person is not active and unpurged")
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1`, id); n != 0 {
		t.Fatalf("a refused purge left %d jobs", n)
	}
}

// dueMalformedPerson inserts a person row under a bound portal's issuer prefix with the given principal and issuer
// (each call a row of its own), gives it portal history and a ledger, and makes it suspended and due under its own
// suspension: deactivated an hour ago with a deadline that has passed and the done cutoff step that proves it.
func dueMalformedPerson(t *testing.T, st store.PG, pool *pgxpool.Pool, p issuerPortal, issuer, principal string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer) VALUES ($1, $2) RETURNING id`, principal, issuer).Scan(&id); err != nil {
		t.Fatalf("insert %s: %v", principal, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO portal_person_portals (identity_id, delegate_id) VALUES ($1, $2)`, id, p.Delegate.ID); err != nil {
		t.Fatalf("history: %v", err)
	}
	if err := markedExec(t, pool, `UPDATE principal_identities SET deactivated_at = now() - interval '2 hours', purge_after = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, detail)
		SELECT id, 'suspend', 'cutoff', '', 'done', jsonb_build_object('deactivated_at_us', (extract(epoch FROM deactivated_at) * 1000000)::bigint)
		  FROM principal_identities WHERE id = $1`, id); err != nil {
		t.Fatalf("ledger: %v", err)
	}
	return id
}

// A row that carries the portal issuer prefix but is not a bound portal's person is never marked: the locked mark
// requires the bound issuer and a principal of that issuer before any write, in both modes, and says so
// (ErrNotPortalPerson). Each case and mode has a row of its own, suspended and due under its own suspension, with
// portal history and its first ledger row: the scheduled mode would mark it were it a bound portal's person, and
// all of it is as found after the refusal. A bound portal's person with the same suspension is marked, in both modes.
func TestPortalPurge_MarkRefusedForMalformedRows_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	unboundKey := "dddddddddddddddddddddddddd"
	cases := map[string]func() (issuer, principal string){
		"an issuer no binding names": func() (string, string) {
			return "delegate:person:" + unboundKey, "delegate:person:" + unboundKey + ":" + uuid.NewString()
		},
		"a principal of another issuer": func() (string, string) {
			return "delegate:person:" + p.Key, "delegate:person:" + unboundKey + ":" + uuid.NewString()
		},
		"a principal that is not a portal's": func() (string, string) {
			return "delegate:person:" + p.Key, "workforce-" + uuid.NewString()
		},
	}
	for name, mk := range cases {
		for _, mode := range []store.PortalPurgeMode{store.PortalPurgeScheduled, store.PortalPurgeExplicit} {
			issuer, principal := mk()
			id := dueMalformedPerson(t, st, pool, p, issuer, principal)
			type snap struct {
				Deactivated, PurgeAfter, Purged bool
				History, Ledger                 int
			}
			read := func() (out snap) {
				if err := pool.QueryRow(ctx, `SELECT deactivated_at IS NOT NULL, purge_after IS NOT NULL, purged_at IS NOT NULL,
					(SELECT count(*) FROM portal_person_portals WHERE identity_id = $1), (SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1)
					FROM principal_identities WHERE id = $1`, id).Scan(&out.Deactivated, &out.PurgeAfter, &out.Purged, &out.History, &out.Ledger); err != nil {
					t.Fatal(err)
				}
				return out
			}
			before := read()
			if before != (snap{Deactivated: true, PurgeAfter: true, History: 1, Ledger: 1}) {
				t.Fatalf("%s, mode %v: control: the fixture is %+v", name, mode, before)
			}
			if marked, err := st.MarkPortalIdentityPurged(ctx, id, mode); marked || !errors.Is(err, store.ErrNotPortalPerson) {
				t.Errorf("%s, mode %v: mark = %v, %v; want false, ErrNotPortalPerson", name, mode, marked, err)
			}
			if after := read(); after != before {
				t.Errorf("%s, mode %v: the row changed: %+v -> %+v", name, mode, before, after)
			}
		}
	}
	// The control: a bound portal's person in the same state (a real one, suspended and due) is marked in both modes.
	for _, mode := range []store.PortalPurgeMode{store.PortalPurgeScheduled, store.PortalPurgeExplicit} {
		person := "ctl-" + uuid.NewString()
		id := dueMalformedPerson(t, st, pool, p, "delegate:person:"+p.Key, types.PortalPrincipal(p.Key, person))
		if marked, err := st.MarkPortalIdentityPurged(ctx, id, mode); err != nil || !marked {
			t.Errorf("control, mode %v: a bound portal person's mark = %v, %v; want true", mode, marked, err)
		}
		if n := rowCount(t, pool, `SELECT count(*) FROM principal_identities WHERE id = $1 AND purged_at IS NOT NULL`, id); n != 1 {
			t.Errorf("control, mode %v: the person is not tombstoned", mode)
		}
	}
}
