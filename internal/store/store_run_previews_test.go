// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// previewFixture is one throwaway database with the preview store over it. RevokeRunPreviewsAbove reaches every
// run's rows, so each test has its own database.
type previewFixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	pg   store.PG
}

func newPreviewFixture(t *testing.T) previewFixture {
	pool := runsPGPoolIsolated(t)
	return previewFixture{t: t, ctx: context.Background(), pool: pool, pg: store.NewPG(pool)}
}

// run persists a RUNNING run with a person of its own as creator.
func (f previewFixture) run() types.AgentRun {
	f.t.Helper()
	r := newRun(types.RunRunning)
	r.CreatedBy = "owner-" + uuid.NewString()
	return persistRun(f.t, f.ctx, f.pool, r)
}

func (f previewFixture) upsert(r types.AgentRun, port int, vis types.PreviewVisibility, ttl time.Duration, hash []byte) (types.RunPreview, bool) {
	f.t.Helper()
	p, created, err := f.pg.UpsertRunPreview(f.ctx, r.ID, port, vis, ttl, r.CreatedBy, hash)
	if err != nil {
		f.t.Fatalf("UpsertRunPreview(%d, %s): %v", port, vis, err)
	}
	if p.RunID != r.ID || p.Port != port || p.Visibility != vis || p.CreatedBy != r.CreatedBy || p.RevokedAt != nil {
		f.t.Fatalf("UpsertRunPreview(%d, %s) = %+v", port, vis, p)
	}
	return p, created
}

func (f previewFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// revokedBy reads a row's revocation: whether it is revoked, and by whom.
func (f previewFixture) revokedBy(id uuid.UUID) (bool, string) {
	f.t.Helper()
	var revoked bool
	var by *string
	if err := f.pool.QueryRow(f.ctx, `SELECT revoked_at IS NOT NULL, revoked_by FROM run_previews WHERE id = $1`, id).Scan(&revoked, &by); err != nil {
		f.t.Fatalf("read preview %s: %v", id, err)
	}
	if by == nil {
		return revoked, ""
	}
	return revoked, *by
}

func (f previewFixture) runUpdatedAt(r types.AgentRun) time.Time {
	f.t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT updated_at FROM agent_runs WHERE id = $1`, r.ID).Scan(&at); err != nil {
		f.t.Fatalf("read run %s: %v", r.ID, err)
	}
	return at
}

func (f previewFixture) expire(id uuid.UUID) {
	f.t.Helper()
	f.exec(`UPDATE run_previews SET expires_at = now() - interval '1 second' WHERE id = $1`, id)
}

// pgCode is err's Postgres error code and constraint, empty when it is not a Postgres error.
func previewPGCode(err error) (code, constraint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

func previewIDs(ps []types.RunPreview) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}

func sortedPreviewIDs(in ...uuid.UUID) []uuid.UUID {
	slices.SortFunc(in, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return in
}

func previewLinkHash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// TestStoreRunPreviewsOneLivePerPort pins one live row per (run, port): the same visibility extends the row,
// another visibility revokes it and inserts a new one, an expired row is never extended, and none of it writes
// agent_runs.
func TestStoreRunPreviewsOneLivePerPort(t *testing.T) {
	f := newPreviewFixture(t)
	r := f.run()
	before := f.runUpdatedAt(r)

	p1, created := f.upsert(r, 8080, types.PreviewPrivate, time.Hour, nil)
	if !created || p1.ID == uuid.Nil || p1.ExpiresAt.Sub(p1.CreatedAt) != time.Hour {
		t.Fatalf("create = %+v, created %v; want a new row expiring an hour after it was made", p1, created)
	}
	p2, created := f.upsert(r, 8080, types.PreviewPrivate, 2*time.Hour, nil)
	if created || p2.ID != p1.ID || p2.ExpiresAt.Sub(p2.UpdatedAt) != 2*time.Hour || p2.UpdatedAt.Before(p1.UpdatedAt) {
		t.Fatalf("same visibility = %+v, created %v; want row %s extended to two hours from now", p2, created, p1.ID)
	}
	p3, created := f.upsert(r, 8080, types.PreviewOrg, time.Hour, nil)
	if !created || p3.ID == p1.ID {
		t.Fatalf("another visibility = %+v, created %v; want a new row", p3, created)
	}
	if revoked, by := f.revokedBy(p1.ID); !revoked || by != r.CreatedBy {
		t.Fatalf("the replaced row: revoked %v by %q, want revoked by %q", revoked, by, r.CreatedBy)
	}
	var live int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_previews WHERE run_id = $1 AND port = 8080 AND revoked_at IS NULL`, r.ID).Scan(&live); err != nil || live != 1 {
		t.Fatalf("unrevoked rows on the port = %d (%v), want 1", live, err)
	}
	_, err := f.pool.Exec(f.ctx, `INSERT INTO run_previews (id, run_id, port, visibility, created_by, expires_at)
		VALUES ($1, $2, 8080, 'private', 'someone', now() + interval '1 hour')`, uuid.New(), r.ID)
	if code, c := previewPGCode(err); code != "23505" || c != "run_previews_one_live" {
		t.Fatalf("a second live row on the port: %v, want the run_previews_one_live violation", err)
	}
	if _, _, err := f.pg.UpsertRunPreview(f.ctx, r.ID, 80, types.PreviewPrivate, time.Hour, r.CreatedBy, nil); err == nil {
		t.Fatal("a preview of port 80 was stored")
	}

	other, _ := f.upsert(r, 8081, types.PreviewPrivate, time.Hour, nil)
	list, err := f.pg.ListRunPreviews(f.ctx, r.ID)
	if err != nil || len(list) != 2 || list[0].ID != p3.ID || list[1].ID != other.ID {
		t.Fatalf("ListRunPreviews = %+v (%v), want rows %s and %s by port", list, err, p3.ID, other.ID)
	}
	if got, err := f.pg.LiveRunPreview(f.ctx, r.ID, 8080); err != nil || got.ID != p3.ID || got.Visibility != types.PreviewOrg {
		t.Fatalf("LiveRunPreview = %+v (%v), want row %s", got, err, p3.ID)
	}

	gone, err := f.pg.RevokeRunPreview(f.ctx, r.ID, 8080, "revoker")
	if err != nil || gone.ID != p3.ID || gone.RevokedAt == nil {
		t.Fatalf("RevokeRunPreview = %+v (%v), want row %s revoked", gone, err, p3.ID)
	}
	if revoked, by := f.revokedBy(p3.ID); !revoked || by != "revoker" {
		t.Fatalf("revoked row: revoked %v by %q", revoked, by)
	}
	if _, err := f.pg.LiveRunPreview(f.ctx, r.ID, 8080); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LiveRunPreview after the revoke: %v, want ErrNotFound", err)
	}
	if _, err := f.pg.RevokeRunPreview(f.ctx, r.ID, 8080, "revoker"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second revoke: %v, want ErrNotFound", err)
	}

	f.expire(other.ID)
	if list, err := f.pg.ListRunPreviews(f.ctx, r.ID); err != nil || len(list) != 0 {
		t.Fatalf("ListRunPreviews with only an expired row = %+v (%v), want none", list, err)
	}
	if _, err := f.pg.LiveRunPreview(f.ctx, r.ID, 8081); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LiveRunPreview of an expired row: %v, want ErrNotFound", err)
	}
	again, created := f.upsert(r, 8081, types.PreviewPrivate, time.Hour, nil)
	if !created || again.ID == other.ID {
		t.Fatalf("same visibility over an expired row = %+v, created %v; want a new row", again, created)
	}
	if revoked, _ := f.revokedBy(other.ID); !revoked {
		t.Fatal("the expired row was not revoked when its port was re-created")
	}

	if after := f.runUpdatedAt(r); !after.Equal(before) {
		t.Fatalf("agent_runs.updated_at moved from %s to %s", before, after)
	}
}

// TestStoreRunPreviewsLinkUnique pins the link hash: unique across every row, revoked ones included; kept with
// its row id on a same-visibility upsert, which stores no new hash; and found only on a live, unexpired row.
// So a new link always comes with a new row id.
func TestStoreRunPreviewsLinkUnique(t *testing.T) {
	f := newPreviewFixture(t)
	r, r2 := f.run(), f.run()

	pub, created := f.upsert(r, 8080, types.PreviewPublic, time.Hour, previewLinkHash("one"))
	if !created {
		t.Fatal("the first public row was not created")
	}
	if got, err := f.pg.RunPreviewByLink(f.ctx, previewLinkHash("one")); err != nil || got.ID != pub.ID {
		t.Fatalf("RunPreviewByLink = %+v (%v), want row %s", got, err, pub.ID)
	}

	kept, created := f.upsert(r, 8080, types.PreviewPublic, 2*time.Hour, previewLinkHash("two"))
	if created || kept.ID != pub.ID {
		t.Fatalf("public over a live public row = %+v, created %v; want row %s kept", kept, created, pub.ID)
	}
	var stored []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT link_hash FROM run_previews WHERE id = $1`, pub.ID).Scan(&stored); err != nil || string(stored) != string(previewLinkHash("one")) {
		t.Fatalf("the kept row's link hash changed (%v)", err)
	}
	if got, err := f.pg.RunPreviewByLink(f.ctx, previewLinkHash("one")); err != nil || got.ID != pub.ID {
		t.Fatalf("the first link after a same-visibility upsert = %+v (%v), want row %s", got, err, pub.ID)
	}
	if _, err := f.pg.RunPreviewByLink(f.ctx, previewLinkHash("two")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the hash a same-visibility upsert passed was stored: %v", err)
	}

	_, _, err := f.pg.UpsertRunPreview(f.ctx, r2.ID, 8080, types.PreviewPublic, time.Hour, r2.CreatedBy, previewLinkHash("one"))
	if code, c := previewPGCode(err); code != "23505" || c != "run_previews_link" {
		t.Fatalf("a second row with a stored link hash: %v, want the run_previews_link violation", err)
	}
	_, _, err = f.pg.UpsertRunPreview(f.ctx, r2.ID, 8081, types.PreviewPublic, time.Hour, r2.CreatedBy, nil)
	if code, _ := previewPGCode(err); code != "23514" {
		t.Fatalf("a public row with no link hash: %v, want a check violation", err)
	}
	_, _, err = f.pg.UpsertRunPreview(f.ctx, r2.ID, 8081, types.PreviewOrg, time.Hour, r2.CreatedBy, previewLinkHash("org"))
	if code, _ := previewPGCode(err); code != "23514" {
		t.Fatalf("an org row with a link hash: %v, want a check violation", err)
	}

	org, created := f.upsert(r, 8080, types.PreviewOrg, time.Hour, nil)
	if !created || org.ID == pub.ID {
		t.Fatalf("public to org = %+v, created %v; want a new row", org, created)
	}
	if _, err := f.pg.RunPreviewByLink(f.ctx, previewLinkHash("one")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a replaced public row's link still answers: %v", err)
	}
	pub2, created := f.upsert(r, 8080, types.PreviewPublic, time.Hour, previewLinkHash("three"))
	if !created || pub2.ID == pub.ID || pub2.ID == org.ID {
		t.Fatalf("public again = %+v, created %v; want a new row id", pub2, created)
	}
	if got, err := f.pg.RunPreviewByLink(f.ctx, previewLinkHash("three")); err != nil || got.ID != pub2.ID {
		t.Fatalf("the new link = %+v (%v), want row %s", got, err, pub2.ID)
	}
	_, _, err = f.pg.UpsertRunPreview(f.ctx, r2.ID, 8080, types.PreviewPublic, time.Hour, r2.CreatedBy, previewLinkHash("one"))
	if code, c := previewPGCode(err); code != "23505" || c != "run_previews_link" {
		t.Fatalf("a revoked row's link hash stored again: %v, want the run_previews_link violation", err)
	}

	if _, err := f.pg.RevokeRunPreview(f.ctx, r.ID, 8080, r.CreatedBy); err != nil {
		t.Fatalf("RevokeRunPreview: %v", err)
	}
	if _, err := f.pg.RunPreviewByLink(f.ctx, previewLinkHash("three")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked link answers: %v", err)
	}
	late, _ := f.upsert(r2, 8082, types.PreviewPublic, time.Hour, previewLinkHash("four"))
	f.expire(late.ID)
	if _, err := f.pg.RunPreviewByLink(f.ctx, previewLinkHash("four")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an expired link answers: %v", err)
	}
	if _, err := f.pg.RunPreviewByLink(f.ctx, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an empty link hash: %v, want ErrNotFound", err)
	}
}

// TestStoreRunPreviewsRevokeAbove pins the ceiling revoke: every run's rows wider than the ceiling are revoked
// and returned, again is a no-op, and an empty or unknown ceiling revokes them all.
func TestStoreRunPreviewsRevokeAbove(t *testing.T) {
	f := newPreviewFixture(t)
	r1, r2 := f.run(), f.run()
	before1, before2 := f.runUpdatedAt(r1), f.runUpdatedAt(r2)
	priv, _ := f.upsert(r1, 8080, types.PreviewPrivate, time.Hour, nil)
	org1, _ := f.upsert(r1, 8081, types.PreviewOrg, time.Hour, nil)
	pub1, _ := f.upsert(r1, 8082, types.PreviewPublic, time.Hour, previewLinkHash("r1"))
	org2, _ := f.upsert(r2, 8080, types.PreviewOrg, time.Hour, nil)
	pub2, _ := f.upsert(r2, 8081, types.PreviewPublic, time.Hour, previewLinkHash("r2"))

	above := func(ceiling types.PreviewVisibility, want ...uuid.UUID) {
		t.Helper()
		got, err := f.pg.RevokeRunPreviewsAbove(f.ctx, ceiling, "site-config")
		if err != nil {
			t.Fatalf("RevokeRunPreviewsAbove(%q): %v", ceiling, err)
		}
		if !slices.Equal(previewIDs(got), sortedPreviewIDs(want...)) {
			t.Fatalf("RevokeRunPreviewsAbove(%q) revoked %v, want %v", ceiling, previewIDs(got), sortedPreviewIDs(want...))
		}
		for _, p := range got {
			if revoked, by := f.revokedBy(p.ID); p.RevokedAt == nil || !revoked || by != "site-config" {
				t.Fatalf("row %s: revoked %v by %q", p.ID, revoked, by)
			}
		}
	}
	above(types.PreviewPublic)
	above(types.PreviewOrg, pub1.ID, pub2.ID)
	above(types.PreviewOrg)
	if _, err := f.pg.RunPreviewByLink(f.ctx, previewLinkHash("r1")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a link above the ceiling still answers: %v", err)
	}
	above(types.PreviewPrivate, org1.ID, org2.ID)
	if list, err := f.pg.ListRunPreviews(f.ctx, r1.ID); err != nil || len(list) != 1 || list[0].ID != priv.ID {
		t.Fatalf("live rows under a private ceiling = %+v (%v), want only %s", list, err, priv.ID)
	}
	above("", priv.ID) // keep stays a non-nil slice: pgx sends a nil one as NULL, and NOT (visibility = ANY(NULL)) revokes nothing
	again, _ := f.upsert(r2, 8082, types.PreviewPrivate, time.Hour, nil)
	above("team", again.ID)
	for _, r := range []types.AgentRun{r1, r2} {
		if list, err := f.pg.ListRunPreviews(f.ctx, r.ID); err != nil || len(list) != 0 {
			t.Fatalf("live rows of run %s after revoking all = %+v (%v)", r.ID, list, err)
		}
	}
	if !f.runUpdatedAt(r1).Equal(before1) || !f.runUpdatedAt(r2).Equal(before2) {
		t.Fatal("a preview write moved agent_runs.updated_at")
	}
}

// TestStoreRunPreviewsFirstRowClashRetried pins UpsertRunPreview's one retry: a write that inserts a port's first
// row while another write's first row on that port is still uncommitted waits on run_previews_one_live, gets its
// violation once the other commits, and runs once more, which sees the committed row. The same visibility keeps
// that row, so the caller gets it back, not the violation.
func TestStoreRunPreviewsFirstRowClashRetried(t *testing.T) {
	f := newPreviewFixture(t)
	r := f.run()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	first := uuid.New()
	if _, err := tx.Exec(f.ctx, `INSERT INTO run_previews (id, run_id, port, visibility, created_by, expires_at)
		VALUES ($1, $2, 8080, 'private', $3, now() + interval '1 hour')`, first, r.ID, r.CreatedBy); err != nil {
		t.Fatalf("the other write's first row: %v", err)
	}
	type result struct {
		p       types.RunPreview
		created bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		p, created, err := f.pg.UpsertRunPreview(f.ctx, r.ID, 8080, types.PreviewPrivate, 2*time.Hour, r.CreatedBy, nil)
		done <- result{p, created, err}
	}()
	// The upsert's insert waits on the uncommitted row's index entry. Commit only once it waits, so its first try is
	// the one that clashes.
	deadline := time.Now().Add(10 * time.Second)
	for waiting := 0; waiting == 0; {
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatalf("read the waiting writes: %v", err)
		}
		select {
		case res := <-done:
			t.Fatalf("the upsert returned before it waited on the other write's row: %+v", res)
		default:
		}
		if waiting == 0 && time.Now().After(deadline) {
			t.Fatal("the upsert never waited on the other write's row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("commit the other write: %v", err)
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the upsert never returned after the other write committed")
	}
	if res.err != nil {
		t.Fatalf("UpsertRunPreview after a clash on the port's first row: %v, want the retry to keep the committed row", res.err)
	}
	if res.created || res.p.ID != first || res.p.Visibility != types.PreviewPrivate || !res.p.ExpiresAt.After(time.Now().Add(90*time.Minute)) {
		t.Fatalf("UpsertRunPreview after the clash = %+v, created %v; want row %s kept and extended to two hours", res.p, res.created, first)
	}
	var live int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_previews WHERE run_id = $1 AND port = 8080 AND revoked_at IS NULL`, r.ID).Scan(&live); err != nil || live != 1 {
		t.Fatalf("unrevoked rows on the port = %d (%v), want 1", live, err)
	}
}

// TestStoreRunPreviewsColumnChecks pins the table's own checks at their edges: a port from 1024 to 65535, one of
// the three visibilities as written, and a created_by that is not empty. Each refusal is a check violation (23514).
func TestStoreRunPreviewsColumnChecks(t *testing.T) {
	f := newPreviewFixture(t)
	r := f.run()
	for _, c := range []struct {
		name     string
		port     int
		vis, by  string
		accepted bool
	}{
		{"port 0", 0, "private", "owner", false},
		{"port 1023", 1023, "private", "owner", false},
		{"port 1024", 1024, "private", "owner", true},
		{"port 65535", 65535, "private", "owner", true},
		{"port 65536", 65536, "private", "owner", false},
		{"visibility private", 2001, "private", "owner", true},
		{"visibility org", 2002, "org", "owner", true},
		{"visibility empty", 2003, "", "owner", false},
		{"visibility Private", 2004, "Private", "owner", false},
		{"visibility team", 2005, "team", "owner", false},
		{"created_by empty", 2006, "private", "", false},
		{"created_by one character", 2007, "private", "a", true},
	} {
		_, err := f.pool.Exec(f.ctx, `INSERT INTO run_previews (id, run_id, port, visibility, created_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour')`, uuid.New(), r.ID, c.port, c.vis, c.by)
		code, _ := previewPGCode(err)
		switch {
		case c.accepted && err != nil:
			t.Errorf("%s: %v, want the row stored", c.name, err)
		case !c.accepted && code != "23514":
			t.Errorf("%s: %v, want a check violation (23514)", c.name, err)
		}
	}
}
