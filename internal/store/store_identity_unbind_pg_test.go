// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_UnbindIdentity: the unbind clears only the principal, so the next sign-in under another one
// binds the row (it was refused before), and it refuses what it must without writing: a row bound to a
// principal other than the one the caller read, an unbound row, a deactivated or purged one, and an
// unknown id.
func TestPG_UnbindIdentity(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	now := time.Now().UTC()
	issuer := "https://login.example/" + uuid.NewString() + "/v2.0"
	tenant, object := uuid.NewString(), uuid.NewString()
	login := func(principal string) (store.PrincipalIdentity, error) {
		return st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: issuer, TenantID: tenant, ObjectID: object}, now)
	}

	row, err := login("sub-old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET authority_epoch = 5, scim_external_id = 'ext-1' WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := login("sub-new"); !errors.Is(err, store.ErrIdentityBindingMismatch) {
		t.Fatalf("sign-in under another sub = %v, want ErrIdentityBindingMismatch", err)
	}

	if err := st.UnbindIdentity(ctx, row.ID, "sub-other"); !errors.Is(err, store.ErrIdentityRebound) {
		t.Fatalf("unbind from a principal the row is not bound to = %v, want ErrIdentityRebound", err)
	}
	if got, _ := st.GetIdentity(ctx, row.ID); got.Principal != "sub-old" {
		t.Fatalf("a refused unbind changed the row: %+v", got)
	}
	// A principal that still holds anything is refused: after the re-bind nothing sweeps it.
	for name, seed := range map[string]string{
		"an API token": `INSERT INTO api_tokens (id, principal, role, name, token_sha256) VALUES (gen_random_uuid(), 'sub-old', 'user', 't', gen_random_uuid()::text)`,
		"an SSH key":   `INSERT INTO ssh_public_keys (fingerprint, principal, role, public_key, name) VALUES ('SHA256:' || gen_random_uuid()::text, 'sub-old', 'member', 'ssh-ed25519 AAAA', 'k')`,
		"a live run":   `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target) VALUES (gen_random_uuid(), 'sub-old', 'claude-code', 'a/b', 'CC2', 'RUNNING', 'spiffe://x/' || gen_random_uuid()::text, 'docker')`,
	} {
		if _, err := pool.Exec(ctx, seed); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		var inUse *store.PrincipalInUseError
		if err := st.UnbindIdentity(ctx, row.ID, "sub-old"); !errors.As(err, &inUse) {
			t.Fatalf("unbind while the principal holds %s = %v, want *PrincipalInUseError", name, err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM api_tokens WHERE principal = 'sub-old'; DELETE FROM ssh_public_keys WHERE principal = 'sub-old'; DELETE FROM agent_runs WHERE created_by = 'sub-old'`); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := st.GetIdentity(ctx, row.ID); got.Principal != "sub-old" {
		t.Fatalf("a refused unbind changed the row: %+v", got)
	}
	if err := st.UnbindIdentity(ctx, row.ID, "sub-old"); err != nil {
		t.Fatalf("UnbindIdentity: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM oidc_session_revocations WHERE sub = 'sub-old'`); n != 1 {
		t.Errorf("%d session cutoffs for the released sub, want 1", n)
	}
	if err := st.UnbindIdentity(ctx, row.ID, "sub-old"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("unbind of an unbound row = %v, want ErrConflict", err)
	}
	bound, err := login("sub-new")
	if err != nil || bound.Principal != "sub-new" || bound.AuthorityEpoch != 5 || bound.ScimExternalID != "ext-1" {
		t.Fatalf("sign-in after the unbind = %+v (%v), want bound to sub-new with the epoch and SCIM linkage kept", bound, err)
	}

	for name, set := range map[string]string{
		"deactivated": `deactivated_at = now()`,
		"purged":      `purged_at = now()`,
	} {
		if _, err := pool.Exec(ctx, `UPDATE principal_identities SET `+set+` WHERE id = $1`, row.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.UnbindIdentity(ctx, row.ID, "sub-new"); !errors.Is(err, store.ErrIdentityDeactivated) {
			t.Errorf("unbind of a %s identity = %v, want ErrIdentityDeactivated", name, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = NULL, purged_at = NULL WHERE id = $1`, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UnbindIdentity(ctx, uuid.New(), "sub-new"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unbind of an unknown identity = %v, want ErrNotFound", err)
	}
}

// TestPG_UnbindIdentity_RefusesARowWithNoObjectID: a row found only by its principal would be orphaned by an
// unbind (the next sign-in inserts a second row with epoch 0 and no SCIM linkage), so it is refused and left alone.
func TestPG_UnbindIdentity_RefusesARowWithNoObjectID(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	principal := "plain-" + uuid.NewString()
	row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: "https://idp.example/" + uuid.NewString()}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UnbindIdentity(ctx, row.ID, principal); !errors.Is(err, store.ErrIdentityNotRebindable) {
		t.Fatalf("unbind of a row with no object id = %v, want ErrIdentityNotRebindable", err)
	}
	if got, _ := st.GetIdentity(ctx, row.ID); got.Principal != principal {
		t.Fatalf("the row was changed: %+v", got)
	}
}

// TestPG_IdentityGuard_RefusesAWriteQueuedBehindAnUnbind: a guarded token insert whose guard starts after an
// unbind has locked the row waits for it, then re-reads the rows by principal, finds none (the row is unbound
// now) and used to pass, leaving a token under the released sub that no leaver step reaches. The cutoff the
// unbind wrote refuses it instead, and a guarded insert after the cutoff's own moment is unaffected.
func TestPG_IdentityGuard_RefusesAWriteQueuedBehindAnUnbind(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := store.NewPG(pool)
	issuer := "https://login.example/" + uuid.NewString() + "/v2.0"
	row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: "sub-old", Issuer: issuer, TenantID: uuid.NewString(), ObjectID: uuid.NewString()}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	// The unbind's transaction, held open between taking the row and committing.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM principal_identities WHERE id = $1 FOR UPDATE`, row.ID); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		g := store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: "sub-old", Epoch: -1})
		_, err := st.CreateAPIToken(g, types.APIToken{ID: uuid.New(), Principal: "sub-old", Role: "user", UserType: types.UserTypeStandard,
			Name: "racer", CreatedAt: time.Now().UTC()}, "wdn_"+uuid.NewString())
		done <- err
	}()
	// Wait until the guarded insert is blocked on the row lock, so its transaction began before the cutoff.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid))
			AND query ILIKE '%principal_identities%FOR SHARE%')`, blockerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the guarded insert never queued behind the row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := tx.Exec(ctx, `UPDATE principal_identities SET principal = NULL WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ('sub-old', clock_timestamp())
		ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-done; !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Fatalf("the insert queued behind the unbind = %v, want ErrIdentityDeactivated", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM api_tokens WHERE principal = 'sub-old'`); n != 0 {
		t.Fatalf("%d token(s) landed under the released sub, want 0", n)
	}
	// An insert whose guard begins after the cutoff is not the racing case.
	g := store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: "sub-old", Epoch: -1})
	if _, err := st.CreateAPIToken(g, types.APIToken{ID: uuid.New(), Principal: "sub-old", Role: "user", UserType: types.UserTypeStandard,
		Name: "later", CreatedAt: time.Now().UTC()}, "wdn_"+uuid.NewString()); err != nil {
		t.Fatalf("a guarded insert after the cutoff = %v, want it to pass", err)
	}
}
