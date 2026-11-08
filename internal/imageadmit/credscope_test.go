// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sink records every request it receives, with body and headers.
type sink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
}

func newSink() *sink {
	s := &sink{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization")+" body="+string(b))
		s.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	return s
}

func (s *sink) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func TestTokenRequestNeverFollowedToAnotherOrigin(t *testing.T) {
	sources := map[string]RegistryAuth{
		"refresh token body": {IdentityToken: "REFRESH-SECRET"},
		"basic credential":   {Username: "alice", Password: "BASIC-SECRET"},
	}
	for name, cred := range sources {
		for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(name+"/"+http.StatusText(code), func(t *testing.T) {
				b := newSink()
				defer b.srv.Close()
				realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "https://127.0.0.1:"+port(b.srv)+"/steal", code)
				}))
				defer realm.Close()
				reg := bearerRegistry("https://"+host(realm)+"/token", "tok", nil)
				defer reg.Close()
				c := newTestClient(t, mapAuth{host(reg): cred}, testGuard(), nil, reg, realm, b.srv)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, err := c.Manifest(ctx, mustRef(t, host(reg)+"/team/app:1.0"))
				if err == nil {
					t.Fatal("a token request redirected to another origin succeeded")
				}
				var un *Unavailable
				if !errors.As(err, &un) || !errors.Is(err, ErrRefusedTarget) {
					t.Errorf("err = %v, want Unavailable{refused}", err)
				}
				for _, r := range b.seen() {
					if strings.Contains(r, "SECRET") || strings.Contains(r, "auth=Basic") {
						t.Errorf("the other origin received a credential: %s", r)
					}
				}
				if got := b.seen(); len(got) != 0 {
					t.Errorf("the other origin was contacted: %v", got)
				}
			})
		}
	}
}

func TestChallengeFromAnotherOriginIsNotAnsweredWithTheRegistryCredential(t *testing.T) {
	sources := map[string]RegistryAuth{
		"basic credential": {Username: "alice", Password: "BASIC-SECRET"},
		"identity token":   {IdentityToken: "REFRESH-SECRET"},
	}
	for name, cred := range sources {
		t.Run(name, func(t *testing.T) {
			e := newSink() // the realm the CDN names
			defer e.srv.Close()
			cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://127.0.0.1:`+port(e.srv)+`/token",service="cdn"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer cdn.Close()
			reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://127.0.0.1:"+port(cdn)+"/blob", http.StatusFound)
			}))
			defer reg.Close()
			c := newTestClient(t, mapAuth{host(reg): cred}, testGuard(), nil, reg, cdn, e.srv)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := c.Blob(ctx, mustRef(t, host(reg)+"/team/app"), layerDesc())
			var un *Unavailable
			if !errors.As(err, &un) || un.Cause != CauseUnauthorized || !un.Withheld {
				t.Fatalf("err = %v, want Unavailable{unauthorized, withheld}", err)
			}
			if want := "checking images from " + host(reg) + " got 401 from an address it redirected to, where its credential is not sent"; un.Error() != want {
				t.Fatalf("Error() = %q, want %q", un.Error(), want)
			}
			if got := e.seen(); len(got) != 0 {
				t.Fatalf("the CDN's realm received a request: %v", got)
			}
		})
	}
}

func TestChallengeFromAnotherOriginUsesTheCredentialConfiguredForIt(t *testing.T) {
	// The CDN has its own credential; the registry's is not used for it, and a realm the CDN names is only
	// ever asked with the CDN's. Here the CDN answers a Basic challenge and its own credential is applied by
	// the redirect rule, so no challenge round trip is needed and the blob arrives.
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "cdn" || p != "cdnpw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="cdn"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(layerBytes)
	}))
	defer cdn.Close()
	var loc = "https://127.0.0.1:" + port(cdn) + "/blob"
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loc, http.StatusFound)
	}))
	defer reg.Close()
	auth := mapAuth{host(reg): {Username: "u", Password: "p"}, "127.0.0.1:" + port(cdn): {Username: "cdn", Password: "cdnpw"}}
	c := newTestClient(t, auth, testGuard(), nil, reg, cdn)
	b, err := c.Blob(context.Background(), mustRef(t, host(reg)+"/team/app"), layerDesc())
	if err != nil || string(b) != string(layerBytes) {
		t.Fatalf("blob = %q, err = %v", b, err)
	}
}

// The registry first challenges, accepts its credential on the retry and redirects to another origin, which
// challenges too. The credential is not sent there, so the 401 is not blamed on it.
func TestChallengeFromAnotherOriginAfterTheRegistryAcceptedItsCredential(t *testing.T) {
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the other origin received an Authorization header")
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="cdn"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer cdn.Close()
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "https://127.0.0.1:"+port(cdn)+"/blob", http.StatusFound)
	}))
	defer reg.Close()
	c := newTestClient(t, mapAuth{host(reg): {Username: "alice", Password: "BASIC-SECRET"}}, testGuard(), nil, reg, cdn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Blob(ctx, mustRef(t, host(reg)+"/team/app"), layerDesc())
	var un *Unavailable
	if !errors.As(err, &un) || un.Cause != CauseUnauthorized || !un.Withheld {
		t.Fatalf("err = %v, want Unavailable{unauthorized, withheld}", err)
	}
}
