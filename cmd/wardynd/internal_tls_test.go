// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// mapKeyStore is the pg store's contract in memory: absent => secretstore.ErrNotFound
// joined with pgx.ErrNoRows.
type mapKeyStore map[string][]byte

func (m mapKeyStore) Get(_ context.Context, name string) ([]byte, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return nil, errors.Join(secretstore.ErrNotFound, pgx.ErrNoRows)
}

func (m mapKeyStore) Put(_ context.Context, name string, v []byte) error {
	m[name] = v
	return nil
}

func TestLoadHopTLS_RefusesPlaintextOnNonLocalInstall(t *testing.T) {
	for _, u := range []string{"http://wardynd:8080", "http://wardyn.wardyn.svc.cluster.local:8080", "http://host.docker.internal:8080"} {
		store := mapKeyStore{}
		_, err := loadHopTLS(context.Background(), unlocked(store), u)
		if err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "WARDYN_INTERNAL_LISTEN") {
			t.Errorf("%s: boot must refuse, naming the fix; got %v", u, err)
		}
		if len(store) != 0 {
			t.Errorf("%s: a refused boot minted a CA", u)
		}
	}
}

func TestLoadHopTLS_LoopbackHTTPIsLocal(t *testing.T) {
	store := mapKeyStore{}
	hop, err := loadHopTLS(context.Background(), unlocked(store), "http://127.0.0.1:8080")
	if err != nil || hop != nil {
		t.Fatalf("loopback http is the local install: want (nil, nil), got (%v, %v)", hop, err)
	}
	if len(store) != 0 || hop.caCertPEM() != "" {
		t.Fatal("a local install must mint no CA and hand proxies none")
	}
}

func TestLoadHopTLS_CAStableAcrossBoots_RotatedNearExpiry(t *testing.T) {
	store := mapKeyStore{}
	first, err := loadHopTLS(context.Background(), unlocked(store), "https://wardynd:8443")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadHopTLS(context.Background(), unlocked(store), "https://wardynd:8443")
	if err != nil {
		t.Fatal(err)
	}
	if first.caPEM == "" || first.caPEM != second.caPEM {
		t.Fatal("a restart must keep the CA live runs already pin")
	}
	leaf, err := x509.ParseCertificate(second.server.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("wardynd"); err != nil {
		t.Fatalf("the serving cert must name the control-plane URL's host: %v", err)
	}

	stale, err := hoptls.NewCA(time.Now().Add(-9*365*24*time.Hour - time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	store[secretInternalCA] = stale
	rotated, err := loadHopTLS(context.Background(), unlocked(store), "https://wardynd:8443")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.caPEM == first.caPEM || string(store[secretInternalCA]) == string(stale) {
		t.Fatal("a CA inside its rotation window must be replaced at boot")
	}
}

// The listener serves the proxy's routes over TLS a pinned client verifies,
// nothing else, and fails closed for a client pinned to another CA.
func TestInternalListener_OnlyInternalRoutes_OverPinnedTLS(t *testing.T) {
	hop, err := loadHopTLS(context.Background(), unlocked(mapKeyStore{}), "https://127.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	var sawTLS atomic.Bool
	srv := httptest.NewUnstartedServer(internalRoutesOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTLS.Store(r.TLS != nil)
		w.WriteHeader(http.StatusTeapot)
	})))
	srv.TLS = hop.server
	srv.StartTLS()
	defer srv.Close()

	pinned, _ := hoptls.ClientConfig(hop.caPEM)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: pinned}, Timeout: 5 * time.Second}
	for path, want := range map[string]int{
		"/api/v1/internal/injection/x": http.StatusTeapot,
		"/healthz":                     http.StatusTeapot,
		"/api/v1/runs":                 http.StatusNotFound,
		"/":                            http.StatusNotFound,
	} {
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", path, resp.StatusCode, want)
		}
	}
	if !sawTLS.Load() {
		t.Fatal("the internal route was not served over TLS")
	}

	otherBlob, _ := hoptls.NewCA(time.Now())
	other, _ := hoptls.ParseCA(otherBlob)
	wrongCfg, _ := hoptls.ClientConfig(string(other.CertPEM))
	wrong := &http.Client{Transport: &http.Transport{TLSClientConfig: wrongCfg}, Timeout: 5 * time.Second}
	if _, err := wrong.Get(srv.URL + "/api/v1/internal/injection/x"); err == nil {
		t.Fatal("a client pinned to another CA must fail the handshake")
	}
}

// A bind failure ends the daemon rather than leaving every run's credential
// resolve with nowhere to go.
func TestInternalListener_BindFailureIsFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hop, err := loadHopTLS(context.Background(), unlocked(mapKeyStore{}), "https://wardynd:8443")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	errCh := make(chan error, 1)
	srv := startInternalListener(hop, &bootFlags{internalListen: &addr}, http.NotFoundHandler(), errCh)
	defer srv.Close()
	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "internal TLS listener") {
			t.Fatalf("error does not name the listener: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a listener that cannot bind must report it")
	}
	if startInternalListener(nil, &bootFlags{}, http.NotFoundHandler(), errCh) != nil {
		t.Fatal("a local install has no listener")
	}
}

// The listener's floor is TLS 1.3: a client capped at TLS 1.2 is refused even
// when it pins the right CA.
func TestInternalListener_RefusesTLS12Client(t *testing.T) {
	hop, err := loadHopTLS(context.Background(), unlocked(mapKeyStore{}), "https://127.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(internalRoutesOnly(http.NotFoundHandler()))
	srv.TLS = hop.server
	srv.StartTLS()
	defer srv.Close()
	cfg, _ := hoptls.ClientConfig(hop.caPEM)
	cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	_, err = c.Get(srv.URL + "/healthz")
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("a TLS 1.2 client must be refused, got %v", err)
	}
}

// The ingest pins this listener from the file wardynd publishes beside the
// groundtruth token file: it must hold exactly the CA the listener's
// certificate chains to, and must not appear on a local (loopback http)
// install or one without a token file.
func TestPublishHopCA_BesideTheGroundtruthToken(t *testing.T) {
	hop, err := loadHopTLS(context.Background(), unlocked(mapKeyStore{}), "https://127.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	gtFile := filepath.Join(t.TempDir(), "token")
	if err := publishHopCA(hop, gtFile); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(gtFile), hopCAFileName))
	if err != nil {
		t.Fatalf("no CA published beside the token file: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) || strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatalf("published file must be the CA certificate alone:\n%s", b)
	}
	leaf, err := x509.ParseCertificate(hop.server.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "127.0.0.1"}); err != nil {
		t.Fatalf("the listener's certificate does not chain to the published CA: %v", err)
	}

	for name, tc := range map[string]struct {
		hop *hopTLS
		gt  string
	}{"loopback http": {nil, filepath.Join(t.TempDir(), "token")}, "no token file": {hop, ""}} {
		if err := publishHopCA(tc.hop, tc.gt); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tc.gt != "" {
			if _, err := os.Stat(filepath.Join(filepath.Dir(tc.gt), hopCAFileName)); !os.IsNotExist(err) {
				t.Errorf("%s: published a CA file", name)
			}
		}
	}
}

// hopListeners serves one api.Server the way serveAndShutdown does: the
// console handler in plaintext, and, when the hop is TLS, the internal
// listener pinned to the CA the server's config carries. It returns the console
// base URL, and the internal URL with its pinned client ("" and nil when the
// control-plane URL is loopback http).
func hopListeners(t *testing.T, controlURL, basePath string) (string, string, *http.Client) {
	t.Helper()
	hop, err := loadHopTLS(context.Background(), unlocked(mapKeyStore{}), controlURL)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(api.Config{ControlPlaneURL: controlURL, ControlPlaneCAPEM: hop.caCertPEM(), BasePath: basePath})
	console := httptest.NewServer(srv.Handler())
	t.Cleanup(console.Close)
	if hop == nil {
		return console.URL + basePath, "", nil
	}
	internal := httptest.NewUnstartedServer(internalRoutesOnly(srv.InternalHandler()))
	internal.TLS = hop.server
	internal.StartTLS()
	t.Cleanup(internal.Close)
	pinned, _ := hoptls.ClientConfig(hop.caPEM)
	return console.URL + basePath, internal.URL, &http.Client{Transport: &http.Transport{TLSClientConfig: pinned}, Timeout: 5 * time.Second}
}

// postStatus POSTs an empty body with no credential and returns status and body.
func postStatus(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// internalProbePaths are one route of each internal auth group: the run-token
// surface and the ground-truth sensor's.
var internalProbePaths = []string{api.InternalPathPrefix + "decisions", api.InternalPathPrefix + "groundtruth"}

// Once the hop is TLS the pinned listener is the only way into the internal
// surface (#1263): the console answers it with the very 404 the internal
// listener gives a console route, under a base path too, while the internal
// listener still reaches each route's own auth (401 without a credential).
func TestInternalRoutesRefusedOnConsoleListenerWhenHopTLS(t *testing.T) {
	for _, base := range []string{"", "/wardyn"} {
		console, internal, pinned := hopListeners(t, "https://127.0.0.1:8443", base)
		resp, err := pinned.Get(internal + "/api/v1/runs")
		if err != nil {
			t.Fatal(err)
		}
		notHere, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("internal listener served a console route: %d", resp.StatusCode)
		}
		for _, p := range internalProbePaths {
			if code, body := postStatus(t, http.DefaultClient, console+p); code != http.StatusNotFound || body != string(notHere) {
				t.Errorf("base %q: console %s = %d %q, want the internal listener's own 404 %q", base, p, code, body, notHere)
			}
			if code, _ := postStatus(t, pinned, internal+p); code != http.StatusUnauthorized {
				t.Errorf("base %q: internal listener %s = %d, want 401 from the route's auth", base, p, code)
			}
		}
		if resp, err := http.Get(console + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("base %q: the console must keep serving its own routes: %v %v", base, resp, err)
		} else {
			_ = resp.Body.Close()
		}
	}
}

// A local install (loopback http control plane) has no internal listener, so
// its proxies call the console listener, which keeps serving the surface.
func TestInternalRoutesServedOnConsoleListenerWithoutHopTLS(t *testing.T) {
	console, internal, _ := hopListeners(t, "http://127.0.0.1:8080", "")
	if internal != "" {
		t.Fatal("loopback http must start no internal listener")
	}
	for _, p := range internalProbePaths {
		if code, _ := postStatus(t, http.DefaultClient, console+p); code != http.StatusUnauthorized {
			t.Errorf("console %s = %d, want 401 from the route's auth", p, code)
		}
	}
}
