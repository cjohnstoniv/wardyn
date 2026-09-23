// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

func TestNew_RefusesPlainHTTPOffLoopback(t *testing.T) {
	for addr, want := range map[string]string{
		"http://vault.internal:8200": "plain http://", "http://10.0.0.5:8200": "plain http://",
		"ftp://vault:8200": "not an http(s) URL", "vault:8200": "not an http(s) URL",
	} {
		_, err := newClient(Config{Addr: addr})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("newClient(%q) = %v; want a refusal saying %q", addr, err, want)
		}
	}
	for _, addr := range []string{"http://127.0.0.1:8200", "http://localhost:8200", "http://[::1]:8200", "https://vault.internal:8200"} {
		if _, err := newClient(Config{Addr: addr}); err != nil {
			t.Errorf("newClient(%q) refused: %v", addr, err)
		}
	}
}

func TestNew_FailsClosedWhenLoginFails(t *testing.T) {
	f := newFakeVault(t)
	_, err := New(t.Context(), Config{Addr: f.srv.URL, Auth: AuthKubernetes, AuthMount: "kubernetes", Role: "wardyn",
		K8sTokenFile: writeFile(t, "not-a-bound-sa"), Mount: "wardyn", Prefix: "p", MaxVersions: 1})
	if err == nil || !strings.Contains(err.Error(), "kubernetes login") {
		t.Fatalf("New with a refused login = %v; want a boot refusal naming the login", err)
	}
}

func TestKubernetesLogin_SendsRoleJWTAndNamespace(t *testing.T) {
	f := newFakeVault(t)
	f.namespace = "team-a"
	s := newFakeStore(t, f)
	if _, err := s.Put(t.Context(), "", "k", "", []byte("v"), false); err != nil {
		t.Fatalf("Put under a namespace: %v", err)
	}
	if f.logins != 1 {
		t.Fatalf("logins = %d, want 1", f.logins)
	}
}

// A token file a Vault Agent rotated: the old token is revoked, the next call
// meets a 403, re-reads the file and succeeds.
func TestTokenFile_RereadOn403(t *testing.T) {
	f := newFakeVault(t)
	f.mu.Lock()
	old := f.issue()
	f.mu.Unlock()
	path := writeFile(t, old)
	s, err := New(t.Context(), Config{Addr: f.srv.URL, Auth: AuthTokenFile, TokenFile: path, Mount: "wardyn", Prefix: "p", MaxVersions: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.mu.Lock()
	delete(f.tokens, old)
	fresh := f.issue()
	f.mu.Unlock()
	if err := os.WriteFile(path, []byte(fresh+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), "", "k", "", []byte("v"), false); err != nil {
		t.Fatalf("Put after the token file rotated: %v", err)
	}
}

func TestKeepAlive_RenewsBeforeExpiry(t *testing.T) {
	f := newFakeVault(t)
	f.ttl = 1
	newFakeStore(t, f)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := f.renews
		f.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("renew-self was never called for a 1s token")
}

// The transient/definitive split (design rule 21): 429, 5xx, sealed and the
// network are transient after the retries; 403 is definitive at once.
func TestClassification(t *testing.T) {
	cases := []struct {
		name      string
		force     []int
		transient bool
	}{
		{"sealed", []int{503, 503, 503, 503}, true},
		{"throttled", []int{429, 429, 429, 429}, true},
		{"internal", []int{500, 500, 500, 500}, true},
		{"forbidden", []int{403, 403}, false},
		{"bad-request", []int{400}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVault(t)
			s := newFakeStore(t, f)
			ref, err := s.Put(t.Context(), "", "k", "", []byte("v"), false)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.force = tc.force
			f.mu.Unlock()
			_, err = s.Get(t.Context(), "", "k", ref)
			if err == nil {
				t.Fatal("Get succeeded through a forced failure")
			}
			if got := errors.Is(err, secretstore.ErrUnavailable); got != tc.transient {
				t.Fatalf("errors.Is(ErrUnavailable) = %v, want %v (err %v)", got, tc.transient, err)
			}
			if errors.Is(err, secretstore.ErrNotFound) {
				t.Fatalf("a store failure reads as not-found: %v", err)
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		f := newFakeVault(t)
		s := newFakeStore(t, f)
		f.srv.Close()
		_, err := s.Get(context.Background(), "", "k", "wardyn/ns1/operator/k")
		if !errors.Is(err, secretstore.ErrUnavailable) {
			t.Fatalf("Get with Vault down = %v, want ErrUnavailable", err)
		}
	})
	t.Run("retried-then-ok", func(t *testing.T) {
		f := newFakeVault(t)
		s := newFakeStore(t, f)
		ref, _ := s.Put(t.Context(), "", "k", "", []byte("v"), false)
		f.mu.Lock()
		f.force = []int{503, 503, 503}
		f.mu.Unlock()
		if v, err := s.Get(t.Context(), "", "k", ref); err != nil || string(v) != "v" {
			t.Fatalf("Get after three 503s = (%q, %v), want the value on the fourth attempt", v, err)
		}
	})
}

func TestPathScheme(t *testing.T) {
	s := &Store{mount: "wardyn", prefix: "ns1"}
	b := func(owner string) string { return strings.ToLower(b32.EncodeToString([]byte(owner))) }
	cases := map[[2]string]string{
		{"", "wardyn-signing-key"}:   "ns1/platform/wardyn-signing-key",
		{"", "github-app-key"}:       "ns1/operator/github-app-key",
		{"alice@example.com", "pat"}: "ns1/people/" + b("alice@example.com") + "/pat",
		{"a/b", "k"}:                 "ns1/people/" + b("a/b") + "/k",
	}
	if b("f") != "co" {
		t.Fatalf("owner segment is not lowercase unpadded base32hex: %q", b("f"))
	}
	for in, want := range cases {
		got, err := s.rel(in[0], in[1])
		if err != nil || got != want {
			t.Errorf("rel(%q, %q) = (%q, %v), want %q", in[0], in[1], got, err, want)
		}
		if strings.Count(got, "/") != strings.Count(want, "/") {
			t.Errorf("owner %q leaked a path separator: %q", in[0], got)
		}
	}
	for _, bad := range []string{"", "..", "a/b", "a/../b", "a//b", "a b", "k%2f"} {
		if _, err := s.rel("", bad); err == nil {
			t.Errorf("rel accepted name %q", bad)
		}
	}
}

// Rule 16, first half: the row's recorded ref never selects the path.
func TestGet_RefusesARefThatIsNotDerived(t *testing.T) {
	f := newFakeVault(t)
	s := newFakeStore(t, f)
	bobRef, err := s.Put(t.Context(), "bob", "pat", "", []byte("bob-secret"), false)
	if err != nil {
		t.Fatal(err)
	}
	// Alice's row, pointed at Bob's value by a database writer.
	v, err := s.Get(t.Context(), "alice", "pat", bobRef)
	if err == nil || !strings.Contains(err.Error(), "forged") {
		t.Fatalf("Get(alice, pat, bob's ref) = (%q, %v); want a refusal", v, err)
	}
	if f.callCount("GET wardyn/data/") != 0 {
		t.Fatal("a forged ref reached Vault; the derived path must be checked first")
	}
}

// Rule 16, second half: the value's own metadata must name the row, key by
// key. A value copied between two of one person's names keeps the owner and
// changes only the name; a wardyn-format this wardynd does not write is a
// value it does not know how to read.
func TestGet_RefusesMetadataThatNamesAnotherRow(t *testing.T) {
	for key, other := range map[string]string{
		metaOwner: "mallory", metaName: "other-pat", metaKind: "platform", metaFormat: "v3",
	} {
		t.Run(key, func(t *testing.T) {
			f := newFakeVault(t)
			s := newFakeStore(t, f)
			ref, err := s.Put(t.Context(), "alice", "pat", "", []byte("v"), false)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := s.rel("alice", "pat")
			f.mu.Lock()
			f.kv[rel].custom[key] = other
			f.mu.Unlock()
			if v, err := s.Get(t.Context(), "alice", "pat", ref); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Get with %s=%q = (%q, %v); want a refusal naming %s", key, other, v, err, key)
			}
			if err := s.Check(t.Context(), "alice", "pat", ref); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Check with %s=%q = %v; want a refusal naming %s", key, other, err, key)
			}
		})
	}
}

// Rule 17 for a boot key: data at the path in any other shape than Wardyn's
// (no "value" key) is a refusal, never zero bytes a caller could mint over.
func TestGet_RefusesADataMapWithNoValue(t *testing.T) {
	f := newFakeVault(t)
	s := newFakeStore(t, f)
	ref, err := s.Put(t.Context(), "", "wardyn-signing-key", "", []byte("key"), false)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := s.rel("", "wardyn-signing-key")
	f.mu.Lock()
	f.kv[rel].versions[f.kv[rel].current] = map[string]string{"password": "x"}
	f.mu.Unlock()
	v, err := s.Get(t.Context(), "", "wardyn-signing-key", ref)
	if err == nil || errors.Is(err, secretstore.ErrNotFound) || errors.Is(err, secretstore.ErrUnavailable) ||
		!strings.Contains(err.Error(), "not in Wardyn's format") {
		t.Fatalf("Get of a data map with no value = (%q, %v); want a definitive refusal", v, err)
	}
}

// A Put never writes over a path whose value is bound to another row (a
// tampered or orphaned value): it refuses, naming the path.
func TestPut_RefusesAPathBoundToAnotherRow(t *testing.T) {
	f := newFakeVault(t)
	s := newFakeStore(t, f)
	ref, err := s.Put(t.Context(), "alice", "pat", "", []byte("v1"), false)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := s.rel("alice", "pat")
	f.mu.Lock()
	f.kv[rel].custom[metaOwner] = "mallory"
	f.mu.Unlock()
	if _, err := s.Put(t.Context(), "alice", "pat", ref, []byte("v2"), false); err == nil || !strings.Contains(err.Error(), s.ref(rel)) {
		t.Fatalf("Put over a value bound to another owner = %v; want a refusal naming %s", err, s.ref(rel))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.kv[rel]
	if got := e.versions[e.current]["value"]; got != base64.StdEncoding.EncodeToString([]byte("v1")) || e.custom[metaOwner] != "mallory" {
		t.Fatalf("the refused Put changed the path: value %q, owner metadata %q", got, e.custom[metaOwner])
	}
}

// Rule 17 at the client: an absent value is a refusal, never not-found.
func TestGet_AbsentValueIsRefusalNotNotFound(t *testing.T) {
	f := newFakeVault(t)
	s := newFakeStore(t, f)
	ref, _ := s.Put(t.Context(), "", "k", "", []byte("v"), false)
	if err := s.Delete(t.Context(), "", "k", ref); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(t.Context(), "", "k", ref)
	if err == nil || errors.Is(err, secretstore.ErrNotFound) || errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Get of a deleted value = %v; want a definitive refusal", err)
	}
}

// A replace writes with check-and-set, so a concurrent writer is refused
// rather than silently overwritten, and createOnly never overwrites.
func TestPut_CheckAndSetAndCreateOnly(t *testing.T) {
	f := newFakeVault(t)
	f.casRequired = true
	s := newFakeStore(t, f)
	ref, err := s.Put(t.Context(), "", "k", "", []byte("v1"), false)
	if err != nil {
		t.Fatalf("first Put on a cas_required mount: %v", err)
	}
	if _, err := s.Put(t.Context(), "", "k", ref, []byte("v2"), false); err != nil {
		t.Fatalf("replace on a cas_required mount: %v", err)
	}
	if _, err := s.Put(t.Context(), "", "k", "", []byte("v3"), true); err == nil {
		t.Fatal("createOnly overwrote an existing value")
	}
	if v, _ := s.Get(t.Context(), "", "k", ref); string(v) != "v2" {
		t.Fatalf("value = %q, want v2", v)
	}
	rel, _ := s.rel("", "k")
	f.mu.Lock()
	if n := len(f.kv[rel].versions); n != 1 {
		f.mu.Unlock()
		t.Fatalf("versions kept = %d, want 1 (max_versions=1: a replaced value does not linger)", n)
	}
	f.mu.Unlock()
	// Soft-deleted at Vault, the path holds no live value, so a create lands.
	if _, err := s.c.call(t.Context(), http.MethodDelete, s.mount+"/data/"+rel, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(t.Context(), "", "k", ref); err == nil {
		t.Fatal("Check passed a soft-deleted current version")
	}
	if _, err := s.Put(t.Context(), "", "k", "", []byte("v4"), true); err != nil {
		t.Fatalf("createOnly after a soft delete: %v", err)
	}
	if v, err := s.Get(t.Context(), "", "k", ref); err != nil || string(v) != "v4" {
		t.Fatalf("Get after the re-create = (%q, %v), want v4", v, err)
	}
}

// A revoked policy answers 403 to every call. Each answer stays definitive,
// and the logins it triggers are bounded: one per reloginEvery, not one per
// call (each is a TokenReview and a line in both audit logs).
func TestRevokedPolicy_ReloginIsBounded(t *testing.T) {
	f := newFakeVault(t)
	s := newFakeStore(t, f)
	ref, err := s.Put(t.Context(), "", "k", "", []byte("v"), false)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.revoked = true
	f.mu.Unlock()
	for range 3 {
		_, err := s.Get(t.Context(), "", "k", ref)
		if err == nil || errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), "403") {
			t.Fatalf("Get under a revoked policy = %v; want a definitive 403", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logins != 2 {
		t.Fatalf("logins = %d; want 2 (the boot login and one re-login for three denied reads)", f.logins)
	}
}

func TestWalk_ListsEveryKindAndDecodesOwners(t *testing.T) {
	f := newFakeVault(t)
	s := newFakeStore(t, f)
	for _, row := range [][2]string{{"", "wardyn-signing-key"}, {"", "github-app-key"}, {"alice@example.com", "pat"}} {
		if _, err := s.Put(t.Context(), row[0], row[1], "", []byte("v"), false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Walk(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"|wardyn-signing-key": true, "|github-app-key": true, "alice@example.com|pat": true}
	for _, e := range got {
		if !want[e.Owner+"|"+e.Name] {
			t.Errorf("unexpected entry %+v", e)
		}
		delete(want, e.Owner+"|"+e.Name)
	}
	if len(want) != 0 {
		t.Fatalf("Walk missed %v", want)
	}
}

func TestDelete_UsesTheDerivedPathNeverTheRef(t *testing.T) {
	f := newFakeVault(t)
	s := newFakeStore(t, f)
	bobRef, _ := s.Put(t.Context(), "bob", "pat", "", []byte("bob-secret"), false)
	if err := s.Delete(t.Context(), "alice", "pat", bobRef); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(t.Context(), "bob", "pat", bobRef); err != nil || string(v) != "bob-secret" {
		t.Fatalf("a delete of alice's row with bob's ref removed bob's value: (%q, %v)", v, err)
	}
}

// TestTwoRoles_PlatformPathsUseOnlyThePlatformToken is the recommended
// configuration (design §2.13 b) against a Vault enforcing the documented
// two-role policy: every boot-key call — write, read, check, delete, walk —
// goes out with the platform role's token and every credential call with the
// other, so neither token ever needs (or is shown to) the other's paths.
func TestTwoRoles_PlatformPathsUseOnlyThePlatformToken(t *testing.T) {
	f := newFakeVault(t)
	f.jwts["sa-jwt"] = "wardyn-credentials"
	f.platformRole = "wardyn-platform"
	s, err := New(t.Context(), Config{
		Addr: f.srv.URL, Auth: AuthKubernetes, AuthMount: "kubernetes", Role: "wardyn-credentials", RolePlatform: "wardyn-platform",
		K8sTokenFile: writeFile(t, "sa-jwt\n"), Mount: f.mount, Prefix: "ns1", MaxVersions: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.c.backoff, s.platform.backoff = 0, 0
	ctx := t.Context()
	rows := []struct{ owner, name string }{{"", "wardyn-signing-key"}, {"", "github-app-key"}, {"alice", "anthropic-api-key"}}
	for _, r := range rows {
		ref, err := s.Put(ctx, r.owner, r.name, "", []byte("v-"+r.name), false)
		if err != nil {
			t.Fatalf("Put %s/%s: %v", r.owner, r.name, err)
		}
		if got, err := s.Get(ctx, r.owner, r.name, ref); err != nil || string(got) != "v-"+r.name {
			t.Fatalf("Get %s/%s = (%q, %v)", r.owner, r.name, got, err)
		}
		if err := s.Check(ctx, r.owner, r.name, ref); err != nil {
			t.Fatalf("Check %s/%s: %v", r.owner, r.name, err)
		}
	}
	entries, err := s.Walk(ctx)
	if err != nil || len(entries) != len(rows) {
		t.Fatalf("Walk = (%v, %v), want all %d values", entries, err, len(rows))
	}
	for _, r := range rows {
		if err := s.Delete(ctx, r.owner, r.name, ""); err != nil {
			t.Fatalf("Delete %s/%s: %v", r.owner, r.name, err)
		}
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want one per role", f.logins)
	}
}

func TestTwoRoles_RefusedWhereTheySeparateNothing(t *testing.T) {
	for label, cfg := range map[string]Config{
		"token-file auth": {Auth: AuthTokenFile, TokenFile: "/dev/null", RolePlatform: "wardyn-platform"},
		"the same role":   {Auth: AuthKubernetes, Role: "wardyn", RolePlatform: "wardyn"},
	} {
		cfg.Addr, cfg.Mount, cfg.Prefix, cfg.MaxVersions = "https://vault.example:8200", "wardyn", "ns1", 1
		if _, err := New(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "WARDYN_VAULT_ROLE_PLATFORM") {
			t.Errorf("%s: New = %v, want a refusal naming WARDYN_VAULT_ROLE_PLATFORM", label, err)
		}
	}
}
