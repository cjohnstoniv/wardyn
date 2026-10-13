// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func redirectClient(t *testing.T, addr string) *client {
	t.Helper()
	c, err := newClient(Config{Addr: addr, Namespace: "team"})
	if err != nil {
		t.Fatal(err)
	}
	c.token, c.backoff = "s.token", time.Millisecond
	return c
}

// A Vault that answers 307 or 308 to another origin (a standby naming the
// active node, a load balancer downgrading to http) is not followed: the token,
// the namespace and the replayed body stay with the address that was configured.
func TestClient_ARedirectOffItsOriginIsNotFollowed(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	t.Cleanup(other.Close)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var asked atomic.Int32
		vault := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked.Add(1)
			http.Redirect(w, r, other.URL+r.URL.Path, status)
		}))
		c := redirectClient(t, vault.URL)
		c.http.Transport = vault.Client().Transport
		_, err := c.call(t.Context(), http.MethodPut, "wardyn/data/k", map[string]string{"value": "a-credential"}, nil)
		vault.Close()
		if statusOf(err) != status || asked.Load() != 1 {
			t.Errorf("a %d to another origin: err = %v after %d requests; want the %d reported after one", status, err, asked.Load(), status)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("%d redirected requests reached another origin over plain http", n)
	}
}

// redirector answers the first request with a 307 to location and records
// where the client went next, and with which token.
type redirector struct {
	location string
	next     []string
}

func (r *redirector) RoundTrip(req *http.Request) (*http.Response, error) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: req}
	if r.location != "" {
		resp.StatusCode = http.StatusTemporaryRedirect
		resp.Header.Set("Location", r.location)
		r.location = ""
		return resp, nil
	}
	r.next = append(r.next, req.URL.String()+" "+req.Header.Get("X-Vault-Token"))
	return resp, nil
}

func TestClient_RedirectOriginIsSchemeHostAndPort(t *testing.T) {
	for _, tc := range []struct{ addr, location, follows string }{
		{"https://vault.example:8200", "/v1/y", "https://vault.example:8200/v1/y"},
		{"https://vault.example:8200", "https://VAULT.example:8200/v1/y", "https://VAULT.example:8200/v1/y"},
		{"https://vault.example", "https://vault.example:443/v1/y", "https://vault.example:443/v1/y"},
		{"https://vault.example:8200", "http://vault.example:8200/v1/y", ""},
		{"https://vault.example:8200", "https://vault.example.other.test:8200/v1/y", ""},
		{"https://vault.example:8200", "https://vault.example:8201/v1/y", ""},
		{"https://vault.example:8200", "https://vault.example/v1/y", ""},
		{"https://vault.example", "https://vault.example:8200/v1/y", ""},
		{"https://vault.example:8200", "https://u:p@vault.example:8200/v1/y", ""},
		{"https://vault.example:8200", "https://@vault.example:8200/v1/y", ""},
		// Unicode case folding is not DNS's: a Kelvin sign is not a k, a long s not an s.
		{"https://vault.kube.example:8200", "https://vault.\u212aube.example:8200/v1/y", ""},
		{"https://vault.secret.example:8200", "https://vault.\u017fecret.example:8200/v1/y", ""},
	} {
		c := redirectClient(t, tc.addr)
		rt := &redirector{location: tc.location}
		c.http.Transport = rt
		_, err := c.call(t.Context(), http.MethodGet, "x", nil, nil)
		if tc.follows == "" {
			if statusOf(err) != http.StatusTemporaryRedirect || len(rt.next) != 0 {
				t.Errorf("%s redirected to %s: err = %v, then requested %v; want the 307 reported and no request", tc.addr, tc.location, err, rt.next)
			}
			continue
		}
		if err != nil || len(rt.next) != 1 || rt.next[0] != tc.follows+" s.token" {
			t.Errorf("%s redirected to %s: err = %v, then requested %v; want %s with the token", tc.addr, tc.location, err, rt.next, tc.follows)
		}
	}
}

// A redirect within the origin still carries the call, and a loop of them ends
// at ten requests an attempt, as it did under net/http's own policy.
func TestClient_SameOriginRedirectsAreFollowedAndBounded(t *testing.T) {
	var looped atomic.Int32
	var arrived atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/moved":
			http.Redirect(w, r, "/v1/here", http.StatusTemporaryRedirect)
		case "/v1/here":
			body, _ := io.ReadAll(r.Body)
			arrived.Store(r.Header.Get("X-Vault-Token") + " " + r.Header.Get("X-Vault-Namespace") + " " + string(body))
			reply(w, http.StatusOK, map[string]any{})
		default:
			looped.Add(1)
			http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
		}
	}))
	t.Cleanup(srv.Close)
	c := redirectClient(t, srv.URL)
	if _, err := c.call(t.Context(), http.MethodPut, "moved", map[string]string{"value": "v"}, nil); err != nil {
		t.Fatalf("a same-origin 307: %v", err)
	}
	if got, want := arrived.Load(), `s.token team {"value":"v"}`; got != want {
		t.Errorf("the redirected request carried %q, want %q", got, want)
	}
	if _, err := c.call(t.Context(), http.MethodGet, "loop", nil, nil); statusOf(err) != 0 {
		t.Errorf("a redirect loop: err = %v, want a transport failure", err)
	}
	if n, want := looped.Load(), int32(10*(retries+1)); n != want {
		t.Errorf("a redirect loop made %d requests, want %d (ten an attempt)", n, want)
	}
}
