// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Two writers that both read one free slot: the first is held at its insert, after its count, and the
// second must wait for it on the scope's lock and then be refused at the cap, not insert a row past it.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_AdmissionCap_TwoWritersForTheLastSlot(t *testing.T) {
	const sub = "sub-cap"
	guarded := func(ctx context.Context) context.Context {
		return store.WithAdmissionCap(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: -1}), 2)
	}
	for _, tc := range []struct {
		name, insert string
		full         error
		count        string // $1 is the cap's scope
		scope        func(run uuid.UUID) any
		write        func(ctx context.Context, st store.PG, run uuid.UUID) error
	}{
		{"approval per run", "INSERT INTO approvals", store.ErrApprovalRunCap, `SELECT count(*) FROM approvals WHERE run_id = $1`,
			func(run uuid.UUID) any { return run },
			func(ctx context.Context, st store.PG, run uuid.UUID) error {
				_, err := st.CreateApproval(store.WithAdmissionCap(ctx, 2),
					pendingApproval(run, types.ApprovalEgressDomain, `{"host":"h`+uuid.NewString()+`.example"}`))
				return err
			}},
		{"live API token per person", "INSERT INTO api_tokens", store.ErrAPITokenCap, `SELECT count(*) FROM api_tokens WHERE principal = $1`,
			func(uuid.UUID) any { return sub },
			func(ctx context.Context, st store.PG, _ uuid.UUID) error {
				_, err := st.CreateAPIToken(guarded(ctx), types.APIToken{ID: uuid.New(), Principal: sub, Role: "user",
					UserType: types.UserTypeStandard, Name: "t", CreatedAt: time.Now().UTC()}, "wdn_"+uuid.NewString())
				return err
			}},
		{"SSH key per person", "INSERT INTO ssh_public_keys", store.ErrSSHKeyCap, `SELECT count(*) FROM ssh_public_keys WHERE principal = $1`,
			func(uuid.UUID) any { return sub },
			func(ctx context.Context, st store.PG, _ uuid.UUID) error {
				_, err := st.AddSSHKey(guarded(ctx), types.SSHPublicKey{Fingerprint: "SHA256:" + uuid.NewString(), Principal: sub,
					Name: "k", PublicKey: "ssh-ed25519 AAAA", Role: "user", CreatedAt: time.Now().UTC()})
				return err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := runsPGPoolIsolated(t)
			notify.SetActive(nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			st := store.NewPG(pool)
			run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
			if err := tc.write(ctx, st, run); err != nil {
				t.Fatalf("the first row, one slot left: %v", err)
			}
			held, h := heldStoreAt(t, pool, tc.insert)
			first := make(chan error, 1)
			go func() { first <- tc.write(ctx, held, run) }()
			waitHeld(ctx, t, h)
			second := make(chan error, 1)
			go func() { second <- tc.write(ctx, st, run) }()
			waitLockWaiters(ctx, t, pool, "advisory", 1, second)
			h.free()
			if err := <-first; err != nil {
				t.Fatalf("the writer that counted first: %v", err)
			}
			if err := <-second; !errors.Is(err, tc.full) {
				t.Fatalf("the writer that waited = %v, want %v", err, tc.full)
			}
			if n := countRows(t, pool, tc.count, tc.scope(run)); n != 2 {
				t.Fatalf("%d rows after two writers raced for the last of 2 slots, want 2", n)
			}
		})
	}
}

// A PUT that starts while a DELETE of the same name has removed the row but not committed lands above
// the deleted version: a client pinned to that version is never matched to the new content.
func TestPG_LaunchPreset_PutBehindAnUncommittedDeleteTakesANewVersion(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	put := func(desc string) (types.LaunchPreset, error) {
		got, _, err := st.PutLaunchPreset(ctx, types.LaunchPreset{Name: "ci", Description: desc, Request: json.RawMessage(`{}`), CreatedBy: "admin"})
		return got, err
	}
	deleted, err := put("one")
	if err != nil {
		t.Fatal(err)
	}
	// A floor row the test holds, so the DELETE removes the preset and then waits, uncommitted, on its floor upsert.
	if _, err := pool.Exec(ctx, `INSERT INTO launch_preset_version_floor (name, version) VALUES ('ci', 0)`); err != nil {
		t.Fatal(err)
	}
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM launch_preset_version_floor WHERE name = 'ci' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	deleting := make(chan error, 1)
	go func() {
		_, err := st.DeleteLaunchPreset(ctx, "ci")
		deleting <- err
	}()
	waitLockWaiters(ctx, t, pool, "", 1, deleting)
	type putResult struct {
		p   types.LaunchPreset
		err error
	}
	recreating := make(chan putResult, 1)
	go func() {
		p, err := put("two")
		recreating <- putResult{p, err}
	}()
	waitLockWaiters(ctx, t, pool, "", 2, deleting)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleting; err != nil {
		t.Fatalf("delete: %v", err)
	}
	got := <-recreating
	if got.err != nil {
		t.Fatalf("put behind the delete: %v", got.err)
	}
	if got.p.Version <= deleted.Version {
		t.Fatalf("the preset recreated behind a delete has version %d, want above the deleted %d", got.p.Version, deleted.Version)
	}
}

// heldStoreAt is a store on its own pool over pool's database whose first statement containing match is
// held at its start, its transaction open, and the hold.
func heldStoreAt(t *testing.T, pool *pgxpool.Pool, match string) (store.PG, *stmtHold) {
	t.Helper()
	h := &stmtHold{match: match, held: make(chan struct{}), freeCh: make(chan struct{})}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = h
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	t.Cleanup(h.free) // runs first: a failed test never leaves the held statement waiting
	return store.NewPG(p), h
}
