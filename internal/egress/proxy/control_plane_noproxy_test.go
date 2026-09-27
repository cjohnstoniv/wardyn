// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestControlPlaneClientNeverRidesTheCorpProxy: the run token, the
// minted credential VALUES, approvals and decision logs must never transit a
// corporate proxy.
//
// The owned transports set Proxy: nil for exactly that reason ("keeps the run
// token off the corp-proxy wire"), but the four control-plane clients NewServer
// wires — the decision sink, the injector, the approval client and the token
// renewer — rode the CALLER's http.Client with a nil Transport, i.e.
// http.DefaultTransport, i.e. ProxyFromEnvironment. And the one place that DID
// give them a transport (the corp-CA branch) built it with Transport.Clone(),
// which PRESERVES the proxy function. So with HTTP(S)_PROXY visible to the
// sidecar — dockerd-level proxy injection, a host-run or custom proxy image —
// every one of those calls went through the corporate proxy and skipped
// resolveTrustedURL's pin.
//
// The proxy function is swapped on http.DefaultTransport rather than driven from
// HTTP_PROXY alone because net/http captures the environment once per process:
// an env-only assertion silently passes on a process where something already
// read it. This drives the exact mechanism the finding names — Clone() carrying
// a non-nil Proxy — and is deterministic.
func TestControlPlaneClientNeverRidesTheCorpProxy(t *testing.T) {
	var cpHits, proxyHits atomic.Int64

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cpHits.Add(1)
		_, _ = io.WriteString(w, `{"header":"Authorization","value":"Bearer minted","expires_at":0}`)
	}))
	defer cp.Close()

	// Answers like a real forward proxy would (the CP response, relayed), so the
	// failure this test reports is "the credential took the corp hop", not a
	// decode error downstream of it.
	corp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		_, _ = io.WriteString(w, `{"header":"Authorization","value":"Bearer minted","expires_at":0}`)
	}))
	defer corp.Close()
	corpURL, err := url.Parse(corp.URL)
	if err != nil {
		t.Fatalf("parse corp proxy url: %v", err)
	}

	// The environment the sidecar can be handed, made deterministic.
	t.Setenv("HTTP_PROXY", corp.URL)
	t.Setenv("HTTPS_PROXY", corp.URL)
	orig := http.DefaultTransport
	envLike := orig.(*http.Transport).Clone()
	envLike.Proxy = func(*http.Request) (*url.URL, error) { return corpURL, nil }
	http.DefaultTransport = envLike
	t.Cleanup(func() { http.DefaultTransport = orig })

	cfg := &Config{
		RunID:           uuid.New(),
		ControlPlaneURL: cp.URL,
		RunToken:        "run-token",
		Listen:          "127.0.0.1:0",
		Policy:          types.RunPolicySpec{AllowedDomains: []string{"m.corp:443"}},
		Injection: []InjectionConfig{{
			InjectionRule: egress.InjectionRule{Host: "m.corp", Header: "Authorization", SecretName: "corp-token", Format: "Bearer %s"},
			GrantID:       uuid.New(),
		}},
	}
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	srv, err := NewServer(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, io.Discard)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	})

	// The mint (buildInjector) is the highest-value of the four: its response
	// body IS the operator's credential.
	if got := cpHits.Load(); got == 0 {
		t.Error("the control plane saw no injection resolve; the mint never reached it directly")
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("the corporate proxy saw %d control-plane request(s): the run token and the minted "+
			"credential value transited the corp-proxy wire", got)
	}

	// Structural, so a future caller that rebuilds one of these clients cannot
	// reintroduce the env proxy without this failing.
	for name, c := range map[string]*http.Client{
		"decision sink":   srv.sink.client,
		"injector":        srv.proxy.inject.client,
		"approval client": srv.proxy.approval.client,
	} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Errorf("%s: Transport is %T, want an owned *http.Transport (a nil one rides "+
				"http.DefaultTransport and therefore ProxyFromEnvironment)", name, c.Transport)
			continue
		}
		if tr.Proxy != nil {
			t.Errorf("%s: transport carries a Proxy function; control-plane traffic must never be proxied", name)
		}
	}
}

// TestControlPlaneClientWorksWithNoProxyEnv is the negative control: with no
// proxy in the environment the four clients reach the control plane exactly as
// before — the fix removes a hop, it does not add a requirement.
func TestControlPlaneClientWorksWithNoProxyEnv(t *testing.T) {
	var cpHits atomic.Int64
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cpHits.Add(1)
		_, _ = io.WriteString(w, `{"header":"Authorization","value":"Bearer minted","expires_at":0}`)
	}))
	defer cp.Close()

	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")

	cfg := &Config{
		RunID:           uuid.New(),
		ControlPlaneURL: cp.URL,
		RunToken:        "run-token",
		Listen:          "127.0.0.1:0",
		Policy:          types.RunPolicySpec{AllowedDomains: []string{"m.corp:443"}},
		Injection: []InjectionConfig{{
			InjectionRule: egress.InjectionRule{Host: "m.corp", Header: "Authorization", SecretName: "corp-token", Format: "Bearer %s"},
			GrantID:       uuid.New(),
		}},
	}
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := NewServer(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, io.Discard)
	if err != nil {
		t.Fatalf("NewServer with no proxy env: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	})
	if cpHits.Load() == 0 {
		t.Error("the control plane saw no injection resolve with no proxy env set")
	}
}
