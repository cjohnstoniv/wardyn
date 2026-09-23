// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// testCPCAPEM is a real internal-CA certificate for fixtures that only need an
// https control-plane URL to pass config validation.
var testCPCAPEM = func() string {
	blob, err := hoptls.NewCA(time.Now())
	if err != nil {
		panic(err)
	}
	ca, err := hoptls.ParseCA(blob)
	if err != nil {
		panic(err)
	}
	return string(ca.CertPEM)
}()

func TestConfig_RefusesPlaintextControlPlaneOnNonLocalInstall(t *testing.T) {
	for _, url := range []string{"http://wardynd:8080", "http://wardyn.wardyn.svc.cluster.local:8080", "http://host.docker.internal:8080"} {
		_, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"control_plane_url": url}))
		if err == nil || !strings.Contains(err.Error(), "WARDYN_INTERNAL_LISTEN") {
			t.Errorf("%s: a non-loopback plaintext control plane must refuse to start, naming the fix; got %v", url, err)
		}
	}
	// Local: plaintext that never leaves the loopback interface.
	if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"control_plane_url": "http://127.0.0.1:8080", "control_plane_ca_pem": nil})); err != nil {
		t.Errorf("loopback http must load: %v", err)
	}
}

func TestConfig_HTTPSControlPlaneNeedsItsCA(t *testing.T) {
	_, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"control_plane_ca_pem": nil}))
	if err == nil || !strings.Contains(err.Error(), "control_plane_ca_pem") {
		t.Fatalf("https without the internal CA must refuse (no system-roots fallback); got %v", err)
	}
	_, err = LoadConfigBytes(baseConfigJSON(t, map[string]any{"control_plane_ca_pem": "garbage"}))
	if err == nil || !strings.Contains(err.Error(), "control_plane_ca_pem") {
		t.Fatalf("garbage control_plane_ca_pem must refuse; got %v", err)
	}
}

// hopCP is a TLS control plane that answers one injection grant and records
// whether each request arrived over TLS.
func hopCP(t *testing.T, grant uuid.UUID) (srv *httptest.Server, caPEM string, overTLS *atomic.Int32) {
	t.Helper()
	return hopCPMax(t, grant, 0)
}

// hopCPMax is hopCP with the server capped at maxVersion (0 = no cap).
func hopCPMax(t *testing.T, grant uuid.UUID, maxVersion uint16) (srv *httptest.Server, caPEM string, overTLS *atomic.Int32) {
	t.Helper()
	overTLS = &atomic.Int32{}
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			overTLS.Add(1)
		}
		if !strings.HasSuffix(r.URL.Path, "/internal/injection/"+grant.String()) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: "Bearer hop-secret"})
	}))
	srv.TLS = &tls.Config{MaxVersion: maxVersion}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})), overTLS
}

func startHopServer(t *testing.T, cpURL, cpCA, corpCA string, grant uuid.UUID) (*Server, error) {
	t.Helper()
	return startHopServerWith(t, cpURL, cpCA, corpCA, grant, &http.Client{Timeout: 5 * time.Second})
}

func startHopServerWith(t *testing.T, cpURL, cpCA, corpCA string, grant uuid.UUID, client *http.Client) (*Server, error) {
	t.Helper()
	cfg := &Config{
		RunID:             uuid.New(),
		ControlPlaneURL:   cpURL,
		ControlPlaneCAPEM: cpCA,
		RunToken:          "tok",
		Listen:            "127.0.0.1:0",
		Policy:            types.RunPolicySpec{AllowedDomains: []string{"example.com"}},
		Injection:         []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "example.com"}, GrantID: grant}},
		TrustedCAPEM:      corpCA,
	}
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := NewServer(context.Background(), cfg, client, &bytes.Buffer{})
	if err == nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
	}
	return srv, err
}

// The startup credential resolve rides TLS to a control plane whose cert the
// pinned CA signed.
func TestNewServer_ResolvesCredentialsOverPinnedTLS(t *testing.T) {
	grant := uuid.New()
	cp, cpCA, overTLS := hopCP(t, grant)
	if _, err := startHopServer(t, cp.URL, cpCA, "", grant); err != nil {
		t.Fatalf("pinned CA: the startup resolve must succeed, got %v", err)
	}
	if overTLS.Load() == 0 {
		t.Fatal("the control plane saw no TLS request — the resolve did not ride the pinned hop")
	}
}

// Fail closed on the wrong CA — even when the operator's corporate bundle (the
// EGRESS trust set) holds the control plane's real cert. That bundle must never
// widen what the control-plane hop trusts.
func TestNewServer_WrongControlPlaneCAFailsClosed(t *testing.T) {
	grant := uuid.New()
	cp, cpCA, _ := hopCP(t, grant)
	for name, corp := range map[string]string{"no corp CA": "", "corp CA holds the real cert": cpCA} {
		_, err := startHopServer(t, cp.URL, testCPCAPEM, corp, grant)
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("%s: a proxy given the wrong control-plane CA must refuse to start on x509; got %v", name, err)
		}
	}
}

// The brokered local routes (mint, approvals, uploads) ride the second
// control-plane transport; it is pinned the same way.
func TestLocalRoutes_ControlTransportIsPinned(t *testing.T) {
	grant := uuid.New()
	cp, cpCA, overTLS := hopCP(t, grant)
	srv, err := startHopServer(t, cp.URL, cpCA, "", grant)
	if err != nil {
		t.Fatal(err)
	}
	before := overTLS.Load()
	resp, err := srv.proxy.forwardToControlPlane(context.Background(), http.MethodPost, "/api/v1/internal/decisions", []byte("{}"), "application/json")
	if err != nil {
		t.Fatalf("pinned control transport: %v", err)
	}
	_ = resp.Body.Close()
	if overTLS.Load() == before {
		t.Fatal("the local-route forward did not reach the control plane over TLS")
	}

	wrong := newProxy(Options{ControlPlaneURL: cp.URL, RunToken: newTokenSource("tok"), Resolver: publicResolver{}})
	if wrong.controlTransport.TLSClientConfig.RootCAs == nil {
		t.Fatal("an unpinned control transport must hold an explicit empty pool; nil RootCAs means the system roots")
	}
	if _, err := wrong.forwardToControlPlane(context.Background(), http.MethodPost, "/api/v1/internal/decisions", []byte("{}"), "application/json"); err == nil {
		t.Fatal("an unpinned control transport must fail closed, not fall back to the system roots")
	}
}

// A nil client is not a way onto http.DefaultTransport: NewServer builds the
// pinned client itself, so the resolve still rides the internal CA and still
// fails closed on the wrong one.
func TestNewServer_NilClientIsPinned(t *testing.T) {
	grant := uuid.New()
	cp, cpCA, overTLS := hopCP(t, grant)
	if _, err := startHopServerWith(t, cp.URL, cpCA, "", grant, nil); err != nil {
		t.Fatalf("nil client, pinned CA: %v", err)
	}
	if overTLS.Load() == 0 {
		t.Fatal("the nil-client resolve did not ride TLS")
	}
	if _, err := startHopServerWith(t, cp.URL, testCPCAPEM, "", grant, nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("nil client, wrong CA: must fail closed on x509, got %v", err)
	}
}

// The proxy's floor is TLS 1.3 (hoptls.ClientConfig): a control plane that
// speaks at most TLS 1.2 is refused even with the right CA.
func TestNewServer_RefusesTLS12ControlPlane(t *testing.T) {
	grant := uuid.New()
	cp, cpCA, _ := hopCPMax(t, grant, tls.VersionTLS12)
	_, err := startHopServer(t, cp.URL, cpCA, "", grant)
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("a TLS 1.2-only control plane must be refused, got %v", err)
	}
}
