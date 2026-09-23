// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The boot half of a key service (credential-storage design §2.3), against a
// real Postgres and an in-memory KEK: with WARDYN_KEK=transit and no age key,
// no ephemeral key is generated, and rows still sealed under the age key
// refuse boot by name, pointing at -rewrap.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
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
	s, err := buildSecretStore(t.Context(), pool, "", "", storeClients{kek: k, kekWrites: true})
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
	ro, err := buildSecretStore(t.Context(), envelopeDB(t), "", "", storeClients{kek: k})
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
	local, err := buildSecretStore(t.Context(), pool, id.String(), "", storeClients{})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	_, err = buildSecretStore(t.Context(), pool, "", "", storeClients{kek: newMemKEK(), kekWrites: true})
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

// `wardynd -rewrap` moves every local row under the key service and writes
// one secret.rewrap row naming the key and the count, never a secret name.
func TestRewrapMode_MovesRowsAndAuditsOnce(t *testing.T) {
	pool := envelopeDB(t)
	id, _ := age.GenerateX25519Identity()
	local, err := buildSecretStore(t.Context(), pool, id.String(), "", storeClients{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if err := local.Put(t.Context(), n, []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	k := newMemKEK()
	s, err := buildSecretStore(t.Context(), pool, id.String(), "", storeClients{kek: k, kekWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recAudit{}
	if err := rewrapMode(t.Context(), s.(*secretstorepg.Store), rec); err != nil {
		t.Fatalf("rewrapMode: %v", err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Action != "secret.rewrap" || rec.evs[0].Actor != rewrapActor || rec.evs[0].Target != k.ID() {
		t.Fatalf("audit = %+v; want one secret.rewrap by %s targeting %s", rec.evs, rewrapActor, k.ID())
	}
	var data map[string]any
	_ = json.Unmarshal(rec.evs[0].Data, &data)
	if data["count"] != float64(2) || data["kek_id"] != k.ID() || strings.Contains(string(rec.evs[0].Data), `"a"`) {
		t.Fatalf("secret.rewrap data = %s", rec.evs[0].Data)
	}
	if _, err := buildSecretStore(t.Context(), pool, "", "", storeClients{kek: k, kekWrites: true}); err != nil {
		t.Fatalf("boot with no age key after -rewrap: %v", err)
	}
}
