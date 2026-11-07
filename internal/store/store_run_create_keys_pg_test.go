// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for run_create_keys: a POST /runs Idempotency-Key, its claim, and its binding to
// the run inside each run-insert transaction. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const runCreateKeyLease = 5 * time.Minute

// newRunCreateKey is a key with a principal of its own, so a case is isolated in the shared database.
func newRunCreateKey(request string) store.RunCreateKey {
	return store.RunCreateKey{
		RunCreateKeyID: store.RunCreateKeyID{Principal: "keys-" + uuid.NewString()[:8] + "@example.com", KeySHA256: uuid.NewString()},
		RequestSHA256:  request,
	}
}

func claimKey(t *testing.T, pg store.PG, k store.RunCreateKey) store.RunCreateKeyClaim {
	t.Helper()
	c, err := pg.ClaimRunCreateKey(context.Background(), k, runCreateKeyLease)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return c
}

// keyRow reads the key's row: whether it exists and the run it names (uuid.Nil for none).
func keyRow(t *testing.T, pool *pgxpool.Pool, id store.RunCreateKeyID) (bool, uuid.UUID) {
	t.Helper()
	var n int
	var run *uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT count(*), max(run_id::text)::uuid FROM run_create_keys
		WHERE principal = $1 AND via = $2 AND key_sha256 = $3`, id.Principal, id.Via, id.KeySHA256).Scan(&n, &run); err != nil {
		t.Fatal(err)
	}
	if run == nil {
		return n > 0, uuid.Nil
	}
	return n > 0, *run
}

func expireLease(t *testing.T, pool *pgxpool.Pool, id store.RunCreateKeyID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE run_create_keys SET claim_until = now() - interval '1 second'
		WHERE principal = $1 AND via = $2 AND key_sha256 = $3`, id.Principal, id.Via, id.KeySHA256); err != nil {
		t.Fatal(err)
	}
}

func runRows(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (n int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_runs WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// createKeyed inserts r through CreateRun with the key's claim on the context.
func createKeyed(pg store.PG, id store.RunCreateKeyID, token uuid.UUID, r types.AgentRun) (types.AgentRun, error) {
	return pg.CreateRun(store.WithRunCreateKey(context.Background(), id, token), r)
}

// TestRunCreateKeyClaimStates walks one key through every claim answer: new, in flight, mismatch,
// takeover only after the lease and only for the same request, done before and after the response is
// saved, and reuse once the key expired.
func TestRunCreateKeyClaimStates(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	k := newRunCreateKey("request-a")
	other := k
	other.RequestSHA256 = "request-b"

	first := claimKey(t, pg, k)
	if first.State != store.RunCreateKeyClaimed || first.Token == uuid.Nil {
		t.Fatalf("a new key = %+v, want Claimed with a token", first)
	}
	if c := claimKey(t, pg, k); c.State != store.RunCreateKeyInFlight {
		t.Fatalf("the same key while its lease is live = %+v, want InFlight", c)
	}
	if c := claimKey(t, pg, other); c.State != store.RunCreateKeyMismatch {
		t.Fatalf("the same key with another request = %+v, want Mismatch", c)
	}

	expireLease(t, pool, k.RunCreateKeyID)
	if c := claimKey(t, pg, other); c.State != store.RunCreateKeyMismatch {
		t.Fatalf("another request after the lease = %+v, want Mismatch (no takeover)", c)
	}
	taken := claimKey(t, pg, k)
	if taken.State != store.RunCreateKeyClaimed || taken.Token == uuid.Nil || taken.Token == first.Token {
		t.Fatalf("the same request after the lease = %+v, want Claimed with a new token (first %s)", taken, first.Token)
	}

	r := newRun(types.RunPending)
	r.CreatedBy = k.Principal
	if _, err := createKeyed(pg, k.RunCreateKeyID, taken.Token, r); err != nil {
		t.Fatalf("create under the current claim: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })

	done := claimKey(t, pg, k)
	if done.State != store.RunCreateKeyDone || done.RunID != r.ID || done.Response != nil || done.CreatedAt.IsZero() {
		t.Fatalf("a bound key with no saved answer = %+v, want Done naming %s with no response", done, r.ID)
	}
	expireLease(t, pool, k.RunCreateKeyID)
	if c := claimKey(t, pg, k); c.State != store.RunCreateKeyDone || c.RunID != r.ID {
		t.Fatalf("a bound key past its lease = %+v, want Done (a bound key is never taken over)", c)
	}
	if c := claimKey(t, pg, other); c.State != store.RunCreateKeyMismatch {
		t.Fatalf("a bound key with another request = %+v, want Mismatch", c)
	}

	body := []byte("{\"id\":\"" + r.ID.String() + "\"}\n")
	if bound, err := pg.SaveRunCreateResponse(context.Background(), k.RunCreateKeyID, taken.Token, 201, "/api/v1/runs/"+r.ID.String(), body); err != nil || !bound {
		t.Fatalf("save = %v, %v; want bound", bound, err)
	}
	saved := claimKey(t, pg, k)
	if saved.State != store.RunCreateKeyDone || saved.Response == nil || saved.Response.Status != 201 ||
		saved.Response.Location != "/api/v1/runs/"+r.ID.String() || !bytes.Equal(saved.Response.Body, body) {
		t.Fatalf("a bound key with a saved answer = %+v, want Done with the same status, location and bytes", saved)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE run_create_keys SET expires_at = now() - interval '1 second'
		WHERE principal = $1 AND via = $2 AND key_sha256 = $3`, k.Principal, k.Via, k.KeySHA256); err != nil {
		t.Fatal(err)
	}
	reused := claimKey(t, pg, other)
	if reused.State != store.RunCreateKeyClaimed || reused.Token == uuid.Nil {
		t.Fatalf("an expired key with a new request = %+v, want Claimed afresh", reused)
	}
	if ok, run := keyRow(t, pool, k.RunCreateKeyID); !ok || run != uuid.Nil {
		t.Errorf("after reuse the key row exists %v naming %s, want a fresh row naming no run", ok, run)
	}
}

// TestRunCreateKeyScopeSeparatesCallers: the same key hash under another principal or another
// delegate is a different key.
func TestRunCreateKeyScopeSeparatesCallers(t *testing.T) {
	pg := store.NewPG(runsPGPool(t))
	k := newRunCreateKey("request-a")
	viaPortal := k
	viaPortal.Via = uuid.NewString()
	service := k
	service.Principal = "service:" + uuid.NewString()[:8]
	for _, key := range []store.RunCreateKey{k, viaPortal, service} {
		if c := claimKey(t, pg, key); c.State != store.RunCreateKeyClaimed {
			t.Errorf("principal %q via %q = %+v, want its own Claimed key", key.Principal, key.Via, c)
		}
	}
}

// TestRunCreateKeyClaimRace: eight claims of one new key at once give exactly one Claimed; the rest see
// it in flight.
func TestRunCreateKeyClaimRace(t *testing.T) {
	pg := store.NewPG(runsPGPool(t))
	k := newRunCreateKey("request-a")
	var wg sync.WaitGroup
	states := make([]store.RunCreateKeyState, 8)
	errs := make([]error, 8)
	for i := range states {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := pg.ClaimRunCreateKey(context.Background(), k, runCreateKeyLease)
			states[i], errs[i] = c.State, err
		}(i)
	}
	wg.Wait()
	claimed := 0
	for i, s := range states {
		if errs[i] != nil {
			t.Fatalf("claim %d: %v", i, errs[i])
		}
		switch s {
		case store.RunCreateKeyClaimed:
			claimed++
		case store.RunCreateKeyInFlight:
		default:
			t.Errorf("claim %d = state %d, want Claimed or InFlight", i, s)
		}
	}
	if claimed != 1 {
		t.Errorf("%d claims won, want exactly 1", claimed)
	}
}

// TestRunCreateKeyBindsInInsertTx: each of the three run-insert writers binds the key in its own
// transaction. A stale claim (its lease taken over) gives ErrRunCreateKeySuperseded and no agent_runs
// row; the current claim binds; and a bound key never binds a second run.
func TestRunCreateKeyBindsInInsertTx(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	writers := []struct {
		name   string
		run    func(owner string) types.AgentRun
		create func(ctx context.Context, r types.AgentRun) (types.AgentRun, error)
	}{
		{"CreateRun", func(owner string) types.AgentRun {
			r := newRun(types.RunPending)
			r.CreatedBy = owner
			return r
		}, pg.CreateRun},
		{"CreateRunUnderCap", func(owner string) types.AgentRun {
			r := newRun(types.RunPending)
			r.CreatedBy = owner
			return r
		}, func(ctx context.Context, r types.AgentRun) (types.AgentRun, error) {
			return pg.CreateRunUnderCap(ctx, r, 1<<20)
		}},
		{"CreateQueuedRun", func(owner string) types.AgentRun {
			return poolRun(p, types.RunQueued, owner)
		}, func(ctx context.Context, r types.AgentRun) (types.AgentRun, error) {
			return pg.CreateQueuedRun(ctx, r, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
		}},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			k := newRunCreateKey("request-a")
			stale := claimKey(t, pg, k)
			expireLease(t, pool, k.RunCreateKeyID)
			current := claimKey(t, pg, k)
			if current.State != store.RunCreateKeyClaimed {
				t.Fatalf("takeover = %+v, want Claimed", current)
			}

			lost := w.run(k.Principal)
			if _, err := w.create(store.WithRunCreateKey(ctx, k.RunCreateKeyID, stale.Token), lost); !errors.Is(err, store.ErrRunCreateKeySuperseded) {
				t.Fatalf("insert under the stale claim: err %v, want ErrRunCreateKeySuperseded", err)
			}
			if n := runRows(t, pool, lost.ID); n != 0 {
				t.Fatalf("the stale attempt left %d agent_runs rows, want 0 (the insert rolls back)", n)
			}

			won := w.run(k.Principal)
			if _, err := w.create(store.WithRunCreateKey(ctx, k.RunCreateKeyID, current.Token), won); err != nil {
				t.Fatalf("insert under the current claim: %v", err)
			}
			if ok, run := keyRow(t, pool, k.RunCreateKeyID); !ok || run != won.ID {
				t.Fatalf("the key names %s (row %v), want the inserted run %s", run, ok, won.ID)
			}

			again := w.run(k.Principal)
			if _, err := w.create(store.WithRunCreateKey(ctx, k.RunCreateKeyID, current.Token), again); !errors.Is(err, store.ErrRunCreateKeySuperseded) {
				t.Fatalf("a second insert under a bound key: err %v, want ErrRunCreateKeySuperseded", err)
			}
			if n := runRows(t, pool, again.ID); n != 0 {
				t.Errorf("a bound key let a second run in (%d rows)", n)
			}
		})
	}
}

// TestRunCreateKeyReleaseKeepsBoundRow: a release deletes only an unbound claim under its own token; a
// bound key survives it and still answers Done.
func TestRunCreateKeyReleaseKeepsBoundRow(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	ctx := context.Background()

	unbound := newRunCreateKey("request-a")
	c := claimKey(t, pg, unbound)
	if err := pg.ReleaseRunCreateKey(ctx, unbound.RunCreateKeyID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := keyRow(t, pool, unbound.RunCreateKeyID); !ok {
		t.Fatal("a release under another token deleted the claim")
	}
	if err := pg.ReleaseRunCreateKey(ctx, unbound.RunCreateKeyID, c.Token); err != nil {
		t.Fatal(err)
	}
	if ok, _ := keyRow(t, pool, unbound.RunCreateKeyID); ok {
		t.Fatal("the release left the unbound claim")
	}
	if again := claimKey(t, pg, unbound); again.State != store.RunCreateKeyClaimed {
		t.Fatalf("a released key = %+v, want Claimed afresh", again)
	}

	bound := newRunCreateKey("request-a")
	bc := claimKey(t, pg, bound)
	r := newRun(types.RunPending)
	r.CreatedBy = bound.Principal
	if _, err := createKeyed(pg, bound.RunCreateKeyID, bc.Token, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
	if err := pg.ReleaseRunCreateKey(ctx, bound.RunCreateKeyID, bc.Token); err != nil {
		t.Fatal(err)
	}
	if ok, run := keyRow(t, pool, bound.RunCreateKeyID); !ok || run != r.ID {
		t.Fatalf("after a release the bound key row exists %v naming %s, want it kept naming %s", ok, run, r.ID)
	}
	if done := claimKey(t, pg, bound); done.State != store.RunCreateKeyDone || done.RunID != r.ID {
		t.Fatalf("a bound key after a release = %+v, want Done", done)
	}
}

// TestRunCreateKeySavesAnyStatusOnceBound: an answer is stored only for a key bound to a run under the
// caller's token, whatever its status, so a refusal after the insert replays as itself. An unbound claim
// stores nothing, and a body past the cap leaves the response unset.
func TestRunCreateKeySavesAnyStatusOnceBound(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	ctx := context.Background()

	unbound := newRunCreateKey("request-a")
	uc := claimKey(t, pg, unbound)
	if bound, err := pg.SaveRunCreateResponse(ctx, unbound.RunCreateKeyID, uc.Token, 422, "", []byte("refused")); err != nil || bound {
		t.Fatalf("save on an unbound claim = %v, %v; want not bound", bound, err)
	}
	var status *int
	if err := pool.QueryRow(ctx, `SELECT response_status FROM run_create_keys WHERE principal = $1 AND key_sha256 = $2`,
		unbound.Principal, unbound.KeySHA256).Scan(&status); err != nil || status != nil {
		t.Fatalf("an unbound claim stored status %v (err %v), want none", status, err)
	}

	k := newRunCreateKey("request-a")
	c := claimKey(t, pg, k)
	r := newRun(types.RunPending)
	r.CreatedBy = k.Principal
	if _, err := createKeyed(pg, k.RunCreateKeyID, c.Token, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
	if bound, err := pg.SaveRunCreateResponse(ctx, k.RunCreateKeyID, uuid.New(), 201, "", []byte("x")); err != nil || bound {
		t.Fatalf("save under another token = %v, %v; want not bound", bound, err)
	}
	refusal := []byte(`{"error":"refused","reason":"member_drive_unsupported"}`)
	if bound, err := pg.SaveRunCreateResponse(ctx, k.RunCreateKeyID, c.Token, 422, "", refusal); err != nil || !bound {
		t.Fatalf("save of a refusal after the insert = %v, %v; want bound", bound, err)
	}
	got := claimKey(t, pg, k)
	if got.State != store.RunCreateKeyDone || got.Response == nil || got.Response.Status != 422 ||
		got.Response.Location != "" || !bytes.Equal(got.Response.Body, refusal) {
		t.Fatalf("a bound key with a saved refusal = %+v, want Done replaying 422 byte-for-byte", got)
	}

	big := newRunCreateKey("request-a")
	bc := claimKey(t, pg, big)
	br := newRun(types.RunPending)
	br.CreatedBy = big.Principal
	if _, err := createKeyed(pg, big.RunCreateKeyID, bc.Token, br); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, br.ID) })
	if bound, err := pg.SaveRunCreateResponse(ctx, big.RunCreateKeyID, bc.Token, 201, "/x", make([]byte, store.MaxRunCreateResponseBytes+1)); err != nil || !bound {
		t.Fatalf("save of an oversized body = %v, %v; want bound", bound, err)
	}
	if got := claimKey(t, pg, big); got.State != store.RunCreateKeyDone || got.RunID != br.ID || got.Response != nil {
		t.Fatalf("a bound key whose body passed the cap = %+v, want Done with no response (reconstructed)", got)
	}
}

// TestRunCreateKeyFKCascade: deleting the run deletes the key that names it.
func TestRunCreateKeyFKCascade(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	k := newRunCreateKey("request-a")
	c := claimKey(t, pg, k)
	r := newRun(types.RunPending)
	r.CreatedBy = k.Principal
	if _, err := createKeyed(pg, k.RunCreateKeyID, c.Token, r); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := keyRow(t, pool, k.RunCreateKeyID); ok {
		t.Error("the key outlived the run it names")
	}
}

// TestDeleteRunCreateKeysOf: erasure deletes every key of the named principals, bound or not, and
// nobody else's.
func TestDeleteRunCreateKeysOf(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	gone, viaPortal, kept := newRunCreateKey("a"), newRunCreateKey("a"), newRunCreateKey("a")
	viaPortal.Principal, viaPortal.Via = gone.Principal, uuid.NewString()
	for _, k := range []store.RunCreateKey{gone, viaPortal, kept} {
		claimKey(t, pg, k)
	}
	n, err := pg.DeleteRunCreateKeysOf(context.Background(), []string{gone.Principal})
	if err != nil || n != 2 {
		t.Fatalf("erase = %d, %v; want the principal's 2 keys", n, err)
	}
	if ok, _ := keyRow(t, pool, kept.RunCreateKeyID); !ok {
		t.Error("erasure deleted another principal's key")
	}
	if n, err := pg.DeleteRunCreateKeysOf(context.Background(), nil); err != nil || n != 0 {
		t.Errorf("erase of nobody = %d, %v; want 0", n, err)
	}
}

// TestRunCreateKeyDoneReportsLease: a bound key says whether the lease of the claim that bound it (or
// took it over) is still live, so a caller can tell a first request that may still be answering from one
// that will never save its answer.
func TestRunCreateKeyDoneReportsLease(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	k := newRunCreateKey("request-a")
	c := claimKey(t, pg, k)
	r := newRun(types.RunPending)
	r.CreatedBy = k.Principal
	if _, err := createKeyed(pg, k.RunCreateKeyID, c.Token, r); err != nil {
		t.Fatalf("create under the claim: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })

	live := claimKey(t, pg, k)
	if live.State != store.RunCreateKeyDone || !live.LeaseLive || !live.ClaimUntil.After(live.CreatedAt) {
		t.Fatalf("a bound key inside its lease = %+v, want Done with a live lease ending after its first use", live)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE run_create_keys SET created_at = now() - interval '1 hour'
		WHERE principal = $1 AND via = $2 AND key_sha256 = $3`, k.Principal, k.Via, k.KeySHA256); err != nil {
		t.Fatal(err)
	}
	if aged := claimKey(t, pg, k); !aged.LeaseLive {
		t.Fatalf("an old key whose claim is still leased = %+v, want a live lease (the lease, not the age, decides)", aged)
	}
	expireLease(t, pool, k.RunCreateKeyID)
	if gone := claimKey(t, pg, k); gone.State != store.RunCreateKeyDone || gone.LeaseLive || gone.ClaimUntil.IsZero() {
		t.Fatalf("a bound key past its lease = %+v, want Done with the lease over and its end reported", gone)
	}
}

// TestClearRunCreateResponsesOf: erasing a person's run tasks clears the answers kept for keys naming
// their runs and keeps each key bound to its run; another person's answer stays; a save that arrives
// after the clear keeps nothing.
func TestClearRunCreateResponsesOf(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	bind := func(k store.RunCreateKey, createdBy string) (uuid.UUID, uuid.UUID) {
		t.Helper()
		c := claimKey(t, pg, k)
		r := newRun(types.RunPending)
		r.CreatedBy = createdBy
		if _, err := createKeyed(pg, k.RunCreateKeyID, c.Token, r); err != nil {
			t.Fatalf("create under the claim: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
		return c.Token, r.ID
	}
	alice, bob := "erase-"+uuid.NewString()[:8], "keep-"+uuid.NewString()[:8]
	saved := newRunCreateKey("request-a")
	late := newRunCreateKey("request-b")
	other := newRunCreateKey("request-c")
	savedToken, savedRun := bind(saved, alice)
	lateToken, lateRun := bind(late, alice)
	otherToken, _ := bind(other, bob)
	for _, s := range []struct {
		k     store.RunCreateKey
		token uuid.UUID
	}{{saved, savedToken}, {other, otherToken}} {
		if ok, err := pg.SaveRunCreateResponse(ctx, s.k.RunCreateKeyID, s.token, 201, "/x", []byte(`{"task":"secret"}`)); err != nil || !ok {
			t.Fatalf("save = %v %v", ok, err)
		}
	}

	n, err := pg.ClearRunCreateResponsesOf(ctx, alice)
	if err != nil || n != 2 {
		t.Fatalf("clear = %d %v, want alice's 2 keys", n, err)
	}
	if ok, err := pg.SaveRunCreateResponse(ctx, late.RunCreateKeyID, lateToken, 201, "/x", []byte(`{"task":"secret"}`)); err != nil || ok {
		t.Fatalf("a save after the clear = %v %v, want nothing kept", ok, err)
	}
	for _, c := range []struct {
		k   store.RunCreateKey
		run uuid.UUID
	}{{saved, savedRun}, {late, lateRun}} {
		got := claimKey(t, pg, c.k)
		if got.State != store.RunCreateKeyDone || got.RunID != c.run || got.Response != nil || !got.ResponseErased {
			t.Errorf("alice's key after the clear = %+v, want Done naming %s with no answer, marked erased", got, c.run)
		}
	}
	if got := claimKey(t, pg, other); got.Response == nil || got.ResponseErased || !bytes.Equal(got.Response.Body, []byte(`{"task":"secret"}`)) {
		t.Errorf("bob's key after alice's clear = %+v, want his answer kept", got)
	}
}
