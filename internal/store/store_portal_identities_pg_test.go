// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func issueFor(t *testing.T, st store.PG, p issuerPortal, person string) (uuid.UUID, bool) {
	t.Helper()
	id, created, err := st.IssuePortalIdentity(context.Background(), types.PortalPrincipal(p.Key, person), p.Key,
		person+"@example.com", p.Issuer, p.Delegate.ID, p.Delegate.AdmissionRev)
	if err != nil {
		t.Fatalf("issue %s: %v", person, err)
	}
	return id, created
}

// markedExec runs one statement in a transaction that carries the portal lifecycle marker.
func markedExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('wardyn.portal_lifecycle', 'v1', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Many first exchanges of one person, through two portals on one issuer, leave one identity row, one
// introducer, and exactly one call told it created the row.
func TestIssuePortalIdentity_ConcurrentFirstExchangesOneIntroducer_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	a := newIssuerPortal(t, st, "")
	b := newIssuerPortalOn(t, st, a)
	portals := []issuerPortal{a, b}
	const n = 10
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	created := make([]bool, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := portals[i%2]
			ids[i], created[i], errs[i] = st.IssuePortalIdentity(context.Background(), types.PortalPrincipal(a.Key, "alice"), a.Key,
				"alice@example.com", a.Issuer, p.Delegate.ID, p.Delegate.AdmissionRev)
		}()
	}
	wg.Wait()
	introducer := -1
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("exchange %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("exchange %d got identity %s, want %s", i, ids[i], ids[0])
		}
		if created[i] {
			if introducer >= 0 {
				t.Fatalf("exchanges %d and %d both report creating the row", introducer, i)
			}
			introducer = i
		}
	}
	if introducer < 0 {
		t.Fatal("no exchange reports creating the row")
	}
	var by uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT introduced_by FROM principal_identities WHERE id = $1`, ids[0]).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if want := portals[introducer%2].Delegate.ID; by != want {
		t.Fatalf("introduced_by = %s, want the creating exchange's portal %s", by, want)
	}
	if got := rowCount(t, pool, `SELECT count(*) FROM principal_identities WHERE principal = $1`, types.PortalPrincipal(a.Key, "alice")); got != 1 {
		t.Fatalf("%d identity rows, want 1", got)
	}
}

// A deactivated or purged person gets no identity, and a retired or revoked portal, or one whose admission
// revision moved, cannot introduce anyone.
func TestIssuePortalIdentity_RefusesPurgedAndDeactivated_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	id, created := issueFor(t, st, p, "alice")
	if !created {
		t.Fatal("first issue did not create")
	}
	if again, created := issueFor(t, st, p, "alice"); again != id || created {
		t.Fatalf("second issue = %s, %v; want %s, false", again, created, id)
	}

	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	issue := func(person string, rev int64) error {
		_, _, err := st.IssuePortalIdentity(ctx, types.PortalPrincipal(p.Key, person), p.Key, "", p.Issuer, p.Delegate.ID, rev)
		return err
	}
	if err := issue("alice", p.Delegate.AdmissionRev); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Fatalf("deactivated person = %v, want ErrIdentityDeactivated", err)
	}
	if marked, err := st.MarkPortalIdentityPurged(ctx, id, store.PortalPurgeExplicit); err != nil || !marked {
		t.Fatalf("purge = %v, %v", marked, err)
	}
	if err := issue("alice", p.Delegate.AdmissionRev); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Fatalf("purged person = %v, want ErrIdentityDeactivated", err)
	}
	if err := issue("bob", p.Delegate.AdmissionRev+1); !errors.Is(err, store.ErrAdmissionChanged) {
		t.Fatalf("moved admission revision = %v, want ErrAdmissionChanged", err)
	}
	if got := rowCount(t, pool, `SELECT count(*) FROM principal_identities WHERE principal = $1`, types.PortalPrincipal(p.Key, "bob")); got != 0 {
		t.Fatalf("a refused introduction left %d rows", got)
	}
	if _, _, err := st.RevokeDelegate(ctx, p.Delegate.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if err := issue("bob", p.Delegate.AdmissionRev); !errors.Is(err, store.ErrPortalGone) {
		t.Fatalf("revoked portal = %v, want ErrPortalGone", err)
	}
}

func pendingFor(t *testing.T, st store.PG, id uuid.UUID) (store.PortalIntroduction, bool) {
	t.Helper()
	list, err := st.PendingPortalIntroductions(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list {
		if in.IdentityID == id {
			return in, true
		}
	}
	return store.PortalIntroduction{}, false
}

// The pending list carries every field of the event as stored, the same on every read, until the event is
// marked delivered.
func TestPendingPortalIntroductions_ReemitFromPersistedFields_PG(t *testing.T) {
	st, _ := isolatedStore(t)
	p := newIssuerPortal(t, st, "")
	id, _ := issueFor(t, st, p, "alice")

	first, ok := pendingFor(t, st, id)
	if !ok {
		t.Fatal("a new person's introduction is not pending")
	}
	if first.Principal != types.PortalPrincipal(p.Key, "alice") || first.IssuerKey != p.Key || first.PortalID != p.Delegate.ID ||
		first.Issuer != p.Issuer || first.EventID == uuid.Nil || first.EventAt.IsZero() {
		t.Fatalf("pending introduction = %+v", first)
	}
	issueFor(t, st, p, "alice") // a later exchange of the same person changes nothing
	second, _ := pendingFor(t, st, id)
	if second.EventID != first.EventID || !second.EventAt.Equal(first.EventAt) {
		t.Fatalf("a later read = %+v, want the same event %+v", second, first)
	}
	if marked, err := st.MarkPortalIntroductionAudited(context.Background(), id); err != nil || !marked {
		t.Fatalf("mark = %v, %v", marked, err)
	}
	if _, ok := pendingFor(t, st, id); ok {
		t.Fatal("a delivered introduction is still pending")
	}
	if marked, err := st.MarkPortalIntroductionAudited(context.Background(), id); err != nil || marked {
		t.Fatalf("second mark = %v, %v; want false, nil", marked, err)
	}
}

// One person's own owed introduction is found directly, however many older introductions are owed ahead of
// it, and is gone once delivered; an id that is not a portal person's is never owed.
func TestPendingPortalIntroductionFor_FindsOwnBehindBacklog_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	for i := 0; i < 101; i++ {
		issueFor(t, st, p, fmt.Sprintf("backlog-%03d", i))
	}
	target, _ := issueFor(t, st, p, "target")
	if _, ok := pendingFor(t, st, target); ok {
		t.Fatal("the target is inside the first 100 pending rows, so the backlog proves nothing")
	}

	got, ok, err := st.PendingPortalIntroductionFor(ctx, target)
	if err != nil || !ok {
		t.Fatalf("PendingPortalIntroductionFor = %+v, %v, %v; want the target's introduction", got, ok, err)
	}
	var wantID uuid.UUID
	var wantAt time.Time
	if err := pool.QueryRow(ctx, `SELECT introduction_event_id, introduction_event_at FROM principal_identities WHERE id = $1`, target).Scan(&wantID, &wantAt); err != nil {
		t.Fatal(err)
	}
	if got.IdentityID != target || got.EventID != wantID || !got.EventAt.Equal(wantAt) || got.Principal != types.PortalPrincipal(p.Key, "target") ||
		got.IssuerKey != p.Key || got.PortalID != p.Delegate.ID || got.Issuer != p.Issuer {
		t.Fatalf("introduction = %+v, want the stored event %v at %v", got, wantID, wantAt)
	}

	if marked, err := st.MarkPortalIntroductionAudited(ctx, target); err != nil || !marked {
		t.Fatalf("mark = %v, %v", marked, err)
	}
	if got, ok, err := st.PendingPortalIntroductionFor(ctx, target); err != nil || ok {
		t.Fatalf("after delivery = %+v, %v, %v; want false", got, ok, err)
	}

	var workforce uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ('wf-sub', 'https://workforce.example.com', 'wf@example.com') RETURNING id`).Scan(&workforce); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{workforce, uuid.New()} {
		if got, ok, err := st.PendingPortalIntroductionFor(ctx, id); err != nil || ok {
			t.Fatalf("PendingPortalIntroductionFor(%v) = %+v, %v, %v; want false", id, got, ok, err)
		}
	}
}

func replayIntroduction(t *testing.T, pool *pgxpool.Pool, in store.PortalIntroduction) {
	t.Helper()
	ev := types.AuditEvent{ID: in.EventID, Time: in.EventAt, ActorType: types.ActorSystem, Actor: "portal:" + in.PortalID.String(),
		Action: "portal.person.introduced", Outcome: "success"}
	if err := store.InsertAuditEvent(store.WithAuditReplay(context.Background()), pool, &ev); err != nil {
		t.Fatal(err)
	}
}

// A crash between the append and the mark leaves the introduction pending; the replay appends nothing more,
// and the mark then clears it.
func TestPortalIntroduction_CrashAfterAppendBeforeMark_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	p := newIssuerPortal(t, st, "")
	id, _ := issueFor(t, st, p, "alice")
	in, _ := pendingFor(t, st, id)

	replayIntroduction(t, pool, in)
	// the process stops here: no mark
	again, ok := pendingFor(t, st, id)
	if !ok || again.EventID != in.EventID {
		t.Fatalf("after the crash the introduction is %+v, %v; want the same pending event", again, ok)
	}
	replayIntroduction(t, pool, again)
	if n := auditRowsWithID(t, pool, in.EventID); n != 1 {
		t.Fatalf("%d audit rows for the event after the replay, want 1", n)
	}
	if marked, err := st.MarkPortalIntroductionAudited(context.Background(), id); err != nil || !marked {
		t.Fatalf("mark = %v, %v", marked, err)
	}
	if _, ok := pendingFor(t, st, id); ok {
		t.Fatal("still pending after the mark")
	}
}

// However many times one introduction is replayed, the audit has one row for it, with the id and time the
// row persisted.
func TestPortalIntroduction_ReplayOneRowSameIDCopies_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	p := newIssuerPortal(t, st, "")
	id, _ := issueFor(t, st, p, "alice")
	in, _ := pendingFor(t, st, id)
	for range 3 {
		cur, _ := pendingFor(t, st, id)
		replayIntroduction(t, pool, cur)
	}
	var at time.Time
	if err := pool.QueryRow(context.Background(), `SELECT count(*), max(time) FROM audit_events WHERE id = $1`, in.EventID).Scan(new(int), &at); err != nil {
		t.Fatal(err)
	}
	if n := auditRowsWithID(t, pool, in.EventID); n != 1 {
		t.Fatalf("%d audit rows for one introduction, want 1", n)
	}
	if !at.Equal(in.EventAt) {
		t.Fatalf("audit time %v, want the persisted %v", at, in.EventAt)
	}
}
