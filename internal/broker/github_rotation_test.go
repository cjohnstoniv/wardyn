// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// genPEM generates a fresh RSA private key PEM, so two calls produce
// genuinely different key bytes (and therefore a different credential hash).
func genPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// TestGitHubMinter_CredentialRotationPickedUpWithoutRestart is the
// W12-W12-B-5 regression: on base 763beb5, githubMinter.client() caches the
// app-authenticated client after the FIRST mint and never looks at the
// secret store again, so rotating (or replacing) github-app-key never takes
// effect without a wardynd restart. The fixed client() re-reads both secrets
// every mint (cheap local Gets) and rebuilds only when their hash changed —
// this pins that a credential rotation between two mints actually rebuilds
// the cached client.
func TestGitHubMinter_CredentialRotationPickedUpWithoutRestart(t *testing.T) {
	ctx := context.Background()
	store := newMemSecrets()
	_ = store.Put(ctx, "github-app-id", []byte("111"))
	_ = store.Put(ctx, "github-app-key", genPEM(t))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id": 42}`))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"2099-01-01T00:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	gm, err := NewGitHubMinter(store, GitHubMinterConfig{AppIDSecret: "github-app-id", PrivateKeySecret: "github-app-key"})
	if err != nil {
		t.Fatalf("NewGitHubMinter: %v", err)
	}
	m := gm.(*githubMinter)
	m.baseURL = srv.URL + "/"

	if _, _, err := gm.MintInstallationToken(ctx, []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	m.mu.Lock()
	firstClient := m.appClient
	m.mu.Unlock()
	if firstClient == nil {
		t.Fatal("appClient must be cached after the first mint")
	}

	// Rotate: a brand new App id + private key, as an operator replacing
	// github-app-key (or re-running the setup wizard) would produce. No
	// restart — mint again through the SAME long-lived minter instance.
	_ = store.Put(ctx, "github-app-id", []byte("222"))
	_ = store.Put(ctx, "github-app-key", genPEM(t))

	if _, _, err := gm.MintInstallationToken(ctx, []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour); err != nil {
		t.Fatalf("mint after rotation: %v", err)
	}
	m.mu.Lock()
	secondClient := m.appClient
	m.mu.Unlock()

	if secondClient == firstClient {
		t.Fatal("rotating the App id/key must rebuild the cached go-github client on the next mint, not keep serving the pre-rotation client")
	}
}

// TestGitHubMinter_CredentialRotationInvalidatesInstallationCache is the
// bug-broker-1 regression: rotating the App credentials must clear
// installByOrg in the SAME step as the client() rebuild, not rely on the
// next mint's CreateInstallationToken 401/404 to self-heal. On base
// e2b3a91, the mint immediately after a rotation still uses the id cached
// under the pre-rotation App and fails outright (a non-401/404 error from
// the stale-under-the-new-App id, which isStaleInstallation does not treat
// as a self-heal signal) instead of re-resolving the installation up front.
func TestGitHubMinter_CredentialRotationInvalidatesInstallationCache(t *testing.T) {
	ctx := context.Background()
	store := newMemSecrets()
	_ = store.Put(ctx, "github-app-id", []byte("111"))
	_ = store.Put(ctx, "github-app-key", genPEM(t))

	var installLookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			n := installLookups.Add(1)
			if n == 1 {
				_, _ = w.Write([]byte(`{"id": 42}`)) // pre-rotation App's installation
			} else {
				_, _ = w.Write([]byte(`{"id": 77}`)) // post-rotation App's (different) installation
			}
		case strings.HasSuffix(r.URL.Path, "/installations/42/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_first","expires_at":"2099-01-01T00:00:00Z"}`))
		case strings.HasSuffix(r.URL.Path, "/installations/77/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_after_rotation","expires_at":"2099-01-01T00:00:00Z"}`))
		default:
			// A non-401/404 status: isStaleInstallation must NOT classify this
			// as a self-heal signal, so relying on the reactive drop alone
			// would leave the second mint failing.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		}
	}))
	defer srv.Close()

	gm, err := NewGitHubMinter(store, GitHubMinterConfig{AppIDSecret: "github-app-id", PrivateKeySecret: "github-app-key"})
	if err != nil {
		t.Fatalf("NewGitHubMinter: %v", err)
	}
	gm.(*githubMinter).baseURL = srv.URL + "/"

	if _, _, err := gm.MintInstallationToken(ctx, []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour); err != nil {
		t.Fatalf("first mint: %v", err)
	}

	// Rotate to a genuinely different App (different id + key): its
	// installation id for the same owner differs (77, not the cached 42).
	_ = store.Put(ctx, "github-app-id", []byte("222"))
	_ = store.Put(ctx, "github-app-key", genPEM(t))

	tok, _, err := gm.MintInstallationToken(ctx, []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour)
	if err != nil {
		t.Fatalf("mint right after rotation must succeed by re-resolving the installation, not reuse the stale cached id: %v", err)
	}
	if tok != "ghs_after_rotation" {
		t.Fatalf("token = %q, want ghs_after_rotation", tok)
	}
	if n := installLookups.Load(); n != 2 {
		t.Fatalf("expected a fresh installation lookup on the mint immediately after rotation, got %d total lookups", n)
	}
}

// TestGitHubMinter_StaleInstallationIDDroppedOn401 is the second half of
// W12-W12-B-5: a cached installation id can go stale even without a
// credential rotation (the App was uninstalled and reinstalled on the org),
// and GitHub answers 401/404 for a dead id. On base 763beb5 that stale id
// stays cached forever (installByOrg is never invalidated), so every
// subsequent mint repeats the same failure until a restart. The fix drops the
// entry on a 401/404 from CreateInstallationToken so the NEXT mint
// re-resolves it.
func TestGitHubMinter_StaleInstallationIDDroppedOn401(t *testing.T) {
	ctx := context.Background()
	store := newMemSecrets()
	_ = store.Put(ctx, "github-app-id", []byte("111"))
	_ = store.Put(ctx, "github-app-key", genPEM(t))

	var installLookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			n := installLookups.Add(1)
			if n == 1 {
				_, _ = w.Write([]byte(`{"id": 42}`)) // the stale id (reinstall changes it)
			} else {
				_, _ = w.Write([]byte(`{"id": 99}`)) // the id after re-resolving
			}
		case strings.HasSuffix(r.URL.Path, "/installations/42/access_tokens"):
			// The App was uninstalled/reinstalled: this id no longer resolves.
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		case strings.HasSuffix(r.URL.Path, "/installations/99/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_recovered","expires_at":"2099-01-01T00:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	gm, err := NewGitHubMinter(store, GitHubMinterConfig{AppIDSecret: "github-app-id", PrivateKeySecret: "github-app-key"})
	if err != nil {
		t.Fatalf("NewGitHubMinter: %v", err)
	}
	gm.(*githubMinter).baseURL = srv.URL + "/"

	// First mint resolves + caches the (now-stale) installation id 42, and
	// fails when GitHub 401s the token request against it.
	if _, _, err := gm.MintInstallationToken(ctx, []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour); err == nil {
		t.Fatal("first mint against the stale installation id must fail")
	}

	// A second mint must re-resolve the installation (not keep reusing the
	// dead cached id 42) and succeed against the new id 99.
	tok, _, err := gm.MintInstallationToken(ctx, []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour)
	if err != nil {
		t.Fatalf("second mint must recover by re-resolving the installation id, got: %v", err)
	}
	if tok != "ghs_recovered" {
		t.Fatalf("token = %q, want ghs_recovered", tok)
	}
	if n := installLookups.Load(); n != 2 {
		t.Fatalf("expected the installation lookup to run twice (cache dropped on 401 and re-resolved), got %d", n)
	}
}
