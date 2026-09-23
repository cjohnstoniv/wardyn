// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
)

// TestBuildSecretStore_StoreModeBootsUnderFIPSOnly: store mode with no
// WARDYN_AGE_KEY boots under GODEBUG=fips140=only through the real Vault client
// against a minimal Vault, mints every boot key and the internal CA, and a
// second boot reads the same ones back. A local-mode boot in the same process
// is refused, so a store-mode boot that reached the age key (X25519) would fail
// here too.
func TestBuildSecretStore_StoreModeBootsUnderFIPSOnly(t *testing.T) {
	if os.Getenv("WARDYN_TEST_PG") == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed store-mode boot test")
	}
	if !secretstoretest.UnderFIPSOnly(t) {
		return
	}
	ctx := t.Context()
	pool := envelopeDB(t)
	srv := httptest.NewServer(&miniVault{meta: map[string]any{}, data: map[string]any{}})
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	ext, err := vaultkv.New(ctx, vaultkv.Config{Addr: srv.URL, Auth: vaultkv.AuthTokenFile, TokenFile: tokenFile,
		Mount: "wardyn", Prefix: "ns1", MaxVersions: 1})
	if err != nil {
		t.Fatal(err)
	}

	boot := func() [][]byte {
		t.Helper()
		s, err := buildSecretStore(ctx, pool, "", vaultkv.Name, ext)
		if err != nil {
			t.Fatalf("store-mode boot under fips140=only: %v", err)
		}
		if _, err := loadOrCreateSigningKey(ctx, s); err != nil {
			t.Fatalf("signing key: %v", err)
		}
		if _, err := loadOrCreateSessionKey(ctx, s); err != nil {
			t.Fatalf("session key: %v", err)
		}
		if _, err := loadOrCreateUISessionKey(ctx, s); err != nil {
			t.Fatalf("ui session key: %v", err)
		}
		if _, err := loadOrCreateSSHHostKey(ctx, s); err != nil {
			t.Fatalf("ssh host key: %v", err)
		}
		if _, err := loadHopTLS(ctx, s, "https://wardynd.example:8443"); err != nil {
			t.Fatalf("internal CA: %v", err)
		}
		var raw [][]byte
		for _, n := range []string{secretSigningKey, secretSessionKey, secretUISessionKey, secretSSHHostKey, secretInternalCA} {
			v, err := s.Get(ctx, n)
			if err != nil {
				t.Fatalf("read back %s: %v", n, err)
			}
			raw = append(raw, v)
		}
		if storesExternally(s) == "" {
			t.Fatal("the store-mode boot is not writing to Vault")
		}
		return raw
	}
	first, second := boot(), boot()
	for i := range first {
		if !bytes.Equal(first[i], second[i]) {
			t.Fatalf("boot key %d changed across a restart under fips140=only", i)
		}
	}

	if _, err := buildSecretStore(ctx, pool, "", "", nil); err == nil || !strings.Contains(err.Error(), "fips140=only") {
		t.Fatalf("a local-mode boot (ephemeral age key) under fips140=only = %v; want the refusal naming fips140=only", err)
	}
}
