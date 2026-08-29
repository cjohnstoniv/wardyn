// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApplyDefaultsAndValidate_TrustedCAPEM covers the "validate but do not
// retain" fail-fast check applyDefaultsAndValidate applies to trusted_ca_pem,
// the same shape as the upstream-proxy-URL check right above it.
func TestApplyDefaultsAndValidate_TrustedCAPEM(t *testing.T) {
	base := func(pem string) *Config {
		return &Config{
			RunID:           uuid.New(),
			ControlPlaneURL: "http://cp:8080",
			RunToken:        "tok",
			TrustedCAPEM:    pem,
		}
	}
	// Negative control: unset is a no-op — LoadConfigBytes-shaped configs
	// without this field must keep validating exactly as before.
	if err := base("").applyDefaultsAndValidate(); err != nil {
		t.Fatalf("empty trusted_ca_pem: %v", err)
	}
	corpCertPEM, _ := genTestCA(t)
	if err := base(string(corpCertPEM)).applyDefaultsAndValidate(); err != nil {
		t.Fatalf("valid trusted_ca_pem: %v", err)
	}
	err := base("not a pem certificate").applyDefaultsAndValidate()
	if err == nil {
		t.Fatal("garbage trusted_ca_pem: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "trusted_ca_pem") {
		t.Fatalf("error %q does not name trusted_ca_pem", err.Error())
	}
}

// TestLoadConfigBytes_RejectsGarbageTrustedCA drives the same check through
// the JSON entry point every substrate actually uses.
func TestLoadConfigBytes_RejectsGarbageTrustedCA(t *testing.T) {
	b := []byte(`{"run_id":"` + uuid.New().String() + `","control_plane_url":"http://cp:8080","run_token":"tok","trusted_ca_pem":"garbage"}`)
	if _, err := LoadConfigBytes(b); err == nil {
		t.Fatal("LoadConfigBytes(garbage trusted_ca_pem): want an error, got nil")
	}
}

// genLeafSignedBy issues a leaf certificate for dnsName, signed by caCert/caKey
// (as returned by genTestCA), reusing certAuthority.leafFor — the same minting
// code the live MITM path uses — and returns it ready for
// httptest.Server.TLS.Certificates.
func genLeafSignedBy(t *testing.T, caCertPEM, caKeyPEM []byte, dnsName string) tls.Certificate {
	t.Helper()
	ca, err := newCertAuthority(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	leaf, err := ca.leafFor(dnsName)
	if err != nil {
		t.Fatalf("leafFor: %v", err)
	}
	return *leaf
}

// genTrustPoolFor builds the *tls.Config trustedCATLSConfig-shaped code in
// server.go builds from a corp CA's cert PEM: system roots + that one cert.
func genTrustPoolFor(t *testing.T, corpCertPEM []byte) *tls.Config {
	t.Helper()
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(corpCertPEM) {
		t.Fatal("AppendCertsFromPEM failed")
	}
	return &tls.Config{RootCAs: pool}
}

// TestTrustedCADialsPassingCorpCA is the end-to-end proof: a run whose MITM
// path forwards to an upstream signed by a corporate CA succeeds ONLY when
// Options.TLSClientConfig trusts that CA — the exact config NewServer builds
// from Config.TrustedCAPEM. Mirrors mitm_test.go's mitmProxy/agentMITMConn
// shape, swapping the insecure test config for a real corp-CA-trusting one.
func TestTrustedCADialsPassingCorpCA(t *testing.T) {
	// The corp CA and a leaf for anthropicHost it signs — standing in for the
	// real upstream's certificate once a corporate TLS-inspecting middlebox
	// (or, here, just a corp-issued cert) sits on the forward path.
	corpCertPEM, corpKeyPEM := genTestCA(t)
	leaf := genLeafSignedBy(t, corpCertPEM, corpKeyPEM, anthropicHost)

	corpSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "corp-ok")
	}))
	corpSrv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	corpSrv.StartTLS()
	defer corpSrv.Close()

	// A SEPARATE CA for the AGENT-facing leg — this is the proxy's own MITM CA
	// (WARDYN_MITM_CA_PEM the sandbox trusts), unrelated to the corp CA under
	// test; every MITM test in this package mints one the same way.
	mitmCertPEM, mitmKeyPEM := genTestCA(t)
	mitmCA, err := newCertAuthority(mitmCertPEM, mitmKeyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}

	newTestProxyWithTLS := func(tlsCfg *tls.Config) (*Proxy, *bytes.Buffer) {
		buf := &bytes.Buffer{}
		sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 32)}
		p := newProxy(Options{
			RunID:           uuid.New(),
			Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost}}),
			Sink:            sink,
			CA:              mitmCA,
			MITMLLM:         true,
			Resolver:        publicResolver{},
			Dial:            redirectDial(upstreamAddr(corpSrv)),
			TLSClientConfig: tlsCfg,
		})
		return p, buf
	}

	t.Run("trusts the corp CA -> 200", func(t *testing.T) {
		p, _ := newTestProxyWithTLS(genTrustPoolFor(t, corpCertPEM))
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		tlsConn := agentMITMConn(t, proxySrv.URL, mitmCertPEM)
		defer tlsConn.Close()

		req, _ := http.NewRequest(http.MethodGet, "https://"+anthropicHost+"/v1/messages", nil)
		if err := req.Write(tlsConn); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(body) != "corp-ok" {
			t.Fatalf("status=%d body=%q, want 200 corp-ok (the corp-CA-signed upstream must be trusted)", resp.StatusCode, body)
		}
	})

	// Negative control: the WHOLE POINT. With no trusted CA configured
	// (nil == WARDYN_TRUSTED_CA_FILE unset), the SAME corp-CA-signed upstream
	// fails certificate verification and the proxy 502s with a
	// builtin:dial-failed deny recorded in the sink — never a silent allow.
	t.Run("nil config -> 502 + builtin:dial-failed", func(t *testing.T) {
		p, buf := newTestProxyWithTLS(nil)
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		tlsConn := agentMITMConn(t, proxySrv.URL, mitmCertPEM)
		defer tlsConn.Close()

		req, _ := http.NewRequest(http.MethodGet, "https://"+anthropicHost+"/v1/messages", nil)
		if err := req.Write(tlsConn); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (untrusted corp-CA-signed upstream must fail closed)", resp.StatusCode)
		}
		if !strings.Contains(buf.String(), "builtin:dial-failed") {
			t.Fatalf("decision sink = %q, want a builtin:dial-failed entry", buf.String())
		}
	})
}

// TestNewServer_WiresTrustedCAPEM proves the WIRING commit 2 adds: NewServer
// threads Config.TrustedCAPEM into the live proxy's forward transport.
func TestNewServer_WiresTrustedCAPEM(t *testing.T) {
	newSrv := func(t *testing.T, trustedCAPEM string) *Server {
		t.Helper()
		cfg := &Config{
			RunID:           uuid.New(),
			ControlPlaneURL: "http://127.0.0.1:1", // never dialed by this test
			RunToken:        "tok",
			Listen:          "127.0.0.1:0",
			Policy:          types.RunPolicySpec{AllowedDomains: []string{"example.com"}},
			TrustedCAPEM:    trustedCAPEM,
		}
		if err := cfg.applyDefaultsAndValidate(); err != nil {
			t.Fatalf("config: %v", err)
		}
		srv, err := NewServer(context.Background(), cfg, &http.Client{Timeout: time.Second}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		return srv
	}

	corpCertPEM, _ := genTestCA(t)
	set := newSrv(t, string(corpCertPEM))
	if set.proxy.transport.TLSClientConfig == nil || set.proxy.transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("NewServer did not wire Config.TrustedCAPEM into the forward transport's TLSClientConfig.RootCAs")
	}

	// Negative control: unset leaves the transport's TLSClientConfig nil —
	// byte-identical to a Config with no trusted_ca_pem key at all.
	unset := newSrv(t, "")
	if unset.proxy.transport.TLSClientConfig != nil {
		t.Fatalf("unset Config.TrustedCAPEM: TLSClientConfig = %#v, want nil", unset.proxy.transport.TLSClientConfig)
	}
}
