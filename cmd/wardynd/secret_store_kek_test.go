// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WARDYN_KEK fails closed on every setting that cannot name a reachable key,
// and a Transit key over plain http:// to a remote host never boots.
func TestBuildKEK_FailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, addr, sel, key, want string }{
		{"unknown provider", "https://vault.example", "awskms", "wardyn", `WARDYN_KEK is "awskms"`},
		{"transit without a key", "https://vault.example", "transit", "", "needs WARDYN_VAULT_TRANSIT_KEY"},
		{"key without an address", "", "transit", "wardyn", "WARDYN_VAULT_ADDR is not"},
		{"plain http", "http://vault.example", "transit", "wardyn", "plain http://"},
		{"unreadable token file", "https://vault.example", "transit", "wardyn", "WARDYN_VAULT_TOKEN_FILE"},
		{"read-only key, no address", "", "local", "wardyn", "WARDYN_VAULT_ADDR is not"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := testExternalFlags(tc.addr, "")
			v.kek, v.transitMount, v.transitKey = strp(tc.sel), strp("transit"), strp(tc.key)
			k, _, err := buildKEK(t.Context(), v, "")
			if err == nil || k != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildKEK = (%v, %v); want a refusal containing %q", k, err, tc.want)
			}
		})
	}
	v, _ := testExternalFlags("", "")
	v.kek, v.transitMount, v.transitKey = strp("local"), strp("transit"), strp("")
	if k, writes, err := buildKEK(t.Context(), v, ""); k != nil || writes || err != nil {
		t.Fatalf("WARDYN_KEK=local with no Transit key = (%v, %v, %v); want no key service", k, writes, err)
	}
}

// TestRewrapMode_KeyServiceNeedsNoAgeKey: with WARDYN_KEK=transit, -rewrap
// needs no age key (every row may already be under the key service), so the
// age-key refusal gives way to the key service's own checks, which still run
// before the database is touched.
func TestRewrapMode_KeyServiceNeedsNoAgeKey(t *testing.T) {
	f := rekeyFlags("postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", "", "")
	*f.vault.kek = kekTransit
	if err := rewrapMode(f); err == nil || !strings.Contains(err.Error(), "needs WARDYN_VAULT_TRANSIT_KEY") {
		t.Fatalf("-rewrap with WARDYN_KEK=transit and no Transit key = %v; want the key service's refusal", err)
	}
}

// A platform Transit key needs WARDYN_KEK=transit and a platform role, and
// names nothing when unset.
func TestBuildPlatformKEK_FailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, sel, auth, role, rolePlatform, want string }{
		{"with WARDYN_KEK=local", "local", "kubernetes", "wardyn", "wardyn-platform", "WARDYN_VAULT_TRANSIT_KEY_PLATFORM needs WARDYN_KEK=transit"},
		{"with WARDYN_KEK unset", "", "kubernetes", "wardyn", "wardyn-platform", "WARDYN_VAULT_TRANSIT_KEY_PLATFORM needs WARDYN_KEK=transit"},
		{"without a platform role", "transit", "kubernetes", "wardyn", "", "WARDYN_VAULT_TRANSIT_KEY_PLATFORM needs WARDYN_VAULT_ROLE_PLATFORM"},
		{"with token-file auth", "transit", "token-file", "wardyn", "wardyn-platform", "WARDYN_VAULT_TRANSIT_KEY_PLATFORM needs WARDYN_VAULT_AUTH=kubernetes"},
		{"with one role for both", "transit", "kubernetes", "wardyn", "wardyn", "WARDYN_VAULT_ROLE_PLATFORM is the same role as WARDYN_VAULT_ROLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := testExternalFlags("https://vault.example", "")
			v.kek, v.transitMount, v.transitKeyPlatform, v.rolePlatform = strp(tc.sel), strp("transit"), strp("wardyn-platform"), strp(tc.rolePlatform)
			v.auth, v.role = strp(tc.auth), strp(tc.role)
			k, err := buildPlatformKEK(t.Context(), v, "", false)
			if err == nil || k != nil || !strings.Contains(err.Error(), "refusing to start: "+tc.want) {
				t.Fatalf("buildPlatformKEK = (%v, %v); want a refusal %q", k, err, tc.want)
			}
		})
	}
	v, _ := testExternalFlags("", "")
	v.kek, v.transitKeyPlatform = strp("local"), strp("")
	if k, err := buildPlatformKEK(t.Context(), v, "", false); k != nil || err != nil {
		t.Fatalf("no platform key = (%v, %v); want no key service", k, err)
	}
}

// The platform Transit key logs in as WARDYN_VAULT_ROLE_PLATFORM, never as the
// credential role: a token that reaches the credential key must not reach it.
func TestBuildPlatformKEK_LogsInAsThePlatformRole(t *testing.T) {
	var roles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		roles = append(roles, string(b))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	defer srv.Close()
	jwt := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(jwt, []byte("sa-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, _ := testExternalFlags(srv.URL, "")
	v.auth, v.k8sTokenFile, v.role, v.rolePlatform = strp("kubernetes"), strp(jwt), strp("wardyn-cred"), strp("wardyn-platform")
	v.kek, v.transitMount, v.transitKeyPlatform = strp("transit"), strp("transit"), strp("wardyn-platform")
	k, err := buildPlatformKEK(t.Context(), v, "", false)
	if err == nil || k != nil || !strings.Contains(err.Error(), `role "wardyn-platform"`) {
		t.Fatalf("buildPlatformKEK = (%v, %v); want a login refusal as the platform role", k, err)
	}
	if len(roles) == 0 {
		t.Fatal("the platform Transit client never logged in")
	}
	for _, body := range roles {
		if !strings.Contains(body, `"role":"wardyn-platform"`) || strings.Contains(body, "wardyn-cred") {
			t.Fatalf("login body %s; want the platform role only", body)
		}
	}
}

// Retiring reads the platform key whatever WARDYN_KEK is, so it is not refused
// for WARDYN_KEK=local; a start or a plain -rewrap still is.
func TestBuildPlatformKEK_RetireNeedsNoTransitKEK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	jwt := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(jwt, []byte("sa-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, _ := testExternalFlags(srv.URL, "")
	v.auth, v.k8sTokenFile, v.role, v.rolePlatform = strp("kubernetes"), strp(jwt), strp("wardyn-cred"), strp("wardyn-platform")
	v.kek, v.transitMount, v.transitKeyPlatform = strp("local"), strp("transit"), strp("wardyn-platform")
	if _, err := buildPlatformKEK(t.Context(), v, "", false); err == nil || !strings.Contains(err.Error(), "needs WARDYN_KEK=transit") {
		t.Fatalf("a start with WARDYN_KEK=local = %v; want the refusal", err)
	}
	if _, err := buildPlatformKEK(t.Context(), v, "", true); err == nil || !strings.Contains(err.Error(), `login (role "wardyn-platform")`) {
		t.Fatalf("retiring with WARDYN_KEK=local = %v; want it to reach the platform login", err)
	}
}
