// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The origin check accepts a link that spells out the default port and refuses
// every other origin.
func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		name, base, link string
		want             bool
	}{
		{"https no port vs :443", "https://v.vault.azure.net", "https://v.vault.azure.net:443/secrets?x=1", true},
		{"https :443 vs no port", "https://v.vault.azure.net:443", "https://v.vault.azure.net/secrets", true},
		{"http no port vs :80", "http://localhost", "http://localhost:80/secrets", true},
		{"identical", "https://v.vault.azure.net", "https://v.vault.azure.net/secrets", true},
		{"host is case-insensitive", "https://v.vault.azure.net", "https://V.Vault.Azure.NET:443/secrets", true},
		{"custom port equals itself", "https://v.vault.azure.net:8443", "https://v.vault.azure.net:8443/secrets", true},
		{"custom port is not the default", "https://v.vault.azure.net:8443", "https://v.vault.azure.net:443/secrets", false},
		{"default is not a custom port", "https://v.vault.azure.net", "https://v.vault.azure.net:8443/secrets", false},
		{"foreign host", "https://v.vault.azure.net", "https://evil.example/secrets", false},
		{"foreign host with the port spelled out", "https://v.vault.azure.net", "https://evil.example:443/secrets", false},
		{"foreign port", "https://127.0.0.1:5000", "https://127.0.0.1:5001/secrets", false},
		{"foreign scheme", "https://v.vault.azure.net", "http://v.vault.azure.net/secrets", false},
		{"http :443 is not https", "http://localhost", "http://localhost:443/secrets", false},
		{"https :80 is not http", "https://v.vault.azure.net", "https://v.vault.azure.net:80/secrets", false},
		{"http on the https port is not https", "https://v.vault.azure.net", "http://v.vault.azure.net:443/secrets", false},
		{"https on the http port is not http", "http://localhost", "https://localhost:80/secrets", false},
		{"userinfo", "https://v.vault.azure.net", "https://u:p@v.vault.azure.net/secrets", false},
		{"a Kelvin sign is not a k", "https://kvs.vault.azure.net", "https://\u212avs.vault.azure.net/secrets", false},
		{"a long s is not an s", "https://kvs.vault.azure.net", "https://kv\u017f.vault.azure.net/secrets", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sameOrigin(mustURL(t, c.base), mustURL(t, c.link)); got != c.want {
				t.Fatalf("sameOrigin(%s, %s) = %v; want %v", c.base, c.link, got, c.want)
			}
		})
	}
}

// A kid that spells out the default port is still a key on this vault.
func TestKidVersion_DefaultPortSpelledOut(t *testing.T) {
	k := &KEK{c: &client{base: mustURL(t, "https://v.vault.azure.net")}}
	const ver = "0123456789abcdef0123456789abcdef"
	for _, kid := range []string{
		"https://v.vault.azure.net/keys/wrap/" + ver,
		"https://v.vault.azure.net:443/keys/wrap/" + ver,
		"https://V.VAULT.azure.net:443/keys/wrap/" + ver,
	} {
		if got, err := k.kidVersion(kid, "wrap"); err != nil || got != ver {
			t.Errorf("kidVersion(%q) = %q, %v; want %q", kid, got, err, ver)
		}
	}
	for _, kid := range []string{
		"https://v.vault.azure.net:8443/keys/wrap/" + ver,
		"https://other.vault.azure.net:443/keys/wrap/" + ver,
		"http://v.vault.azure.net/keys/wrap/" + ver,
		"http://v.vault.azure.net:443/keys/wrap/" + ver,
	} {
		if _, err := k.kidVersion(kid, "wrap"); err == nil || !strings.Contains(err.Error(), "off this vault") {
			t.Errorf("kidVersion(%q) = %v; want a refusal", kid, err)
		}
	}
}

const virtualVault = "vault.vault.azure.net"

// mapVault sends every request for the virtual vault origin (any port) to the
// fake server; anything else, such as the token endpoint, goes straight
// through.
type mapVault struct {
	f    *fakeKV
	next http.RoundTripper
}

func (m mapVault) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() == virtualVault {
		u := mustURLNoT(m.f.srv.URL)
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
	}
	return m.next.RoundTrip(r)
}

func mustURLNoT(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// virtualStore is a Store whose vault URL is https://vault.vault.azure.net
// (default port), served by f through a transport that maps that origin.
func virtualStore(t *testing.T, f *fakeKV) *Store {
	t.Helper()
	s := newFakeStore(t, f, func(c *Config) { c.VaultURL = "https://" + virtualVault })
	s.c.http.Transport = mapVault{f: f, next: s.c.http.Transport}
	return s
}

func setNextLinkHost(f *fakeKV, host string) {
	f.mu.Lock()
	f.nextLinkHost = host
	f.mu.Unlock()
}

// Put past one page of versions follows a nextLink that spells out :443.
func TestPut_FollowsDefaultPortNextLinkAcrossPages(t *testing.T) {
	f := newFakeKV(t)
	setNextLinkHost(f, "https://"+virtualVault+":443")
	s := virtualStore(t, f)
	ctx := t.Context()
	ref := ""
	for i := range 27 { // the 27th lists 26 versions: two pages
		var err error
		if ref, err = s.Put(ctx, "alice", "pat", ref, []byte("v"+strconv.Itoa(i)), false); err != nil {
			t.Fatalf("Put #%d: %v", i+1, err)
		}
	}
	sn, _, _, err := s.parse("alice", "pat", ref)
	if err != nil {
		t.Fatal(err)
	}
	if enabled, total := f.liveVersions(sn); total != 27 || enabled != 1 {
		t.Fatalf("versions = %d enabled of %d; want 1 of 27 (every older version listed and disabled)", enabled, total)
	}
}

// Walk past one page of secrets follows a nextLink that spells out :443.
func TestWalk_FollowsDefaultPortNextLinkAcrossPages(t *testing.T) {
	f := newFakeKV(t)
	setNextLinkHost(f, "https://"+virtualVault+":443")
	s := virtualStore(t, f)
	ctx := t.Context()
	const n = 30
	for i := range n {
		if _, err := s.Put(ctx, "", "op"+strconv.Itoa(i), "", []byte("v"), false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Walk(ctx)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != n {
		t.Fatalf("Walk = %d entries; want %d", len(got), n)
	}
}

// A real random-port server that answers :443 links is a different origin and
// stays refused: the default port is not a wildcard.
func TestNextLink_DefaultPortOnACustomPortVaultIsRefused(t *testing.T) {
	f := newFakeKV(t)
	f.pageSize = 1
	s := newFakeStore(t, f) // vault is http://127.0.0.1:<random>
	ctx := t.Context()
	for _, name := range []string{"a", "b"} {
		if _, err := s.Put(ctx, "", name, "", []byte("v"), false); err != nil {
			t.Fatal(err)
		}
	}
	setNextLinkHost(f, "http://"+mustURL(t, f.srv.URL).Hostname()+":80")
	if _, err := s.Walk(ctx); err == nil || !strings.Contains(err.Error(), "off this vault") {
		t.Fatalf("Walk with a default-port link on a custom-port vault = %v; want a refusal", err)
	}
	setNextLinkHost(f, "https://"+mustURL(t, f.srv.URL).Hostname()+":443")
	if _, err := s.Walk(ctx); err == nil || !strings.Contains(err.Error(), "off this vault") {
		t.Fatalf("Walk with a :443 link on a custom-port http vault = %v; want a refusal", err)
	}
}
