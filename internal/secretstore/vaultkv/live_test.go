// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

// The live acceptance case (credential-storage design §2.3a.10): a real Vault
// OSS or OpenBao server. WARDYN_TEST_VAULT is its address and
// WARDYN_TEST_VAULT_TOKEN_FILE a file holding a token that may mount engines,
// write policies and create tokens (a dev server's root token). The test
// mounts its own KV v2 engine, writes the least-privilege policy the docs
// give (no destroy/ or undelete/ stanza), and runs everything as a child
// token holding only that policy — so a capability word the docs got wrong
// fails here. With WARDYN_TEST_PG set it also runs the conformance suite
// through the pg store in store mode.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

type liveAdmin struct {
	t           *testing.T
	addr, token string
}

func (a liveAdmin) do(method, path string, in any) (int, map[string]any) {
	a.t.Helper()
	var body *bytes.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, a.addr+"/v1/"+path, body)
	req.Header.Set("X-Vault-Token", a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("admin %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (a liveAdmin) must(method, path string, in any) map[string]any {
	a.t.Helper()
	status, out := a.do(method, path, in)
	if status/100 != 2 {
		a.t.Fatalf("admin %s %s: %d %v", method, path, status, out)
	}
	return out
}

// childToken creates a token holding only policy, with the given TTL.
func (a liveAdmin) childToken(policy, ttl string) string {
	out := a.must(http.MethodPost, "auth/token/create", map[string]any{"policies": []string{policy}, "ttl": ttl, "renewable": true})
	return out["auth"].(map[string]any)["client_token"].(string)
}

// livePolicy is the documented least-privilege policy (docs/OPERATIONS.md),
// with the prefix spelled out instead of templated on the Kubernetes alias.
func livePolicy(mount, prefix string) string {
	return fmt.Sprintf(`path "%[1]s/data/%[2]s/*" { capabilities = ["create", "update", "read", "delete"] }
path "%[1]s/metadata/%[2]s/*" { capabilities = ["create", "update", "read", "delete", "list"] }
`, mount, prefix)
}

func liveSetup(t *testing.T) (liveAdmin, string, string) {
	addr := os.Getenv("WARDYN_TEST_VAULT")
	tokFile := os.Getenv("WARDYN_TEST_VAULT_TOKEN_FILE")
	if addr == "" || tokFile == "" {
		t.Skip("WARDYN_TEST_VAULT / WARDYN_TEST_VAULT_TOKEN_FILE not set; skipping the live Vault case")
	}
	b, err := os.ReadFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	a := liveAdmin{t: t, addr: strings.TrimRight(addr, "/"), token: strings.TrimSpace(string(b))}
	mount := "wardyn-live-" + uuid.NewString()[:8]
	a.must(http.MethodPost, "sys/mounts/"+mount, map[string]any{"type": "kv", "options": map[string]string{"version": "2"}})
	t.Cleanup(func() { a.do(http.MethodDelete, "sys/mounts/"+mount, nil) })
	a.must(http.MethodPut, "sys/policies/acl/"+mount, map[string]any{"policy": livePolicy(mount, "ns-live")})
	t.Cleanup(func() { a.do(http.MethodDelete, "sys/policies/acl/"+mount, nil) })
	return a, mount, "ns-live"
}

func liveStore(t *testing.T, a liveAdmin, mount, prefix, ttl string) (*Store, string) {
	t.Helper()
	path := writeFile(t, a.childToken(mount, ttl))
	s, err := New(t.Context(), Config{Addr: a.addr, Auth: AuthTokenFile, TokenFile: path, Mount: mount, Prefix: prefix, MaxVersions: 1})
	if err != nil {
		t.Fatalf("New against the live server: %v", err)
	}
	return s, path
}

func TestLive_VaultKV(t *testing.T) {
	a, mount, prefix := liveSetup(t)
	s, _ := liveStore(t, a, mount, prefix, "1h")
	ctx := t.Context()

	t.Run("roundtrip_binary_and_binding", func(t *testing.T) {
		ref, err := s.Put(ctx, "alice@example.com", "pat", "", []byte("a\x00\xffb"), false)
		if err != nil {
			t.Fatal(err)
		}
		if v, err := s.Get(ctx, "alice@example.com", "pat", ref); err != nil || string(v) != "a\x00\xffb" {
			t.Fatalf("Get = (%q, %v)", v, err)
		}
		if err := s.Check(ctx, "alice@example.com", "pat", ref); err != nil {
			t.Fatalf("Check: %v", err)
		}
		rel, _ := s.rel("alice@example.com", "pat")
		a.must(http.MethodPost, mount+"/metadata/"+rel, map[string]any{"custom_metadata": map[string]string{metaOwner: "mallory", metaName: "pat"}})
		if _, err := s.Get(ctx, "alice@example.com", "pat", ref); err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("Get with the owner metadata changed at Vault = %v; want a refusal", err)
		}
	})

	t.Run("replace_keeps_one_version_and_delete_removes_all", func(t *testing.T) {
		ref, err := s.Put(ctx, "", "github-app-key", "", []byte("v1"), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(ctx, "", "github-app-key", ref, []byte("v2"), false); err != nil {
			t.Fatalf("replace: %v", err)
		}
		rel, _ := s.rel("", "github-app-key")
		meta := a.must(http.MethodGet, mount+"/metadata/"+rel, nil)["data"].(map[string]any)
		if n := len(meta["versions"].(map[string]any)); n != 1 {
			t.Fatalf("versions kept after a replace = %d, want 1", n)
		}
		if _, err := s.Put(ctx, "", "github-app-key", "", []byte("v3"), true); err == nil {
			t.Fatal("createOnly overwrote a live value")
		}
		if err := s.Delete(ctx, "", "github-app-key", ref); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if status, _ := a.do(http.MethodGet, mount+"/metadata/"+rel, nil); status != http.StatusNotFound {
			t.Fatalf("metadata after Delete: status %d, want 404 (every version gone)", status)
		}
		if _, err := s.Get(ctx, "", "github-app-key", ref); err == nil || errors.Is(err, secretstore.ErrNotFound) {
			t.Fatalf("Get after Delete = %v; want a refusal that is not not-found", err)
		}
		if err := s.Delete(ctx, "", "github-app-key", ref); err != nil {
			t.Fatalf("second Delete must be idempotent: %v", err)
		}
	})

	t.Run("walk_lists_every_kind", func(t *testing.T) {
		for _, r := range [][2]string{{"", "wardyn-signing-key"}, {"", "operator-key"}, {"bob", "nested/name"}} {
			if _, err := s.Put(ctx, r[0], r[1], "", []byte("v"), false); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Walk(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, e := range got {
			seen[e.Owner+"|"+e.Name] = true
		}
		for _, want := range []string{"|wardyn-signing-key", "|operator-key", "bob|nested/name"} {
			if !seen[want] {
				t.Errorf("Walk missed %q (got %v)", want, got)
			}
		}
	})

	t.Run("revoked_token_is_definitive", func(t *testing.T) {
		s2, path := liveStore(t, a, mount, prefix, "1h")
		ref, err := s2.Put(ctx, "", "revoke-check", "", []byte("v"), false)
		if err != nil {
			t.Fatal(err)
		}
		tok, _ := os.ReadFile(path)
		a.must(http.MethodPost, "auth/token/revoke", map[string]string{"token": strings.TrimSpace(string(tok))})
		_, err = s2.Get(ctx, "", "revoke-check", ref)
		if err == nil || errors.Is(err, secretstore.ErrUnavailable) {
			t.Fatalf("Get with the token revoked = %v; want a definitive refusal", err)
		}
	})

	t.Run("short_token_is_renewed", func(t *testing.T) {
		s3, _ := liveStore(t, a, mount, prefix, "4s")
		time.Sleep(7 * time.Second)
		if _, err := s3.Put(ctx, "", "renew-check", "", []byte("v"), false); err != nil {
			t.Fatalf("Put 7s into a 4s token: %v (renew-self did not keep it alive)", err)
		}
	})

	t.Run("conformance_through_pg", func(t *testing.T) {
		if os.Getenv("WARDYN_TEST_PG") == "" {
			t.Skip("WARDYN_TEST_PG not set")
		}
		pool := throwawayDB(t)
		secretstoretest.RunConformance(t, func(t *testing.T) secretstore.Store { return storeMode(t, pool, s, nil) })
	})
}

// TestLive_KubernetesAuth logs in with a real projected service-account token
// (WARDYN_TEST_VAULT_K8S_JWT_FILE, audience "vault") against a server whose
// Kubernetes auth is configured as docs/OPERATIONS.md gives it: role "wardyn"
// on mount "kubernetes", bound to the token's service account, and the KV
// mount "wardyn" under the policy templated on the service account's
// namespace. The prefix is that namespace, so the second half proves the
// template confines an install to its own namespace's paths.
func TestLive_KubernetesAuth(t *testing.T) {
	addr, jwtFile := os.Getenv("WARDYN_TEST_VAULT"), os.Getenv("WARDYN_TEST_VAULT_K8S_JWT_FILE")
	if addr == "" || jwtFile == "" {
		t.Skip("WARDYN_TEST_VAULT / WARDYN_TEST_VAULT_K8S_JWT_FILE not set; skipping the live Kubernetes-auth case")
	}
	raw, err := os.ReadFile(jwtFile)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), ".")
	if len(parts) != 3 {
		t.Fatal("WARDYN_TEST_VAULT_K8S_JWT_FILE does not hold a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		K8s struct {
			Namespace string `json:"namespace"`
		} `json:"kubernetes.io"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.K8s.Namespace == "" {
		t.Fatalf("the JWT names no service-account namespace: %v", err)
	}
	open := func(prefix string) *Store {
		s, err := New(t.Context(), Config{Addr: addr, Auth: AuthKubernetes, AuthMount: "kubernetes", Role: "wardyn",
			K8sTokenFile: jwtFile, Mount: "wardyn", Prefix: prefix, MaxVersions: 1})
		if err != nil {
			t.Fatalf("kubernetes login: %v", err)
		}
		return s
	}
	s := open(claims.K8s.Namespace)
	ref, err := s.Put(t.Context(), "alice", "pat", "", []byte("v"), false)
	if err != nil {
		t.Fatalf("Put under the service account's namespace: %v", err)
	}
	if v, err := s.Get(t.Context(), "alice", "pat", ref); err != nil || string(v) != "v" {
		t.Fatalf("Get = (%q, %v)", v, err)
	}
	if err := s.Delete(t.Context(), "alice", "pat", ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	other := open("another-namespace")
	_, err = other.Put(t.Context(), "alice", "pat", "", []byte("v"), false)
	if err == nil || errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Put under another namespace's prefix = %v; want a definitive 403 from the templated policy", err)
	}
}
