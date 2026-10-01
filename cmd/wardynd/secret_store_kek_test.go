// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// WARDYN_KEK fails closed on every setting that cannot name a reachable key,
// and a Transit key over plain http:// to a remote host never boots.
func TestBuildKEK_FailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, addr, sel, key, want string }{
		{"unknown provider", "https://vault.example", "awskms", "wardyn", `WARDYN_KEK is "awskms"; want "local", "transit" or "azurekv"`},
		{"transit without a key", "https://vault.example", "transit", "", "needs WARDYN_VAULT_TRANSIT_KEY"},
		{"key without an address", "", "transit", "wardyn", "WARDYN_VAULT_ADDR is not"},
		{"plain http", "http://vault.example", "transit", "wardyn", "plain http://"},
		{"unreadable token file", "https://vault.example", "transit", "wardyn", "WARDYN_VAULT_TOKEN_FILE"},
		{"read-only key, no address", "", "local", "wardyn", "WARDYN_VAULT_ADDR is not"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, az := testExternalFlags(tc.addr, "")
			v.kek, v.transitMount, v.transitKey, v.transitKeyPlatform = strp(tc.sel), strp("transit"), strp(tc.key), strp("")
			k, _, err := buildKEK(t.Context(), v, az, "")
			if err == nil || k != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildKEK = (%v, %v); want a refusal containing %q", k, err, tc.want)
			}
		})
	}
	v, az := testExternalFlags("", "")
	v.kek, v.transitMount, v.transitKey, v.transitKeyPlatform = strp("local"), strp("transit"), strp(""), strp("")
	if k, writes, err := buildKEK(t.Context(), v, az, ""); k != nil || writes || err != nil {
		t.Fatalf("WARDYN_KEK=local with no Transit key = (%v, %v, %v); want no key service", k, writes, err)
	}
}

const (
	testAzureKEK = "https://kv.vault.azure.net/keys/wardyn-kek"
	testAzureSig = "https://kv.vault.azure.net/keys/wardyn-kek-sig"
)

// WARDYN_KEK=azurekv needs both Key Vault keys, one key service is
// configured at a time, and the provider list names azurekv.
func TestBuildKEK_AzureFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, sel, key, sig, transit, transitPlatform, want string }{
		{"with a Transit key", "azurekv", testAzureKEK, testAzureSig, "wardyn", "", "both name a key service; configure one"},
		{"with a platform Transit key", "local", testAzureKEK, testAzureSig, "", "wardyn-platform", "both name a key service; configure one"},
		{"with only the signing key and Transit", "transit", "", testAzureSig, "wardyn", "", "both name a key service; configure one"},
		{"without keys", "azurekv", "", "", "", "", "WARDYN_KEK=azurekv needs WARDYN_AZURE_KEK_KEY and WARDYN_AZURE_KEK_SIGNING_KEY"},
		{"without the signing key", "azurekv", testAzureKEK, "", "", "", "WARDYN_KEK=azurekv needs WARDYN_AZURE_KEK_KEY and WARDYN_AZURE_KEK_SIGNING_KEY"},
		{"one key, read-only", "local", testAzureKEK, "", "", "", "are set together or not at all"},
		{"unreadable federated token", "azurekv", testAzureKEK, testAzureSig, "", "", "WARDYN_AZURE_FEDERATED_TOKEN_FILE"},
		{"versioned key id", "azurekv", testAzureKEK + "/0123456789abcdef0123456789abcdef", testAzureSig, "", "", "versionless"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, az := testExternalFlags("https://vault.example", "")
			v.kek, v.transitMount, v.transitKey, v.transitKeyPlatform = strp(tc.sel), strp("transit"), strp(tc.transit), strp(tc.transitPlatform)
			az.kekKey, az.kekSigningKey = strp(tc.key), strp(tc.sig)
			k, _, err := buildKEK(t.Context(), v, az, "")
			if err == nil || k != nil || !strings.Contains(err.Error(), "refusing to start: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildKEK = (%v, %v); want a refusal containing %q", k, err, tc.want)
			}
		})
	}
}

// With WARDYN_KEK=local, named Key Vault keys are still built, read-only (the
// way back via -rewrap), so the boot reaches the vault and fails closed on it.
func TestBuildKEK_AzureReadOnlyWithLocal(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/token") {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("sa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, az := testExternalFlags("", "")
	v.kek, v.transitMount, v.transitKey, v.transitKeyPlatform = strp("local"), strp("transit"), strp(""), strp("")
	az.kekKey, az.kekSigningKey = strp(srv.URL+"/keys/wardyn-kek"), strp(srv.URL+"/keys/wardyn-kek-sig")
	az.federatedTokenFile, az.authorityHost = strp(token), strp(srv.URL)
	k, _, err := buildKEK(t.Context(), v, az, "")
	if err == nil || k != nil || !strings.Contains(err.Error(), "refusing to start: key vault KEK azurekv-key:") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("WARDYN_KEK=local with Key Vault keys = (%v, %v); want the Key Vault KEK built and refused by the vault", k, err)
	}
	if !slices.Contains(paths, "/keys/wardyn-kek") {
		t.Fatalf("the vault saw %v; want the wrapping key read", paths)
	}
}

// -rewrap with WARDYN_KEK=azurekv needs no age key, and refuses the Lane A
// combination in retire mode too: buildKEK runs before the platform key.
func TestRewrapMode_AzureKEK(t *testing.T) {
	f := rekeyFlags("postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", "", "")
	*f.vault.kek = kekAzure
	if err := rewrapMode(f); err == nil || !strings.Contains(err.Error(), "WARDYN_KEK=azurekv needs WARDYN_AZURE_KEK_KEY") {
		t.Fatalf("-rewrap with WARDYN_KEK=azurekv and no Key Vault keys = %v; want the key service's refusal", err)
	}
	*f.azure.kekKey, *f.azure.kekSigningKey, *f.vault.transitKeyPlatform = testAzureKEK, testAzureSig, "wardyn-platform"
	*f.rewrapRetirePlatformKey = true
	if err := rewrapMode(f); err == nil || !strings.Contains(err.Error(), "both name a key service; configure one") {
		t.Fatalf("-rewrap -rewrap-retire-platform-key with WARDYN_KEK=azurekv and a platform Transit key = %v; want the one-key-service refusal", err)
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
		{"with one key for both", "transit", "kubernetes", "wardyn", "wardyn-platform", "WARDYN_VAULT_TRANSIT_KEY_PLATFORM is the same key as WARDYN_VAULT_TRANSIT_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := testExternalFlags("https://vault.example", "")
			v.transitKey = strp("wardyn")
			if tc.name == "with one key for both" {
				v.transitKey = strp(" wardyn-platform ")
			}
			v.kek, v.transitMount, v.transitKeyPlatform, v.rolePlatform = strp(tc.sel), strp("transit"), strp("wardyn-platform"), strp(tc.rolePlatform)
			v.auth, v.role = strp(tc.auth), strp(tc.role)
			k, err := buildPlatformKEK(t.Context(), v, "", false)
			if err == nil || k != nil || !strings.Contains(err.Error(), "refusing to start: "+tc.want) {
				t.Fatalf("buildPlatformKEK = (%v, %v); want a refusal %q", k, err, tc.want)
			}
		})
	}
	v, _ := testExternalFlags("", "")
	v.kek, v.transitKey, v.transitKeyPlatform = strp("local"), strp(""), strp("")
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
	v.kek, v.transitMount, v.transitKey, v.transitKeyPlatform = strp("transit"), strp("transit"), strp("wardyn"), strp("wardyn-platform")
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
	v.kek, v.transitMount, v.transitKey, v.transitKeyPlatform = strp("local"), strp("transit"), strp("wardyn"), strp("wardyn-platform")
	// Retiring the credential key's twin is meaningless too.
	same := v
	same.transitKey = strp("wardyn-platform")
	if _, err := buildPlatformKEK(t.Context(), same, "", true); err == nil || !strings.Contains(err.Error(), "is the same key as WARDYN_VAULT_TRANSIT_KEY") {
		t.Fatalf("retiring with one key for both = %v; want the refusal", err)
	}
	if _, err := buildPlatformKEK(t.Context(), v, "", false); err == nil || !strings.Contains(err.Error(), "needs WARDYN_KEK=transit") {
		t.Fatalf("a start with WARDYN_KEK=local = %v; want the refusal", err)
	}
	if _, err := buildPlatformKEK(t.Context(), v, "", true); err == nil || !strings.Contains(err.Error(), `login (role "wardyn-platform")`) {
		t.Fatalf("retiring with WARDYN_KEK=local = %v; want it to reach the platform login", err)
	}
}
