// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The lost-local-key recovery (ephemeralKeyRecoverySQL) against a real
// Postgres: after it, a boot with a new key or with none starts, and it
// removes only what the lost key sealed.

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestPG_LostLocalKeyRecoveryRestoresBoot: an age key that sealed rows and
// wrapped a person's two principal keys is gone. Neither a new key nor an
// ephemeral one boots; after the recovery both do, the person's credential
// is gone rather than unreadable, and their next write gets a new generation.
func TestPG_LostLocalKeyRecoveryRestoresBoot(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	on := storeClients{principalKeys: true}
	boot := func(ageKey string) (secretstore.Store, error) {
		s, err := newSecretStore(ctx, pool, ageKey, nil, "", on, &capturingRecorder{})
		if err != nil {
			return nil, err
		}
		return s, verifyKeyDomains(ctx, s)
	}

	lost, _ := age.GenerateX25519Identity()
	s, err := boot(lost.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSigningKey(ctx, unlocked(s)); err != nil {
		t.Fatal(err)
	}
	if err := s.For("alice").Put(ctx, "github-token", []byte("before")); err != nil {
		t.Fatal(err)
	}
	_, key, err := s.(*secretstorepg.Store).SubjectKeys().Current(ctx, "alice", subjectkey.PurposeAuditSeal)
	if err != nil {
		t.Fatal(err)
	}
	clear(key)

	fresh, _ := age.GenerateX25519Identity()
	if _, err := boot(fresh.String()); err == nil {
		t.Fatal("a new key booted over principal keys only the lost key opens")
	}
	if _, err := boot(""); err == nil {
		t.Fatal("an ephemeral key booted over rows sealed under the lost key")
	}

	if _, err := pool.Exec(ctx, ephemeralKeyRecoverySQL); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if _, err := boot(""); err != nil {
		t.Fatalf("an ephemeral boot after the recovery: %v", err)
	}
	s, err = boot(fresh.String())
	if err != nil {
		t.Fatalf("a boot with a new key after the recovery: %v", err)
	}
	if _, err := loadOrCreateSigningKey(ctx, unlocked(s)); err != nil {
		t.Fatalf("minting the signing key after the recovery: %v", err)
	}
	read := secretstore.WithPurpose(ctx, secretstore.PurposeStatus)
	if _, err := s.For("alice").Get(read, "github-token"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("alice's credential after the recovery: %v; want not found, since it was sealed under the lost key", err)
	}
	if err := s.For("alice").Put(ctx, "github-token", []byte("after")); err != nil {
		t.Fatalf("alice's write after the recovery: %v", err)
	}
	if v, err := s.For("alice").Get(read, "github-token"); err != nil || string(v) != "after" {
		t.Fatalf("alice's credential = (%q, %v); want the new value", v, err)
	}
	var gens string
	if err := pool.QueryRow(ctx, `SELECT string_agg(purpose || '/' || version || '/' || (destroyed_at IS NOT NULL), ',' ORDER BY purpose, version)
		FROM principal_keys WHERE owner='alice'`).Scan(&gens); err != nil {
		t.Fatal(err)
	}
	if want := "audit-seal/1/true,cred/1/true,cred/2/false"; gens != want {
		t.Fatalf("alice's generations (purpose/version/destroyed) = %s; want %s", gens, want)
	}
}

// TestPG_LostLocalKeyRecoveryKeepsNamesInOlderAuditRows: under
// WARDYN_AUDIT_SEAL=full a row stores a person as a subject and reads them as
// erased once their audit-seal key was destroyed after the row. The recovery
// destroys that key without erasing anyone, so the row still names the person;
// only its sealed field, whose key is gone, reads erased. An erasure after the
// recovery still turns the name into erased.
func TestPG_LostLocalKeyRecoveryKeepsNamesInOlderAuditRows(t *testing.T) {
	rg := newSealRigMode(t, audit.SealFull)
	ctx := t.Context()
	if _, err := rg.pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "alice", Issuer: "https://idp.test"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rg.decide("alice", "alice typed these words")
	if got := rg.reasons(rg.b); got["alice"] != "alice typed these words" {
		t.Fatalf("before the recovery wardynd B reads %v, want alice's field opened", got)
	}

	if _, err := rg.pool.Exec(ctx, ephemeralKeyRecoverySQL); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	var tombstoned bool
	if err := rg.pool.QueryRow(ctx, `SELECT bool_and(destroyed_at = 'epoch') FROM principal_keys WHERE owner='alice' AND purpose='audit-seal'`).Scan(&tombstoned); err != nil || !tombstoned {
		t.Fatalf("alice's audit-seal key after the recovery: destroyed at the epoch = %v (%v); the rig's key is local, so the recovery must reach it", tombstoned, err)
	}
	for name, h := range map[string]http.Handler{"A": rg.a, "B": rg.b} {
		if got := rg.actorsRead(h, ""); !slices.Equal(got, []string{"alice"}) {
			t.Errorf("wardynd %s after the recovery reads actors %v, want alice by name: nobody was erased", name, got)
		}
		if got := rg.reasons(h); got["alice"] != audit.ErasedValue {
			t.Errorf("wardynd %s after the recovery reads %v, want alice's sealed field erased: its key is gone", name, got)
		}
	}

	// An erasure after the recovery still wins: it destroys a key generation dated now, which is after the row.
	if w := rg.call(rg.a, http.MethodPost, "/api/v1/people/alice/erasure", `{"scopes":["audit_personal_fields"]}`); w.Code != http.StatusOK {
		t.Fatalf("erasure after the recovery = %d %s", w.Code, w.Body)
	}
	for name, h := range map[string]http.Handler{"A": rg.a, "B": rg.b} {
		if got := rg.actorsRead(h, ""); !slices.Equal(got, []string{audit.ErasedValue}) {
			t.Errorf("wardynd %s after a later erasure reads actors %v, want alice erased", name, got)
		}
	}
}

// keyRow and secretRow are a fixture row and what the recovery must leave of it.
type keyRow struct {
	owner, purpose     string
	version            int
	domain, kekID      string
	destroyed, retired bool // destroyed_at set before; superseded_at set
	wantTombstone      bool
}

type secretRow struct {
	owner, name string
	encVersion  int
	kekID       string
	wantGone    bool
}

// TestPG_LostLocalKeyRecoveryRemovesOnlyTheLostKeyRows names every row the
// recovery may change and pins that the rest survive byte for byte: principal
// keys under a key service or a declared domain, a generation already
// destroyed and a tombstone generation, a credential row under a surviving
// generation (one whose kek_id text matches a removed row's), and service and
// pointer rows. The variant docs/OPERATIONS.md gives for a kept
// WARDYN_PLATFORM_KEY_FILE keeps the local/platform: rows too.
func TestPG_LostLocalKeyRecoveryRemovesOnlyTheLostKeyRows(t *testing.T) {
	const local = "kek_id LIKE 'local/%'"
	if n := strings.Count(ephemeralKeyRecoverySQL, local); n != 3 {
		t.Fatalf("the recovery names %q %d times; the platform-file variant replaces every one, so keep it at 3", local, n)
	}
	keys := []keyRow{
		{owner: "alice", purpose: "cred", version: 1, domain: "default", kekID: "local/cred:0123456789abcdef", wantTombstone: true},
		{owner: "alice", purpose: "audit-seal", version: 1, domain: "default", kekID: "local/cred:0123456789abcdef", wantTombstone: true},
		{owner: "bob", purpose: "cred", version: 1, domain: "default", kekID: "local:0123456789abcdef", wantTombstone: true},
		{owner: "gina", purpose: "cred", version: 1, domain: "default", kekID: "local/cred:0123456789abcdef", retired: true, wantTombstone: true},
		{owner: "gina", purpose: "cred", version: 2, domain: "eu", kekID: "transit:eu-key"},
		{owner: "carol", purpose: "cred", version: 1, domain: "default", kekID: "transit:wardyn-cred"},
		{owner: "dave", purpose: "cred", version: 1, domain: "eu", kekID: "transit:eu-key"},
		{owner: "hank", purpose: "cred", version: 1, domain: "default", kekID: "transit:wardyn-cred"},
		{owner: "hank", purpose: "audit-seal", version: 1, domain: "default", kekID: "local/cred:0123456789abcdef", wantTombstone: true},
		{owner: "erin", purpose: "cred", version: 1, domain: "default", kekID: "local/cred:fedcba9876543210", destroyed: true},
		{owner: "frank", purpose: "audit-seal", version: 1, destroyed: true},
	}
	secrets := []secretRow{
		{"", "wardyn-signing-key", 1, "local/platform:0123456789abcdef", true},
		{"", "operator-token", 1, "local/cred:0123456789abcdef", true},
		{"", "pre-split-token", 1, "local:0123456789abcdef", true},
		{"", "legacy-token", 0, "", true},
		{"alice", "github-token", 3, "pk:v1", true},
		{"gina", "old-token", 3, "pk:v1", true},
		{"gina", "new-token", 3, "pk:v2", false},
		{"carol", "github-token", 3, "pk:v1", false},
		{"dave", "github-token", 3, "pk:v1", false},
		{"hank", "github-token", 3, "pk:v1", false},
		{"erin", "github-token", 3, "pk:v1", false},
		{"", "service-token", 1, "transit:wardyn-cred", false},
		{"", "pointer-token", 2, "vaultkv:wardyn/pointer-token", false},
	}
	for _, tc := range []struct {
		name, sql string
		keep      string // a wantGone row this variant keeps
	}{
		{"as given", ephemeralKeyRecoverySQL, ""},
		{"platform file kept", strings.ReplaceAll(ephemeralKeyRecoverySQL, local, "kek_id LIKE 'local/cred:%'"), "/wardyn-signing-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := envelopeDB(t)
			ctx := t.Context()
			for _, k := range keys {
				q := `INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key, superseded_at)
					VALUES ($1, $2, $3, $4, $5, '\x01'::bytea, CASE WHEN $6 THEN now() - interval '2 days' END)`
				if k.destroyed {
					q = `INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key, destroyed_at, handle, superseded_at)
						VALUES ($1, $2, $3, $4, $5, NULL, now() - interval '1 day', NULL, CASE WHEN $6 THEN now() - interval '2 days' END)`
				}
				if _, err := pool.Exec(ctx, q, k.owner, k.purpose, k.version, k.domain, k.kekID, k.retired); err != nil {
					t.Fatalf("seed key %s/%s/%d: %v", k.owner, k.purpose, k.version, err)
				}
			}
			for _, r := range secrets {
				if _, err := pool.Exec(ctx, `INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext)
					VALUES ($1, $2, $3, $4, '\x02'::bytea, '\x03'::bytea)`, r.owner, r.name, r.encVersion, r.kekID); err != nil {
					t.Fatalf("seed row %s/%s: %v", r.owner, r.name, err)
				}
			}
			keysBefore, rowsBefore := keySnapshot(t, pool), secretSnapshot(t, pool)

			if _, err := pool.Exec(ctx, tc.sql); err != nil {
				t.Fatalf("recovery: %v", err)
			}

			keysAfter, rowsAfter := keySnapshot(t, pool), secretSnapshot(t, pool)
			if len(keysAfter) != len(keys) {
				t.Errorf("principal_keys holds %d rows after the recovery, want all %d: it never deletes one", len(keysAfter), len(keys))
			}
			for _, k := range keys {
				id := fmt.Sprintf("%s/%s/%d", k.owner, k.purpose, k.version)
				before, after := keysBefore[id], keysAfter[id]
				if !k.wantTombstone {
					if after != before {
						t.Errorf("key %s changed: %+v -> %+v; it is not under the lost key", id, before, after)
					}
					continue
				}
				if !after.destroyed || !after.epoch || after.wrapped || after.handle {
					t.Errorf("key %s after the recovery = %+v; want destroyed_at the epoch, wrapped_key and handle cleared", id, after)
				}
				after.destroyed, after.epoch, after.destroyedAt, after.wrapped, after.handle = before.destroyed, before.epoch, before.destroyedAt, before.wrapped, before.handle
				if after != before {
					t.Errorf("key %s: the recovery changed more than its tombstone columns: %+v -> %+v", id, keysBefore[id], keysAfter[id])
				}
			}
			for _, r := range secrets {
				id := r.owner + "/" + r.name
				after, kept := rowsAfter[id]
				switch {
				case r.wantGone && id != tc.keep:
					if kept {
						t.Errorf("row %s survived; it was sealed under the lost key", id)
					}
				case !kept:
					t.Errorf("row %s was deleted; it is not under the lost key", id)
				case after != rowsBefore[id]:
					t.Errorf("row %s changed: %q -> %q", id, rowsBefore[id], after)
				}
			}
			ps, err := secretstorepg.New(pool, mustIdentity(t))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.keep != "" {
				want = 1 // the kept local/platform: row, which its file still opens
			}
			if n, err := ps.LocalRows(ctx); err != nil || n != want {
				t.Errorf("LocalRows after the recovery = (%d, %v); want %d", n, err, want)
			}
		})
	}
}

// keyState is one principal_keys row as the recovery may see it.
type keyState struct {
	domain, kekID, superseded, created, destroyedAt string
	destroyed, epoch, wrapped, handle               bool
}

func keySnapshot(t *testing.T, pool *pgxpool.Pool) map[string]keyState {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT owner || '/' || purpose || '/' || version, domain, kek_id,
		COALESCE(superseded_at::text, ''), created_at::text, COALESCE(destroyed_at::text, ''),
		destroyed_at IS NOT NULL, COALESCE(destroyed_at = 'epoch', false), wrapped_key IS NOT NULL, handle IS NOT NULL FROM principal_keys`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]keyState{}
	for rows.Next() {
		var id string
		var k keyState
		if err := rows.Scan(&id, &k.domain, &k.kekID, &k.superseded, &k.created, &k.destroyedAt, &k.destroyed, &k.epoch, &k.wrapped, &k.handle); err != nil {
			t.Fatal(err)
		}
		out[id] = k
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func secretSnapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT owned_by || '/' || name,
		concat_ws('|', enc_version, kek_id, encode(wrapped_dek, 'hex'), encode(ciphertext, 'hex'), value_rev, updated_at) FROM secrets`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		out[id] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
