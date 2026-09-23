// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// credInput.Protocol was parsed and never used, so `git clone
// http://dev.azure.com/…` (ADO is allowlisted, and a bare allow entry matches
// any port) got the brokered PAT emitted and git sent it as `Authorization:
// Basic` in CLEARTEXT. ADO is not on the git-broker route — it rides handlePlain
// — and the plain lane refuses cleartext only for INJECTION rules, so nothing
// downstream catches it. This pins the refusal at the door that hands the
// credential out.
func TestGetPlainHTTPEmitsNothing(t *testing.T) {
	var minted atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, _ *http.Request) {
		minted.Store(true)
		writeJSON(w, http.StatusOK, mintResponse{
			Kind: "git_pat", Token: "azdo-pat-value-12345", Username: "pat", JTI: "jti-pat",
			ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "")
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"dev.azure.com":"pat-grant-1"}`)

	stdin := strings.NewReader("protocol=http\nhost=dev.azure.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run must not error — git has to fall through, not break: %v", err)
	}
	if stdout.Len() > 0 {
		t.Fatalf("plain-http credential request must emit NOTHING, got: %q", stdout.String())
	}
	if minted.Load() {
		t.Fatal("plain-http credential request must not even mint — the credential is never created, not merely withheld")
	}
	// Asserted THROUGH the DRAFT constant, never against a literal: the wording
	// is still pending canon, and a test pinning a copy of it would have to be
	// edited by the canon pass instead of surviving it.
	if want := fmt.Sprintf(helperRefusePlaintext, "http"); stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

// A scheme git spells in another case is still the same scheme: the git
// credential protocol does not promise a canonical case, so the gate compares
// case-insensitively rather than handing an operator a refusal for "HTTPS".
func TestGetProtocolCaseInsensitive(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mintResponse{
			Kind: "git_pat", Token: "azdo-pat-value-12345", Username: "pat", JTI: "jti-pat",
			ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "")
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"dev.azure.com":"pat-grant-1"}`)

	stdin := strings.NewReader("protocol=HTTPS\nhost=dev.azure.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout.String(), "password=azdo-pat-value-12345") {
		t.Fatalf("HTTPS (upper case) is https and must still emit, got: %q", stdout.String())
	}
}

// NEGATIVE CONTROL for an ordinary https request over the SAME grant
// still emits. The gate must refuse a transport, not a forge.
func TestGetHTTPSStillEmits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mintResponse{
			Kind: "git_pat", Token: "azdo-pat-value-12345", Username: "pat", JTI: "jti-pat",
			ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "")
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"dev.azure.com":"pat-grant-1"}`)

	stdin := strings.NewReader("protocol=https\nhost=dev.azure.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertContains(t, stdout.String(), "username=pat")
	assertContains(t, stdout.String(), "password=azdo-pat-value-12345")
}

// An unmatched host over plain http must STILL be silent on stderr as well as
// stdout: the helper is configured globally, so every unrelated plain-http git
// operation on the box would otherwise collect a warning about a credential
// that was never going to be emitted.
func TestGetPlainHTTPUnmatchedHostIsSilent(t *testing.T) {
	t.Setenv("WARDYN_PROXY_URL", "http://localhost:1") // unreachable — must not be hit
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "")
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"dev.azure.com":"pat-grant-1"}`)

	stdin := strings.NewReader("protocol=http\nhost=bitbucket.org\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout.Len() > 0 || stderr.Len() > 0 {
		t.Fatalf("an unbrokered host must be silent on both streams, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// strings.Cut(v, ":") split at the FIRST colon, truncating a
// bracketed IPv6 literal to "[2001" — fail-safe (nothing matches that key) but
// silently unbrokered, with no explanation for an operator who wired the grant.
func TestParseInputHostPort(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bare host", "github.com", "github.com"},
		{"host with port", "github.com:443", "github.com"},
		{"ipv6 with port", "[2001:db8::1]:443", "2001:db8::1"},
		{"ipv6 no port", "[2001:db8::1]", "2001:db8::1"},
		{"ipv4 with port", "192.0.2.10:8443", "192.0.2.10"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ci := parseInput(strings.NewReader("protocol=https\nhost=" + c.in + "\n\n"))
			if ci.Host != c.want {
				t.Fatalf("host %q parsed to %q, want %q", c.in, ci.Host, c.want)
			}
		})
	}
}

// An IPv6 forge with a port really reaches its grant end-to-end — the parse fix
// is only worth anything if the resolved host matches the grant map key.
func TestGetIPv6HostWithPortResolvesGrant(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mintResponse{
			Kind: "git_pat", Token: "v6-pat-value", Username: "pat", JTI: "jti-v6",
			ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "")
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"2001:db8::1":"pat-grant-v6"}`)

	stdin := strings.NewReader("protocol=https\nhost=[2001:db8::1]:443\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertContains(t, stdout.String(), "password=v6-pat-value")
}
