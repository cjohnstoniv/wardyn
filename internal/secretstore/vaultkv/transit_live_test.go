// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

// The live Transit acceptance case (credential-storage design §2.3, CS-2): a
// real Vault OSS or OpenBao server (WARDYN_TEST_VAULT, as live_test.go). The
// test mounts its own Transit engine, writes the documented least-privilege
// policy (update on encrypt/ and decrypt/ only) and runs as a child token
// holding just that — so it proves, on the real server, that associated_data
// binds a wrap to its row, that nothing beyond those two paths is needed, and
// that a key type which ignores associated_data is refused at boot.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

func TestLive_Transit(t *testing.T) {
	a, _, _ := liveSetup(t)
	mount := "wardyn-transit-" + uuid.NewString()[:8]
	a.must(http.MethodPost, "sys/mounts/"+mount, map[string]any{"type": "transit"})
	t.Cleanup(func() { a.do(http.MethodDelete, "sys/mounts/"+mount, nil) })
	a.must(http.MethodPost, mount+"/keys/wardyn", map[string]any{"type": "aes256-gcm96"})
	a.must(http.MethodPost, mount+"/keys/rsa", map[string]any{"type": "rsa-2048"})
	policy := fmt.Sprintf(`path "%[1]s/encrypt/wardyn" { capabilities = ["update"] }
path "%[1]s/decrypt/wardyn" { capabilities = ["update"] }
path "%[1]s/encrypt/rsa" { capabilities = ["update"] }
path "%[1]s/decrypt/rsa" { capabilities = ["update"] }
`, mount)
	a.must(http.MethodPut, "sys/policies/acl/"+mount, map[string]any{"policy": policy})
	t.Cleanup(func() { a.do(http.MethodDelete, "sys/policies/acl/"+mount, nil) })
	cfg := Config{Addr: a.addr, Auth: AuthTokenFile, TokenFile: writeFile(t, a.childToken(mount, "1h"))}
	ctx := t.Context()

	tr, err := NewTransit(ctx, cfg, mount, "wardyn")
	if err != nil {
		t.Fatalf("NewTransit against the live server: %v", err)
	}

	t.Run("associated_data_binds_the_wrap", func(t *testing.T) {
		dek := testDEK()
		w, err := tr.Wrap(ctx, dek, kek.Bind("alice@example.com", "pat"))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := tr.Unwrap(ctx, w, kek.Bind("alice@example.com", "pat")); err != nil || string(got) != string(dek) {
			t.Fatalf("Unwrap = (%x, %v)", got, err)
		}
		for _, b := range []map[string]string{kek.Bind("bob@example.com", "pat"), kek.Bind("alice@example.com", "other"), kek.Bind("", "pat")} {
			_, err := tr.Unwrap(ctx, w, b)
			if err == nil || errors.Is(err, secretstore.ErrUnavailable) {
				t.Fatalf("live Unwrap under %v = %v; want a definitive refusal (associated_data not enforced?)", b, err)
			}
		}
	})

	t.Run("a_key_that_ignores_associated_data_is_refused", func(t *testing.T) {
		_, err := NewTransit(ctx, cfg, mount, "rsa")
		if err == nil {
			t.Fatal("NewTransit accepted an rsa-2048 key, which does not bind associated_data")
		}
		t.Logf("refused as expected: %v", err)
	})

	t.Run("rotation_and_min_decryption_version", func(t *testing.T) {
		w1, err := tr.Wrap(ctx, testDEK(), kek.Bind("", "k"))
		if err != nil {
			t.Fatal(err)
		}
		a.must(http.MethodPost, mount+"/keys/wardyn/rotate", nil)
		n, err := tr.LatestVersion(ctx)
		if err != nil || n != 2 {
			t.Fatalf("LatestVersion after a rotation = (%d, %v), want 2", n, err)
		}
		if _, err := tr.Unwrap(ctx, w1, kek.Bind("", "k")); err != nil {
			t.Fatalf("a v1 wrap after the rotation: %v", err)
		}
		a.must(http.MethodPost, mount+"/keys/wardyn/config", map[string]any{"min_decryption_version": 2})
		_, err = tr.Unwrap(ctx, w1, kek.Bind("", "k"))
		if err == nil || errors.Is(err, secretstore.ErrUnavailable) {
			t.Fatalf("a v1 wrap with min_decryption_version=2 = %v; want a definitive refusal", err)
		}
		a.must(http.MethodPost, mount+"/keys/wardyn/config", map[string]any{"min_decryption_version": 1})
	})

	t.Run("revoked_token_is_definitive", func(t *testing.T) {
		tokPath := writeFile(t, a.childToken(mount, "1h"))
		tr2, err := NewTransit(ctx, Config{Addr: a.addr, Auth: AuthTokenFile, TokenFile: tokPath}, mount, "wardyn")
		if err != nil {
			t.Fatal(err)
		}
		w, _ := tr2.Wrap(ctx, testDEK(), kek.Bind("", "k"))
		tok, _ := os.ReadFile(tokPath)
		a.must(http.MethodPost, "auth/token/revoke", map[string]string{"token": strings.TrimSpace(string(tok))})
		if _, err := tr2.Unwrap(ctx, w, kek.Bind("", "k")); err == nil || errors.Is(err, secretstore.ErrUnavailable) {
			t.Fatalf("Unwrap with the token revoked = %v; want a definitive refusal", err)
		}
	})

	t.Run("conformance_through_pg", func(t *testing.T) {
		if os.Getenv("WARDYN_TEST_PG") == "" {
			t.Skip("WARDYN_TEST_PG not set")
		}
		pool := throwawayDB(t)
		secretstoretest.RunConformance(t, func(t *testing.T) secretstore.Store { return pgStore(t, pool, nil, tr, true) })
	})
}
