// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNewServerWiresEveryConfiguredControl is T-52 (#712): one config carrying
// EVERY optional control NewServer wires at once — a MITM CA, block-mode LLM
// content inspection with a workspace secret corpus, a credentialed upstream
// proxy with a bypass list, and a trusted corporate CA — proves LoadConfigBytes
// (the JSON entry point every substrate actually delivers WARDYN_PROXY_CONFIG_JSON
// through, not a Config struct literal) drives NewServer into wiring all of them
// together, not just each in isolation the way the rest of this package's tests
// do one control at a time.
func TestNewServerWiresEveryConfiguredControl(t *testing.T) {
	mitmCertPEM, mitmKeyPEM := genTestCA(t)
	corpCertPEM, _ := genTestCA(t)

	const (
		upstreamUser    = "corpuser"
		upstreamPass    = "corp-s3cret-passw0rd" // >= secretmask.MinLen, so it registers
		workspaceSecret = "workspace-secret-T52-9f3a7c1e"
	)

	build := func(t *testing.T, trustedCAPEM string) (*Config, error) {
		t.Helper()
		cfg := &Config{
			RunID:           uuid.New(),
			ControlPlaneURL: "http://127.0.0.1:1", // never dialed: no injection rules configured
			RunToken:        "tok",
			Listen:          "127.0.0.1:0",
			Policy: types.RunPolicySpec{
				AllowedDomains: []string{"example.com"},
				LLMInspection: &types.LLMInspectionSpec{
					Mode:                  "block",
					DetectSecrets:         true,
					WorkspaceSecretValues: []string{workspaceSecret},
				},
			},
			MITMCACertPEM:        string(mitmCertPEM),
			MITMCAKeyPEM:         string(mitmKeyPEM),
			UpstreamProxyURL:     "http://" + upstreamUser + ":" + upstreamPass + "@proxy.corp.example:8080",
			UpstreamProxyNoProxy: []string{"internal.example.com"},
			TrustedCAPEM:         trustedCAPEM,
		}
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		// The entry point every substrate actually uses (WARDYN_PROXY_CONFIG_JSON /
		// -config), not cfg.applyDefaultsAndValidate() directly — LoadConfigBytes'
		// own strict decode is part of what this test is proving still accepts
		// every one of these keys together.
		return LoadConfigBytes(b)
	}

	// Invalid PEM refused: the same fail-fast check TestLoadConfigBytes_RejectsGarbageTrustedCA
	// pins in isolation, run here beside every other control to prove it still
	// fires when the rest of the config is fully populated.
	t.Run("invalid trusted_ca_pem is refused", func(t *testing.T) {
		if _, err := build(t, "not a pem certificate"); err == nil {
			t.Fatal("LoadConfigBytes: want an error for a garbage trusted_ca_pem, got nil")
		} else if !strings.Contains(err.Error(), "trusted_ca_pem") {
			t.Fatalf("error %q does not name trusted_ca_pem", err.Error())
		}
	})

	loaded, err := build(t, string(corpCertPEM))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}

	srv, err := NewServer(context.Background(), loaded, &http.Client{Timeout: time.Second}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	if srv.proxy.ca == nil {
		t.Error("mitm_ca_cert_pem/mitm_ca_key_pem configured but Proxy.ca is nil")
	}
	if srv.proxy.scanner == nil {
		t.Error("llm_inspection{mode:block, detect_secrets, workspace_secret_values} configured but Proxy.scanner is nil")
	}
	if srv.proxy.transport == nil || srv.proxy.transport.TLSClientConfig == nil || srv.proxy.transport.TLSClientConfig.RootCAs == nil {
		t.Error("trusted_ca_pem configured but the forward transport does not trust it")
	} else {
		block, _ := pem.Decode(corpCertPEM)
		corpCert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("parse corp CA: %v", err)
		}
		if _, err := corpCert.Verify(x509.VerifyOptions{Roots: srv.proxy.transport.TLSClientConfig.RootCAs}); err != nil {
			t.Errorf("trusted_ca_pem configured but the forward transport's RootCAs do not trust the corp CA: %v", err)
		}
	}
	if len(srv.proxy.noProxy) == 0 {
		t.Error("upstream_proxy_no_proxy configured but Proxy.noProxy is empty")
	}

	// The mask registry: every rendering maskValues() registers for the
	// upstream-proxy credential (the wire base64 form and the password half
	// alone), plus each configured workspace secret, must be unreadable in any
	// decision-log/stdout line after NewServer has wired them in.
	decodedCred := upstreamUser + ":" + upstreamPass
	b64Cred := base64.StdEncoding.EncodeToString([]byte(decodedCred))
	line := []byte(`{"proxy_authorization":"Basic ` + b64Cred + `","upstream_pw":"` + upstreamPass +
		`","upstream_cred":"` + decodedCred + `","prompt":"leaked ` + workspaceSecret + ` here"}`)
	masked := maskDecisionBytes(line)
	for _, want := range []string{upstreamPass, b64Cred, decodedCred, workspaceSecret} {
		if bytes.Contains(masked, []byte(want)) {
			t.Errorf("mask registry did not mask %q: %s", want, masked)
		}
	}
	if !bytes.Contains(masked, []byte("<secret-hidden>")) {
		t.Fatalf("nothing was masked at all: %s", masked)
	}
}

// TestNewServer_NilClientStillPinnedAndProxyless is the nil-client pin T-52
// carries over from T-32 (#597): a nil *http.Client passed to NewServer must
// still end up PINNED (Proxy: nil) before it reaches the decision sink, the
// injector, the approval client and the token renewer.
//
// Before the fix (defaulting client BEFORE the "client != nil && Transport ==
// nil" pin check in server.go), a nil client sailed past that check untouched
// and was handed downstream as nil. Three of the four control-plane
// constructors (decision sink, injector, approval client) have their own
// "client == nil" fallback, but each builds a BARE &http.Client{} with no
// Transport override — riding http.DefaultTransport, which honours
// HTTP_PROXY/HTTPS_PROXY via ProxyFromEnvironment. The fourth, the token
// renewer, has NO such fallback at all: it calls client.Do on whatever it was
// handed, so a nil client reaching it is a startup goroutine panic.
//
// The pin is asserted on the clients actually handed downstream, not by routing
// a request: the fake control plane is on loopback, which ProxyFromEnvironment
// never proxies, so an unpinned client would reach it just the same. NewServer
// succeeding (the startup injection resolve plus the renewer running without a
// panic) covers the renewer, which keeps no client field to inspect.
func TestNewServer_NilClientStillPinnedAndProxyless(t *testing.T) {
	grant := uuid.New()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/internal/injection/"+grant.String()) {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: "Bearer tok"})
	}))
	defer cp.Close()

	cfg := &Config{
		RunID:           uuid.New(),
		ControlPlaneURL: cp.URL,
		RunToken:        "tok",
		Listen:          "127.0.0.1:0",
		Policy:          types.RunPolicySpec{AllowedDomains: []string{"example.com"}},
		Injection:       []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "example.com"}, GrantID: grant}},
	}
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	// client is nil — the one shape every OTHER NewServer test in this package
	// deliberately avoids, and the exact call shape a nil-client caller (present
	// or future) would make.
	srv, err := NewServer(context.Background(), cfg, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewServer with a nil client: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	for name, c := range map[string]*http.Client{
		"decision sink":   srv.sink.client,
		"injector":        srv.proxy.inject.client,
		"approval client": srv.proxy.approval.client,
		"approval reader": srv.proxy.inject.approvals.(httpApprovalReader).client,
	} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Errorf("%s: Transport is %T, want the pinned *http.Transport NewServer builds", name, c.Transport)
			continue
		}
		if tr.Proxy != nil {
			t.Errorf("%s: Transport.Proxy is set, want nil (pinned, never HTTP_PROXY/HTTPS_PROXY)", name)
		}
	}
}
