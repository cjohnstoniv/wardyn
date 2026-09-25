// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The boot half of store mode (credential-storage design §2.3a.7), against a
// real Postgres and an in-memory external store (or the real Vault client
// against a minimal Vault): no age key is needed or generated, local rows left
// behind refuse boot by name, and a boot key whose external value is gone or
// not in Wardyn's format fails boot instead of being minted over (rule 17).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
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
func (m *memExternal) Ref(owner, name, _ string) (string, error) { return m.key(owner, name), nil }
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
	s, err := buildSecretStore(t.Context(), pool, "", nil, vaultkv.Name, ext, 0, &capturingRecorder{})
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
	rec := &capturingRecorder{}
	s2, err := buildSecretStore(t.Context(), pool, "", nil, vaultkv.Name, ext, 0, rec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateSigningKey(t.Context(), s2)
	if err != nil || !first.Equal(second) {
		t.Fatalf("second boot = (%v); want the same signing key back", err)
	}
	// Store mode is audited like local mode: the read from Vault is one
	// secret.read naming the store.
	if len(rec.got) != 1 || rec.got[0].Action != "secret.read" || !strings.Contains(string(rec.got[0].Data), `"store":"vaultkv"`) ||
		!strings.Contains(string(rec.got[0].Data), `"purpose":"boot"`) {
		t.Fatalf("store-mode boot read recorded %+v; want one secret.read with store vaultkv and purpose boot", rec.got)
	}
	if got := storesExternally(s2); got != "Vault at test" {
		t.Fatalf("storesExternally = %q", got)
	}
}

func TestBuildSecretStore_StoreModeRefusesWhileLocalRowsRemain(t *testing.T) {
	pool := envelopeDB(t)
	id, _ := age.GenerateX25519Identity()
	local, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", nil, 0, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	_, err = buildSecretStore(t.Context(), pool, "", nil, vaultkv.Name, &memExternal{vals: map[string][]byte{}}, 0, &capturingRecorder{})
	if err == nil || !strings.Contains(err.Error(), "-migrate-secrets") {
		t.Fatalf("store-mode boot over a local row with no age key = %v; want a refusal naming -migrate-secrets", err)
	}
}

// Rule 17 at boot: the signing key's pointer row exists but its value is gone.
func TestLoadOrCreateSecret_NeverMintsOverAGoneExternalValue(t *testing.T) {
	pool := envelopeDB(t)
	ext := &memExternal{vals: map[string][]byte{}}
	s, err := buildSecretStore(t.Context(), pool, "", nil, vaultkv.Name, ext, 0, &capturingRecorder{})
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

// miniVault is just enough Vault KV v2 for the real vaultkv client's Put and
// Get: token lookup, and metadata and data reads and writes. dataWrites counts
// every data write.
type miniVault struct {
	mu         sync.Mutex
	meta, data map[string]any
	dataWrites int
}

func (v *miniVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	route, rel, _ := strings.Cut(strings.TrimPrefix(p, "wardyn/"), "/")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	var out any
	switch {
	case p == "auth/token/lookup-self":
		out = map[string]any{"data": map[string]any{"ttl": 0}}
	case p == "wardyn/config":
		out = map[string]any{"data": map[string]any{"max_versions": 0}}
	case r.Method == http.MethodPost && route == "metadata":
		v.meta[rel] = body["custom_metadata"]
	case r.Method == http.MethodPost && route == "data":
		v.data[rel] = body["data"]
		v.dataWrites++
	case v.meta[rel] == nil:
		w.WriteHeader(http.StatusNotFound)
		return
	case route == "metadata":
		out = map[string]any{"data": map[string]any{"current_version": 1, "max_versions": 1,
			"custom_metadata": v.meta[rel], "versions": map[string]any{"1": map[string]any{}}}}
	default:
		out = map[string]any{"data": map[string]any{"data": v.data[rel], "metadata": map[string]any{"custom_metadata": v.meta[rel]}}}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// Rule 17 through the real Vault client: a writer at Vault replaced the
// signing key's data with another shape (its custom_metadata survives a data
// write, so the binding still matches). Boot refuses it rather than reading
// zero bytes and minting a fresh signing key over it.
func TestLoadOrCreateSecret_NeverMintsOverAValueNotInWardynsFormat(t *testing.T) {
	pool := envelopeDB(t)
	v := &miniVault{meta: map[string]any{}, data: map[string]any{}}
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	ext, err := vaultkv.New(t.Context(), vaultkv.Config{Addr: srv.URL, Auth: vaultkv.AuthTokenFile, TokenFile: tokenFile,
		Mount: "wardyn", Prefix: "ns1", MaxVersions: 1})
	if err != nil {
		t.Fatal(err)
	}
	s, err := buildSecretStore(t.Context(), pool, "", nil, vaultkv.Name, ext, 0, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSigningKey(t.Context(), s); err != nil {
		t.Fatalf("first boot mints the signing key: %v", err)
	}
	v.mu.Lock()
	v.data["ns1/platform/"+secretSigningKey] = map[string]any{"password": "x"}
	writes := v.dataWrites
	v.mu.Unlock()
	if _, err := loadOrCreateSigningKey(t.Context(), s); err == nil {
		t.Fatal("boot accepted a signing key whose Vault data holds no value")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataWrites != writes {
		t.Fatal("a fresh signing key was written over the one at Vault")
	}
}

// failingExternal refuses every write.
type failingExternal struct{ memExternal }

func (*failingExternal) Put(context.Context, string, string, string, []byte, bool) (string, error) {
	return "", fmt.Errorf("vault POST wardyn/data/x: 404")
}

// An aborted migration is audited, not only a successful one: secret.migrate
// with outcome failure and the rows committed before the abort.
func TestMigrateMode_AuditsAnAbort(t *testing.T) {
	pool := envelopeDB(t)
	id, _ := age.GenerateX25519Identity()
	ext := &failingExternal{memExternal{vals: map[string][]byte{}}}
	s, err := newSecretStore(t.Context(), pool, id.String(), nil, "", ext, 0, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	rec := &fakeAuditRecorder{}
	if err := migrateMode(t.Context(), s.(*secretstorepg.Store), rec, vaultkv.Name); err == nil {
		t.Fatal("migrateMode succeeded with every write refused")
	}
	if rec.last.Action != "secret.migrate" || rec.last.Outcome != "failure" || !strings.Contains(string(rec.last.Data), `"count":0`) {
		t.Fatalf("last audit row = %s %s %s; want secret.migrate failure with count 0", rec.last.Action, rec.last.Outcome, rec.last.Data)
	}
}

// A migration that succeeds records one secret.read per value it moved, with
// purpose migrate and never the value, then one secret.migrate with the count.
func TestMigrateMode_AuditsEveryReadThenTheMove(t *testing.T) {
	pool := envelopeDB(t)
	id, _ := age.GenerateX25519Identity()
	ext := &memExternal{vals: map[string][]byte{}}
	s, err := newSecretStore(t.Context(), pool, id.String(), nil, "", ext, 0, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, w := range []struct{ owner, name, value string }{
		{"", "k1", "value-one"}, {"", "k2", "value-two"}, {"alice", "k1", "value-alice"},
	} {
		if err := s.For(w.owner).Put(t.Context(), w.name, []byte(w.value)); err != nil {
			t.Fatal(err)
		}
		values[w.owner+"/"+w.name] = w.value
	}
	rec := &capturingRecorder{}
	if err := migrateMode(t.Context(), s.(*secretstorepg.Store), rec, vaultkv.Name); err != nil {
		t.Fatalf("migrateMode: %v", err)
	}
	if len(rec.got) != 4 {
		t.Fatalf("migration recorded %d audit rows, want 3 secret.read + 1 secret.migrate: %+v", len(rec.got), rec.got)
	}
	for _, ev := range rec.got[:3] {
		var d map[string]any
		if ev.Action != "secret.read" || ev.Outcome != "success" || json.Unmarshal(ev.Data, &d) != nil || d["purpose"] != "migrate" {
			t.Fatalf("read row = %s %s %s, want a secret.read success with purpose migrate", ev.Action, ev.Outcome, ev.Data)
		}
		for _, v := range values {
			if strings.Contains(string(ev.Data), v) || strings.Contains(ev.Target, v) {
				t.Fatalf("read row carries the value %q: %s", v, ev.Data)
			}
		}
	}
	last := rec.got[3]
	var d map[string]any
	if last.Action != "secret.migrate" || last.Outcome != "success" || json.Unmarshal(last.Data, &d) != nil ||
		d["count"] != float64(3) || d["from"] != "pg" || d["to"] != vaultkv.Name {
		t.Fatalf("last row = %s %s %s, want secret.migrate success, count 3, from pg, to vaultkv", last.Action, last.Outcome, last.Data)
	}
	if len(ext.vals) != 3 {
		t.Fatalf("%d values reached the external store, want 3", len(ext.vals))
	}
}

// -reconcile lists every owner and name in the store, so it leaves one
// audit row with its counts.
func TestReconcileMode_Audits(t *testing.T) {
	pool := envelopeDB(t)
	ext := &memExternal{vals: map[string][]byte{}}
	s, err := newSecretStore(t.Context(), pool, "", nil, vaultkv.Name, ext, 0, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	rec := &fakeAuditRecorder{}
	if err := reconcileMode(t.Context(), s.(*secretstorepg.Store), rec); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 || rec.last.Action != "secret.reconcile" || rec.last.Outcome != "success" || !strings.Contains(string(rec.last.Data), `"checked":1`) {
		t.Fatalf("audit = %d rows, last %s %s %s; want one secret.reconcile success with checked 1", rec.calls, rec.last.Action, rec.last.Outcome, rec.last.Data)
	}
}
