// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A registry whose challenge spells the scope differently from the one the client computes still gets
// its token reused: one token request for two fetches.
func TestChallengeScopeSpellingStillCachesTheToken(t *testing.T) {
	var tokenRequests atomic.Int32
	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "expires_in": 300})
	}))
	defer realm.Close()
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://127.0.0.1:`+port(realm)+`/token",service="reg.test",scope="repository:team/app:pull,push"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serveManifest(w, imageBody, ociManifestType, "")
	}))
	defer reg.Close()
	c := newTestClient(t, nil, testGuard(), nil, reg, realm)
	for range 2 {
		if _, err := c.Manifest(context.Background(), mustRef(t, host(reg)+"/team/app:1.0")); err != nil {
			t.Fatal(err)
		}
	}
	if n := tokenRequests.Load(); n != 1 {
		t.Errorf("token requests = %d, want 1: the second fetch must reuse the token", n)
	}
}

type failingAuth struct {
	ok   mapAuth
	fail string
}

func (f failingAuth) Lookup(host string) (RegistryAuth, bool, error) {
	if host == f.fail {
		return RegistryAuth{}, false, errors.New("read auth file: permission denied")
	}
	return f.ok.Lookup(host)
}

// An unreadable auth file on a redirect destination is reported, not turned into an anonymous request.
func TestRedirectDestinationAuthLookupErrorIsReported(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(layerBytes)
	}))
	defer target.Close()
	var loc atomic.Value
	loc.Store("https://127.0.0.1:" + port(target) + "/blob")
	reg := blobRegistry(&loc)
	defer reg.Close()
	auth := failingAuth{ok: mapAuth{host(reg): {Username: "u", Password: "p"}}, fail: "127.0.0.1:" + port(target)}
	c := newTestClient(t, auth, testGuard(), nil, reg, target)
	_, err := c.Blob(context.Background(), mustRef(t, host(reg)+"/team/app:1.0"), layerDesc())
	if err == nil || !strings.Contains(err.Error(), "read auth file") {
		t.Fatalf("err = %v, want it to name the auth file read", err)
	}
	if hits.Load() != 0 {
		t.Errorf("the destination was asked %d times without its credential", hits.Load())
	}
}
