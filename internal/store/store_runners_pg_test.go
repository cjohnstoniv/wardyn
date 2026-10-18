// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for registered runners and the credential delivery policy (migration
// 0139); pending actions are in store_runner_actions_pg_test.go. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newRunner is a runner of owner that may hold as many runs at once as any runner may, and as much as
// any runner may declare, so a fixture's runs are never refused for its limits; a test of limits sets
// its own.
func newRunner(owner string) types.Runner {
	id := uuid.New()
	return types.Runner{ID: id, Owner: owner, Name: "laptop", PublicKey: make([]byte, 32), KeyFingerprint: "fp-" + id.String(), Version: "0.9.0",
		Limits: roomyLimits(types.RunnerMaxRunsCeiling)}
}

// roomyLimits is a limit set of runs at once with the largest totals and per-run amounts a runner may
// declare.
func roomyLimits(runs int) *types.RunnerLimits {
	most := types.RunnerAmounts{CPUMillis: types.RunnerPoolCPUMillisMax, MemoryMiB: types.RunnerPoolMemoryMiBMax, DiskMiB: types.RunnerPoolDiskMiBMax}
	each := most
	each.DiskMiB = most.DiskMiB / int64(runs) / types.RunnerDiskStepMiB * types.RunnerDiskStepMiB
	return &types.RunnerLimits{RunsAtOnce: runs, Total: most, PerRun: each}
}

func TestPG_Runners_CreateStartsUnclaimedAndFingerprintIsUnique(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r := newRunner("alice@example.com")
	r.State = types.RunnerClaimed // ignored: every registration starts unclaimed
	got, err := st.CreateRunner(ctx, r)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	if got.State != types.RunnerUnclaimed || got.ClaimedAt != nil || got.PostureSource != "" || got.PostureReportedAt != nil {
		t.Fatalf("a new runner is not plainly unclaimed: %+v", got)
	}
	if got.Owner != r.Owner || got.KeyFingerprint != r.KeyFingerprint || len(got.PublicKey) != 32 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	dup := newRunner("bob@example.com")
	dup.KeyFingerprint = r.KeyFingerprint
	if _, err := st.CreateRunner(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a reused key fingerprint = %v, want ErrConflict", err)
	}
	bad := newRunner("x")
	bad.PublicKey = []byte("short")
	if _, err := st.CreateRunner(ctx, bad); err == nil {
		t.Fatal("a public key that is not 32 bytes was stored")
	}
}

func TestPG_Runners_ClaimOnlyByOwnerWithFingerprint(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	now := time.Now().UTC()

	if _, err := st.ClaimRunner(ctx, r.ID, "admin@example.com", r.KeyFingerprint, now); !errors.Is(err, store.ErrRunnerClaimMismatch) {
		t.Fatalf("a claim by someone but the owner = %v, want ErrRunnerClaimMismatch", err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, "another-fingerprint", now); !errors.Is(err, store.ErrRunnerClaimMismatch) {
		t.Fatalf("a claim with another fingerprint = %v, want ErrRunnerClaimMismatch", err)
	}
	if _, err := st.ClaimRunner(ctx, uuid.New(), r.Owner, r.KeyFingerprint, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a claim of an unknown runner = %v", err)
	}
	got, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now)
	if err != nil || got.State != types.RunnerClaimed || got.ClaimedAt == nil {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second claim = %v, want ErrConflict", err)
	}
}

func TestPG_Runners_ConcurrentClaimHasOneWinner(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, time.Now().UTC()); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d claims won, want exactly one", wins.Load())
	}
}

func TestPG_Runners_PostureOnlyForClaimedAndReportsChange(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	r, err := st.CreateRunner(ctx, newRunner("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	now := time.Now().UTC()
	p := types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "darwin", OSVersion: "15.1"}

	if _, err := st.RecordRunnerPosture(ctx, r.ID, p, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("posture from an unclaimed runner = %v, want ErrNotFound", err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.RecordRunnerPosture(ctx, r.ID, p, now); err != nil || !changed {
		t.Fatalf("first report: changed=%v err=%v", changed, err)
	}
	if changed, err := st.RecordRunnerPosture(ctx, r.ID, p, now); err != nil || changed {
		t.Fatalf("an identical report: changed=%v err=%v, want unchanged", changed, err)
	}
	p.DiskEncrypted = false
	if changed, err := st.RecordRunnerPosture(ctx, r.ID, p, now); err != nil || !changed {
		t.Fatalf("a different report: changed=%v err=%v", changed, err)
	}
	got, err := st.GetRunner(ctx, r.ID)
	if err != nil || got.Posture != p || got.PostureSource != types.RunnerPostureSourceRunnerAsserted || got.PostureReportedAt == nil {
		t.Fatalf("stored posture = %+v, %v", got, err)
	}
}

// claimed registers and claims a runner for the action queue, which takes claimed runners only.
func claimed(t *testing.T, st store.PG, ctx context.Context, owner string) types.Runner {
	t.Helper()
	r, err := st.CreateRunner(ctx, newRunner(owner))
	if err != nil {
		t.Fatal(err)
	}
	if r, err = st.ClaimRunner(ctx, r.ID, owner, r.KeyFingerprint, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPG_Runners_ListAndExpireUnclaimed(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	owner := "owner-" + uuid.NewString()
	a, _ := st.CreateRunner(ctx, newRunner(owner))
	b, _ := st.CreateRunner(ctx, newRunner(owner))
	other, _ := st.CreateRunner(ctx, newRunner("someone-else"))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id = ANY($1)`, []uuid.UUID{a.ID, b.ID, other.ID})
	})
	if _, err := st.ClaimRunner(ctx, b.ID, owner, b.KeyFingerprint, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	mine, err := st.ListRunnersByOwner(ctx, owner)
	if err != nil || len(mine) != 2 {
		t.Fatalf("by owner = %d runners, %v", len(mine), err)
	}
	if all, err := st.ListRunners(ctx); err != nil || len(all) < 3 {
		t.Fatalf("list = %d, %v", len(all), err)
	}
	// Only the unclaimed runner older than the cutoff goes; the claimed one stays.
	n, err := st.ExpireUnclaimedRunners(ctx, time.Now().Add(time.Hour))
	if err != nil || n < 2 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	if _, err := st.GetRunner(ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the unclaimed runner survived: %v", err)
	}
	if _, err := st.GetRunner(ctx, b.ID); err != nil {
		t.Fatalf("the claimed runner was expired: %v", err)
	}
}

func TestPG_CredentialDeliveryPolicy_DefaultsEmptyAndUpserts(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	_, _ = pool.Exec(ctx, `DELETE FROM credential_delivery_policy`)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM credential_delivery_policy`) })

	d, err := st.GetCredentialDeliveryPolicy(ctx)
	if err != nil || string(d.Policy) != `{"classes":{}}` {
		t.Fatalf("default policy = %s, %v", d.Policy, err)
	}
	doc := json.RawMessage(`{"classes":{"api_key":{"mode":"via_org"}}}`)
	now := time.Now().UTC()
	if _, err := st.PutCredentialDeliveryPolicy(ctx, doc, "sec@example.com", now); err != nil {
		t.Fatal(err)
	}
	next := json.RawMessage(`{"classes":{"env_secret":{"mode":"runner_resident","require_posture":{"mdm_managed":true}}}}`)
	if _, err := st.PutCredentialDeliveryPolicy(ctx, next, "sec2@example.com", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCredentialDeliveryPolicy(ctx)
	if err != nil || got.UpdatedBy != "sec2@example.com" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	var a, b any
	_ = json.Unmarshal(got.Policy, &a)
	_ = json.Unmarshal(next, &b)
	if ja, jb := mustJSON(a), mustJSON(b); ja != jb {
		t.Fatalf("policy = %s, want %s", ja, jb)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credential_delivery_policy`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d policy rows, want the singleton", n)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO credential_delivery_policy (singleton) VALUES (false)`); err == nil {
		t.Fatal("a second policy row was inserted")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestPG_CredentialGrants_DeliveryRoundTripAndCheck(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	for _, d := range []types.GrantDelivery{types.GrantDeliveryNone, types.GrantDeliveryOwn, types.GrantDeliveryViaOrg, types.GrantDeliveryResident} {
		g, err := st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, CreatedAt: time.Now().UTC(), Delivery: d,
			Spec: types.GrantSpec{Kind: types.GrantEnvSecret}})
		if err != nil || g.Delivery != d {
			t.Fatalf("create with delivery %q = %q, %v", d, g.Delivery, err)
		}
	}
	grants, err := st.ListGrantsByRun(ctx, run.ID)
	if err != nil || len(grants) != 4 {
		t.Fatalf("list = %d, %v", len(grants), err)
	}
	seen := map[types.GrantDelivery]bool{}
	for _, g := range grants {
		seen[g.Delivery] = true
	}
	if len(seen) != 4 {
		t.Fatalf("deliveries read back = %v", seen)
	}
	if _, err := st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, CreatedAt: time.Now().UTC(), Delivery: "everywhere",
		Spec: types.GrantSpec{Kind: types.GrantEnvSecret}}); err == nil {
		t.Fatal("a delivery outside the CHECK was stored")
	}
	// A grant written the old way (no delivery column) reads back as ''.
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO credential_grants (id, run_id, spec) VALUES ($1,$2,'{"kind":"env_secret"}')`, id, run.ID); err != nil {
		t.Fatal(err)
	}
	var d string
	if err := pool.QueryRow(ctx, `SELECT delivery FROM credential_grants WHERE id=$1`, id).Scan(&d); err != nil || d != "" {
		t.Fatalf("legacy grant delivery = %q, %v", d, err)
	}
}

// The owner's own list leaves out a lapsed unclaimed runner and a revoked one unless someone else revoked it
// within the window, in the query itself: the same rows the console's list showed when the filter ran after
// the read.
func TestPG_Runners_ListVisibleByOwnerFiltersInTheQuery(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	owner := "owner-" + uuid.NewString()
	now := time.Now().UTC()
	mk := func(owner string) types.Runner {
		r, err := st.CreateRunner(ctx, newRunner(owner))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id = $1`, r.ID) })
		return r
	}
	fresh, lapsed, byOwner, bySomeone, stale, oldClaimed, other := mk(owner), mk(owner), mk(owner), mk(owner), mk(owner), mk(owner), mk("someone-else")
	if _, err := pool.Exec(ctx, `UPDATE runners SET created_at = $2 WHERE id = $1`, lapsed.ID, now.Add(-types.RunnerUnclaimedTTL-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Only an unclaimed runner lapses: a claimed one older than the wait stays on the list.
	if _, err := st.ClaimRunner(ctx, oldClaimed.ID, owner, oldClaimed.KeyFingerprint, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET created_at = $2 WHERE id = $1`, oldClaimed.ID, now.Add(-types.RunnerUnclaimedTTL-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		r  types.Runner
		by string
		at time.Time
	}{{byOwner, owner, now}, {bySomeone, "admin", now.Add(-time.Hour)}, {stale, "admin", now.Add(-8 * 24 * time.Hour)}} {
		if _, err := st.RevokeRunner(ctx, c.r.ID, c.at, c.by, "not_mine"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListVisibleRunnersByOwner(ctx, owner, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if len(ids) != 3 || !slices.Contains(ids, fresh.ID) || !slices.Contains(ids, bySomeone.ID) || !slices.Contains(ids, oldClaimed.ID) {
		t.Errorf("visible = %v, want only the fresh runner %s, the one someone else revoked %s and the old claimed %s (not lapsed %s, owner-revoked %s, stale %s, other's %s)",
			ids, fresh.ID, bySomeone.ID, oldClaimed.ID, lapsed.ID, byOwner.ID, stale.ID, other.ID)
	}
}

// A purge clears the person as the revoker of a runner and as the minter of a token made for someone
// else, and leaves a self-minted token (minted_by = owner) and another admin's marks alone.
func TestPG_Runners_EraseAttributionOfClearsRevokerAndMinterOnly(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	leaver, other := "leaver-"+uuid.NewString(), "admin-"+uuid.NewString()
	byLeaver, byOther := claimed(t, st, ctx, "alice@example.com"), claimed(t, st, ctx, "bob@example.com")
	var tokens []uuid.UUID
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id = ANY($1)`, []uuid.UUID{byLeaver.ID, byOther.ID})
		_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id = ANY($1)`, tokens)
	})
	now := time.Now().UTC()
	if _, err := st.RevokeRunner(ctx, byLeaver.ID, now, leaver, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeRunner(ctx, byOther.ID, now, other, ""); err != nil {
		t.Fatal(err)
	}
	mint := func(owner, mintedBy string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO runner_registration_tokens (id, owner, minted_by, token_sha256, org_url_sha256, created_at, expires_at)
			VALUES ($1, $2, $3, $4, 'org', $5::timestamptz, $5::timestamptz + interval '1 hour')`, id, owner, mintedBy, id.String(), now); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, id)
		return id
	}
	forSomeone, forSelf, byAdmin := mint("carol@example.com", leaver), mint(leaver, leaver), mint("dave@example.com", other)

	n, err := st.EraseRunnerAttributionOf(ctx, leaver)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows changed = %d, want the revoked runner and the token made for someone else", n)
	}
	revokedBy := func(id uuid.UUID) (by string) {
		if err := pool.QueryRow(ctx, `SELECT revoked_by FROM runners WHERE id = $1`, id).Scan(&by); err != nil {
			t.Fatal(err)
		}
		return by
	}
	mintedBy := func(id uuid.UUID) (by string) {
		if err := pool.QueryRow(ctx, `SELECT minted_by FROM runner_registration_tokens WHERE id = $1`, id).Scan(&by); err != nil {
			t.Fatal(err)
		}
		return by
	}
	for _, c := range []struct{ name, got, want string }{
		{"runner the leaver revoked", revokedBy(byLeaver.ID), ""},
		{"runner another admin revoked", revokedBy(byOther.ID), other},
		{"token the leaver minted for someone", mintedBy(forSomeone), ""},
		{"token the leaver minted for themselves", mintedBy(forSelf), leaver},
		{"token another admin minted", mintedBy(byAdmin), other},
	} {
		if c.got != c.want {
			t.Errorf("%s: attribution = %q, want %q", c.name, c.got, c.want)
		}
	}
	if again, err := st.EraseRunnerAttributionOf(ctx, leaver); err != nil || again != 0 {
		t.Errorf("a second erase = %d, %v; want nothing left to change", again, err)
	}
}
