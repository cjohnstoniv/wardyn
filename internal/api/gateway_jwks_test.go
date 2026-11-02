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
// serveIndex or http.NotFound for GET, chi's 405 or 404 for the rest. The sibling
// is the comparison; the status and body pin which real answer that is, so both
// cannot drift together.
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
						case ui == "":
							if got.Code != http.StatusNotFound || got.Body.String() != notFound {
								t.Errorf("no console: %d %q, want http.NotFound", got.Code, got.Body)
							}
						case method == http.MethodGet:
							if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "<!doctype html>") || got.Header().Get("Cache-Control") != "no-cache" {
								t.Errorf("console: %d %v %q, want the shell", got.Code, got.Header(), got.Body)
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
	token, _, err := signer.Sign(gatewayidentity.Claims{Issuer: "spiffe://wardyn.local", Audience: "a", Subject: "s", Run: "r", Provider: "p"})
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
