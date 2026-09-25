// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The boot half of a key service (credential-storage design §2.3), against a
// real Postgres and an in-memory KEK: with WARDYN_KEK=transit and no age key,
// no ephemeral key is generated, and rows still sealed under the age key
// refuse boot by name, pointing at -rewrap; and -rewrap moving the rows onto
// the key service and back.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memKEK is a kek.KEK over a random AES key, binding each wrap to its row.
type memKEK struct{ key []byte }

func newMemKEK() *memKEK {
	k := make([]byte, kek.DEKSize)
	_, _ = rand.Read(k)
	return &memKEK{key: k}
}

func (m *memKEK) ID() string       { return "transit:transit/test" }
func (m *memKEK) Describe() string { return "Vault Transit at test" }
func (m *memKEK) Wrap(_ context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	aad, err := kek.WrapAAD(bind, m.ID())
	if err != nil {
		return nil, err
	}
	return kek.Seal(m.key, dek, aad)
}
func (m *memKEK) Unwrap(_ context.Context, w []byte, bind map[string]string) ([]byte, error) {
	aad, err := kek.WrapAAD(bind, m.ID())
	if err != nil {
		return nil, err
	}
	dek, err := kek.Open(m.key, w, aad)
	if err != nil {
		return nil, errors.New("refused")
	}
	return dek, nil
}

func TestBuildSecretStore_KeyServiceNeedsNoAgeKey(t *testing.T) {
	pool := envelopeDB(t)
	k := newMemKEK()
	s, err := buildSecretStore(t.Context(), pool, "", nil, "", storeClients{kek: k, kekWrites: true}, &capturingRecorder{})
	if err != nil {
		t.Fatalf("boot with a key service and no age key: %v", err)
	}
	if err := s.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(t.Context(), `SELECT kek_id FROM secrets WHERE name='k'`).Scan(&id); err != nil || id != k.ID() {
		t.Fatalf("kek_id = (%q, %v), want %q", id, err, k.ID())
	}
	if !secretsDurable("", s) || keyService(s) != "Vault Transit at test" {
		t.Fatalf("durable=%v keyService=%q; want durable, described", secretsDurable("", s), keyService(s))
	}
	// Read-only, the key service does not make an install durable on its own.
	ro, err := buildSecretStore(t.Context(), envelopeDB(t), "", nil, "", storeClients{kek: k}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if keyService(ro) != "" {
		t.Fatalf("a read-only key service is described as the one that wraps writes: %q", keyService(ro))
	}
}

func TestBuildSecretStore_KeyServiceRefusesWhileLocalRowsRemain(t *testing.T) {
	pool := envelopeDB(t)
	id, _ := age.GenerateX25519Identity()
	local, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	_, err = buildSecretStore(t.Context(), pool, "", nil, "", storeClients{kek: newMemKEK(), kekWrites: true}, &capturingRecorder{})
	if err == nil || !strings.Contains(err.Error(), "wardynd -rewrap") {
		t.Fatalf("key-service boot over a local row with no age key = %v; want a refusal naming -rewrap", err)
	}
}

// recAudit collects audit events.
type recAudit struct{ evs []types.AuditEvent }

func (r *recAudit) Record(_ context.Context, ev types.AuditEvent) error {
	r.evs = append(r.evs, ev)
	return nil
}

func rowKEKs(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT name, kek_id FROM secrets`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err != nil {
			t.Fatal(err)
		}
		got[n] = k
	}
	return got
}

// TestRewrapMode_KeyServiceBothWays is `wardynd -rewrap`'s key-service path
// through the one command's body: with WARDYN_KEK=transit it moves every local
// row onto the key service — the boot keys too, from under the age key while
// WARDYN_PLATFORM_KEY_FILE is set — and writes one secret.rewrap row naming the
// key service and the count, never a secret; the store then boots with no age
// key. Without the key service configured, a run aborts on the first row under
// it and commits nothing. With the key service read-only, the rows move back
// to the local key of their purpose, and the store boots with no key service.
func TestRewrapMode_KeyServiceBothWays(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	id, platform := mustAgeIdentity(t), mustAgeIdentity(t)
	local, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if err := local.Put(ctx, n, []byte("v-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	orig, err := loadOrCreateSigningKey(ctx, local)
	if err != nil {
		t.Fatal(err)
	}

	k := newMemKEK()
	rec := &recAudit{}
	if err := rewrapKeys(ctx, rec, secretstore.Deps{Pool: pool, AgeIdentity: id, PlatformIdentity: platform, KEK: k, KEKWrites: true}); err != nil {
		t.Fatalf("-rewrap onto the key service: %v", err)
	}
	for n, got := range rowKEKs(t, pool) {
		if got != k.ID() {
			t.Fatalf("after -rewrap onto the key service, %s is under %q", n, got)
		}
	}
	if len(rec.evs) != 1 || rec.evs[0].Action != "secret.rewrap" || rec.evs[0].Actor != rewrapActor || rec.evs[0].Target != "pg" {
		t.Fatalf("audit = %+v; want one secret.rewrap by %s", rec.evs, rewrapActor)
	}
	var data map[string]any
	_ = json.Unmarshal(rec.evs[0].Data, &data)
	if data["secrets"] != float64(3) || data["key_service"] != k.ID() || data["platform_key_separate"] != true || data["key_version"] != nil ||
		strings.Contains(string(rec.evs[0].Data), `"a"`) || strings.Contains(string(rec.evs[0].Data), secretSigningKey) {
		t.Fatalf("secret.rewrap data = %s", rec.evs[0].Data)
	}

	svc, err := buildSecretStore(ctx, pool, "", nil, "", storeClients{kek: k, kekWrites: true}, &capturingRecorder{})
	if err != nil {
		t.Fatalf("boot with no age key after -rewrap: %v", err)
	}
	if got, err := loadOrCreateSigningKey(ctx, svc); err != nil || !got.Equal(orig) {
		t.Fatalf("signing key under the key service = %v; want the same key", err)
	}
	if err := rewrapKeys(ctx, &recAudit{}, secretstore.Deps{Pool: pool, AgeIdentity: id, PlatformIdentity: platform, KEK: k, KEKWrites: true}); err != nil {
		t.Fatalf("second -rewrap: %v", err)
	}

	before := rowKEKs(t, pool)
	err = rewrapKeys(ctx, &recAudit{}, secretstore.Deps{Pool: pool, AgeIdentity: id, PlatformIdentity: platform})
	if err == nil || !strings.Contains(err.Error(), "ABORTED") || !strings.Contains(err.Error(), "not configured to reach") {
		t.Fatalf("-rewrap with the key service unset over its rows = %v; want an abort naming the key", err)
	}
	if after := rowKEKs(t, pool); len(after) != len(before) || after["a"] != before["a"] || after[secretSigningKey] != before[secretSigningKey] {
		t.Fatalf("an aborted -rewrap committed rows: %v -> %v", before, after)
	}

	if err := rewrapKeys(ctx, &recAudit{}, secretstore.Deps{Pool: pool, AgeIdentity: id, PlatformIdentity: platform, KEK: k}); err != nil {
		t.Fatalf("-rewrap back to the local key: %v", err)
	}
	for n, got := range rowKEKs(t, pool) {
		want := "local/cred:"
		if n == secretSigningKey {
			want = "local/platform:"
		}
		if !strings.HasPrefix(got, want) {
			t.Fatalf("after -rewrap back, %s is under %q, want %s…", n, got, want)
		}
	}
	back, err := buildSecretStore(ctx, pool, id.String(), platform, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := loadOrCreateSigningKey(ctx, back); err != nil || !got.Equal(orig) {
		t.Fatalf("signing key back under the platform key = %v; want the same key", err)
	}
	if v, err := back.Get(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), "a"); err != nil || string(v) != "v-a" {
		t.Fatalf("Get(a) back under the local key = (%q, %v)", v, err)
	}
}
