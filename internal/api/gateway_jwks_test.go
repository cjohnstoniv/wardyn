// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
)

const (
	testGatewayJWKSPath = "/.well-known/wardyn-experimental/gateway-jwks.json"
	testGatewaySibling  = "/.well-known/wardyn-experimental/not-a-route.json"
)

func testGatewayServer(t *testing.T, uiDir, base string, id func(context.Context, bool) (*gatewayidentity.Signer, error)) *Server {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), newAuthzStore())
	cfg.UIDir, cfg.BasePath, cfg.GatewayIdentity = uiDir, base, id
	return New(cfg)
}

func testGatewayUIDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<!doctype html><script src="./assets/a.js"></script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testGatewayGet(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// Off answers byte for byte as a path no route holds, at every method, with and
// without a console and a base path, with no signer wired and with no key row:
// http.NotFound for GET, chi's 405 or 404 for the rest. The sibling is the
// comparison; the status and body pin which real answer that is, so both cannot
// drift together.
func TestGatewayJWKSOffEqualsAnUnregisteredPath(t *testing.T) {
	noKey := func(context.Context, bool) (*gatewayidentity.Signer, error) { return nil, nil }
	uiDir := testGatewayUIDir(t)
	const notFound = "404 page not found\n"
	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}
	for _, ui := range []string{"", uiDir} {
		for _, base := range []string{"", "/wardyn"} {
			for signerName, id := range map[string]func(context.Context, bool) (*gatewayidentity.Signer, error){"no signer": nil, "no key": noKey} {
				srv := testGatewayServer(t, ui, base, id)
				for _, method := range methods {
					t.Run(fmt.Sprintf("ui=%v base=%q %s %s", ui != "", base, signerName, method), func(t *testing.T) {
						got := testGatewayGet(t, srv, method, base+testGatewayJWKSPath)
						want := testGatewayGet(t, srv, method, base+testGatewaySibling)
						if got.Code != want.Code || got.Body.String() != want.Body.String() || !reflect.DeepEqual(got.Header(), want.Header()) {
							t.Errorf("route: %d %v %q\nsibling: %d %v %q", got.Code, got.Header(), got.Body, want.Code, want.Header(), want.Body)
						}
						switch {
						case ui == "" || method == http.MethodGet:
							if got.Code != http.StatusNotFound || got.Body.String() != notFound {
								t.Errorf("ui=%v: %d %q, want http.NotFound", ui != "", got.Code, got.Body)
							}
						default:
							if got.Code != http.StatusMethodNotAllowed || !slices.Equal(got.Header().Values("Allow"), []string{"GET"}) || got.Body.Len() != 0 {
								t.Errorf("console: %d %v %q, want chi's 405", got.Code, got.Header(), got.Body)
							}
						}
					})
				}
			}
		}
	}
}

// Under /.well-known/ the console answers a path naming no file with a plain
// 404, never its HTML shell; a file the deployment put there is still served;
// every other unknown path still gets the shell. The off JWKS answer matches an
// unregistered /.well-known/ path in status, every header and body, with and
// without a console.
func TestWellKnownUnservedIsPlain404(t *testing.T) {
	uiDir := testGatewayUIDir(t)
	if err := os.MkdirAll(filepath.Join(uiDir, ".well-known"), 0o750); err != nil {
		t.Fatal(err)
	}
	const own = `{"own":"file"}`
	if err := os.WriteFile(filepath.Join(uiDir, ".well-known", "own.json"), []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}
	noKey := func(context.Context, bool) (*gatewayidentity.Signer, error) { return nil, nil }
	const notFound = "404 page not found\n"
	for _, ui := range []string{"", uiDir} {
		for _, base := range []string{"", "/wardyn"} {
			srv := testGatewayServer(t, ui, base, noKey)
			get := func(p string) *httptest.ResponseRecorder { return testGatewayGet(t, srv, http.MethodGet, base+p) }
			name := fmt.Sprintf("ui=%v base=%q", ui != "", base)
			t.Run(name+" unserved", func(t *testing.T) {
				want := get("/.well-known/x")
				if want.Code != http.StatusNotFound || want.Body.String() != notFound {
					t.Fatalf("/.well-known/x: %d %q, want http.NotFound", want.Code, want.Body)
				}
				for _, p := range []string{testGatewayJWKSPath, testGatewaySibling, "/.well-known/", "/.well-known", "/.well-known/a/b/c"} {
					got := get(p)
					if got.Code != want.Code || got.Body.String() != want.Body.String() || !reflect.DeepEqual(got.Header(), want.Header()) {
						t.Errorf("%s: %d %v %q\nwant: %d %v %q", p, got.Code, got.Header(), got.Body, want.Code, want.Header(), want.Body)
					}
				}
			})
			if ui == "" {
				continue
			}
			t.Run(name+" own file", func(t *testing.T) {
				w := get("/.well-known/own.json")
				if w.Code != http.StatusOK || w.Body.String() != own {
					t.Errorf("own file: %d %q, want %q", w.Code, w.Body, own)
				}
			})
			t.Run(name+" spa unchanged", func(t *testing.T) {
				for _, p := range []string{"/runs/abc", "/well-known/x", "/.wellknown/x", "/x/.well-known/y"} {
					w := get(p)
					if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<!doctype html>") || w.Header().Get("Cache-Control") != "no-cache" {
						t.Errorf("%s: %d %v %q, want the shell", p, w.Code, w.Header(), w.Body)
					}
				}
			})
		}
	}
}

// On, the set is the same with a console as without one: the new 404 under
// /.well-known/ never reaches a route that is registered.
func TestGatewayJWKSOnWithAConsoleIsServed(t *testing.T) {
	raw, err := gatewayidentity.Rotate(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gatewayidentity.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	srv := testGatewayServer(t, testGatewayUIDir(t), "/wardyn", func(context.Context, bool) (*gatewayidentity.Signer, error) { return signer, nil })
	srv.cfg.Store.(*authzStore).siteCfg = testGatewaySite(testGatewayProvider())
	w := testGatewayGet(t, srv, http.MethodGet, "/wardyn"+testGatewayJWKSPath)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(w.Body.Bytes(), &set); err != nil || len(set.Keys) == 0 {
		t.Errorf("not a key set: %v %q", err, w.Body)
	}
}

// On: the set is served under the base path, carries the cache header a gateway
// reads, and verifies a token the signer issued. The caller never asks for a key
// to be made.
func TestGatewayJWKSServesTheKeysThatVerifyAToken(t *testing.T) {
	raw, err := gatewayidentity.Rotate(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gatewayidentity.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	var askedToCreate bool
	srv := testGatewayServer(t, "", "/wardyn", func(_ context.Context, create bool) (*gatewayidentity.Signer, error) {
		askedToCreate = askedToCreate || create
		return signer, nil
	})
	srv.cfg.Store.(*authzStore).siteCfg = testGatewaySite(testGatewayProvider())
	if w := testGatewayGet(t, srv, http.MethodGet, testGatewayJWKSPath); w.Code != http.StatusNotFound {
		t.Errorf("outside the base path: %d, want 404", w.Code)
	}
	w := testGatewayGet(t, srv, http.MethodGet, "/wardyn"+testGatewayJWKSPath)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if askedToCreate {
		t.Error("an anonymous read asked for a key to be made")
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(w.Body.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	token, _, err := signer.Sign(gatewayidentity.Claims{Issuer: "spiffe://wardyn.local", Audience: "a", Subject: "s", Run: "r", Provider: "p"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := parsed.Claims(set, &claims); err != nil {
		t.Errorf("the served set does not verify the token: %v", err)
	}
}

// A row that cannot be read is a plain 503 that names nothing and is not cached.
func TestGatewayJWKSUnreadableKeyIs503(t *testing.T) {
	srv := testGatewayServer(t, "", "", func(context.Context, bool) (*gatewayidentity.Signer, error) {
		return nil, errors.New("decrypt failed: row wardyn-gateway-identity-key")
	})
	srv.cfg.Store.(*authzStore).siteCfg = testGatewaySite(testGatewayProvider())
	w := testGatewayGet(t, srv, http.MethodGet, testGatewayJWKSPath)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if body := w.Body.String(); body != `{"status":"error"}`+"\n" && body != `{"status":"error"}` {
		t.Errorf("body = %q", body)
	}
}
