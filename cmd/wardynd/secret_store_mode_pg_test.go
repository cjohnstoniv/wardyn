// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The boot half of store mode (credential-storage design §2.3a.7), against a
// real Postgres and an in-memory external store: no age key is needed or
// generated, local rows left behind refuse boot by name, and a boot key whose
// external value is gone fails boot instead of being minted over (rule 17).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
)

// memExternal is a minimal secretstore.External named "vaultkv".
type memExternal struct {
	mu   sync.Mutex
	vals map[string][]byte
}

func (m *memExternal) key(owner, name string) string { return owner + "/" + name }
func (m *memExternal) Name() string                  { return vaultkv.Name }
func (m *memExternal) Describe() string              { return "Vault at test" }
func (m *memExternal) Put(_ context.Context, owner, name, _ string, v []byte, _ bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[m.key(owner, name)] = v
	return m.key(owner, name), nil
}
func (m *memExternal) Get(_ context.Context, owner, name, ref string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[ref]
	if ref != m.key(owner, name) || !ok {
		return nil, fmt.Errorf("refused: nothing at %s", ref)
	}
	return v, nil
}
func (m *memExternal) Check(ctx context.Context, owner, name, ref string) error {
	_, err := m.Get(ctx, owner, name, ref)
	return err
}
func (m *memExternal) Delete(_ context.Context, owner, name, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vals, m.key(owner, name))
	return nil
}
func (m *memExternal) Walk(context.Context) ([]secretstore.ExternalEntry, error) { return nil, nil }

func TestBuildSecretStore_StoreModeBootsWithNoAgeKey(t *testing.T) {
	pool := envelopeDB(t)
	ext := &memExternal{vals: map[string][]byte{}}
	s, err := buildSecretStore(t.Context(), pool, "", vaultkv.Name, ext)
	if err != nil {
		t.Fatalf("store-mode boot with no age key: %v", err)
	}
	first, err := loadOrCreateSigningKey(t.Context(), s)
	if err != nil {
		t.Fatalf("first boot mints the signing key: %v", err)
	}
	if len(ext.vals) != 1 {
		t.Fatalf("the signing key did not land in the external store (%d values)", len(ext.vals))
	}
	s2, err := buildSecretStore(t.Context(), pool, "", vaultkv.Name, ext)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateSigningKey(t.Context(), s2)
	if err != nil || !first.Equal(second) {
		t.Fatalf("second boot = (%v); want the same signing key back", err)
	}
	if got := storesExternally(s2); got != "Vault at test" {
		t.Fatalf("storesExternally = %q", got)
	}
}

func TestBuildSecretStore_StoreModeRefusesWhileLocalRowsRemain(t *testing.T) {
	pool := envelopeDB(t)
	id, _ := age.GenerateX25519Identity()
	local, err := buildSecretStore(t.Context(), pool, id.String(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	_, err = buildSecretStore(t.Context(), pool, "", vaultkv.Name, &memExternal{vals: map[string][]byte{}})
	if err == nil || !strings.Contains(err.Error(), "-migrate-secrets") {
		t.Fatalf("store-mode boot over a local row with no age key = %v; want a refusal naming -migrate-secrets", err)
	}
}

// Rule 17 at boot: the signing key's pointer row exists but its value is gone.
func TestLoadOrCreateSecret_NeverMintsOverAGoneExternalValue(t *testing.T) {
	pool := envelopeDB(t)
	ext := &memExternal{vals: map[string][]byte{}}
	s, err := buildSecretStore(t.Context(), pool, "", vaultkv.Name, ext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSigningKey(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	ext.vals = map[string][]byte{}
	if _, err := loadOrCreateSigningKey(t.Context(), s); err == nil {
		t.Fatal("boot minted a new signing key over a pointer row whose value is gone")
	}
	if len(ext.vals) != 0 {
		t.Fatal("a signing key was written over the lost one")
	}
}

// The boot keys live under platform/ (design §2.13): the vaultkv package's
// list must name every one of them.
func TestBootKeysAreThePlatformSet(t *testing.T) {
	for _, n := range []string{secretSigningKey, secretSessionKey, secretUISessionKey, secretSSHHostKey} {
		if !vaultkv.PlatformNames[n] {
			t.Errorf("boot key %q is not in vaultkv.PlatformNames", n)
		}
	}
	if len(vaultkv.PlatformNames) != 4 {
		t.Errorf("vaultkv.PlatformNames has %d names, want the four boot keys", len(vaultkv.PlatformNames))
	}
}
