// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"context"
	"errors"
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
	for _, bad := range []string{"", "..", "a/../b", "a//b", "a b", "k%2f"} {
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

// Rule 16, second half: the value's own metadata must name the row.
func TestGet_RefusesMetadataThatNamesAnotherRow(t *testing.T) {
	f := newFakeVault(t)
	s := newFakeStore(t, f)
	ref, err := s.Put(t.Context(), "alice", "pat", "", []byte("v"), false)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := s.rel("alice", "pat")
	f.mu.Lock()
	f.kv[rel].custom[metaOwner] = "mallory"
	f.mu.Unlock()
	if v, err := s.Get(t.Context(), "alice", "pat", ref); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("Get with mismatched custom_metadata = (%q, %v); want a refusal", v, err)
	}
	if err := s.Check(t.Context(), "alice", "pat", ref); err == nil {
		t.Fatal("Check passed a value whose metadata names another owner")
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
	defer f.mu.Unlock()
	if n := len(f.kv[rel].versions); n != 1 {
		t.Fatalf("versions kept = %d, want 1 (max_versions=1: a replaced value does not linger)", n)
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
