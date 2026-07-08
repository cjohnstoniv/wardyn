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
	"testing"
	"time"
)

// The lazy GitHub minter must read its App credentials at MINT time, not at
// construction: a mint succeeds when the two secrets were ABSENT at boot but
// PRESENT at mint time (proving no wardynd restart is needed after adding them).
// It must still fail closed with a clear error when the secrets are absent at
// mint time.
func TestGitHubMinter_LazyReadsSecretsAtMintTime(t *testing.T) {
	ctx := context.Background()
	store := newMemSecrets() // App secrets ABSENT at boot

	gm, err := NewGitHubMinter(store, GitHubMinterConfig{AppIDSecret: "github-app-id", PrivateKeySecret: "github-app-key"})
	if err != nil {
		t.Fatalf("NewGitHubMinter (lazy) must succeed with secrets absent: %v", err)
	}

	// Mint with secrets STILL absent => fail closed, naming the missing secret.
	_, _, err = gm.MintInstallationToken(ctx, []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour)
	if err == nil {
		t.Fatal("mint with absent App secrets must fail closed")
	}
	if !strings.Contains(err.Error(), "read github app id secret") {
		t.Fatalf("absent-secret mint error = %v; want it to name the missing app id secret", err)
	}

	// Add the App credentials AFTER boot (no restart).
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	_ = store.Put(ctx, "github-app-id", []byte("12345"))
	_ = store.Put(ctx, "github-app-key", pemBytes)

	// Point the go-github client at a fake API so the mint completes offline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id": 42}`))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_lazy_ok","expires_at":"2099-01-01T00:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	gm.(*githubMinter).baseURL = srv.URL + "/"

	tok, exp, err := gm.MintInstallationToken(ctx, []string{"acme/widgets"}, map[string]string{"contents": "read"}, time.Hour)
	if err != nil {
		t.Fatalf("mint after adding secrets (no restart) must succeed: %v", err)
	}
	if tok != "ghs_lazy_ok" {
		t.Fatalf("token = %q, want ghs_lazy_ok", tok)
	}
	if exp.IsZero() {
		t.Fatal("expiry must be set from the GitHub response")
	}
}
