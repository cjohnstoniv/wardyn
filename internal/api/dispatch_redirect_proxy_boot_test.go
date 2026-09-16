// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDispatch_TokenBearingRedirect_ProxySidecarBoots (B10-F1) closes the one
// seam that let a producer/consumer contradiction ship: dispatch writes a
// token-bearing redirect's allowlist entry PORT-QUALIFIED ("artifactory.corp:443",
// F106) while the paired injection rule host is BARE ("artifactory.corp" — what
// buildInjector's exact-allowlist binding requires), and NO test ever fed one
// producer's output to the consumer. AllowedExactHost consulted only the
// port-less map, so buildInjector errored, NewServer errored, and
// cmd/wardyn-proxy/main.go exited 1 — every run on an estate with a corporate
// artifact mirror got a sidecar that died at boot.
//
// It drives the REAL wire: dispatch -> runner.BuildProxyConfig (the exact JSON
// every substrate hands the sidecar as WARDYN_PROXY_CONFIG_JSON) ->
// proxy.LoadConfigBytes -> proxy.NewServer. Anything short of that is what let
// the contradiction live: both halves had unit tests and both were right about
// their own half.
func TestDispatch_TokenBearingRedirect_ProxySidecarBoots(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{
		"npm-artifactory-token": []byte("s3cr3t-npm-token"),
	}}

	ctx := context.Background()
	if _, err := srv.cfg.Store.PutSiteConfig(ctx, types.SiteConfig{
		EgressRedirects: []types.EgressRedirect{
			{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm", TokenSecretRef: "npm-artifactory-token", Ecosystem: "npm"},
		},
	}); err != nil {
		t.Fatalf("seed site config: %v", err)
	}
	t.Cleanup(func() { _, _ = srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{}) })

	spec := dispatchAndCaptureSpec(t, srv, fr)

	// The producer half, asserted here so a failure below can never be blamed on
	// a redirect that simply did not apply.
	if len(spec.ProxyConfig.Injection) == 0 {
		t.Fatalf("no injection rule authored for the token-bearing redirect; ProxyConfig = %+v", spec.ProxyConfig)
	}

	// A control-plane stub standing in for wardynd's internal injection-resolve
	// route: NewServer mints every injection rule's secret ONCE at boot (fail
	// closed), so a sidecar that gets past the allowlist binding still has to
	// reach this.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: "Bearer s3cr3t-npm-token",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)

	if !spec.ProxyConfig.Injection[0].Rule.RequireTLS {
		t.Error("the https:// redirect's injection rule has require_tls=false (B10-F5)")
	}

	pc := spec.ProxyConfig
	pc.ControlPlaneURL = cp.URL
	raw, err := runner.BuildProxyConfig(spec.RunID, pc, 3128)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("LoadConfigBytes over the dispatched ProxyConfig: %v", err)
	}
	cfg.Listen = "127.0.0.1:0"

	psrv, err := proxy.NewServer(ctx, cfg, &http.Client{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatalf("proxy.NewServer over a token-bearing redirect's own dispatched config: %v\n"+
			"the sidecar of every run on an estate with a corporate artifact mirror dies at boot", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = psrv.Shutdown(sctx)
	})
}

// TestDispatch_CleartextRedirect_LeavesRequireTLSUnset is the negative control on
// the B10-F5 producer half: `require_tls` is derived from the transport the
// OPERATOR spelled, not stamped on unconditionally. A `to` of `http://…` is the
// operator asking for a plaintext connector, so the flag stays false and
// injectableTransport's port-80 arm keeps credentialing it exactly as before.
func TestDispatch_CleartextRedirect_LeavesRequireTLSUnset(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"npm-artifactory-token": []byte("s3cr3t-npm-token")}}

	if _, err := srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{
		EgressRedirects: []types.EgressRedirect{
			{From: "https://registry.npmjs.org/", To: "http://artifactory.corp/npm", TokenSecretRef: "npm-artifactory-token", Ecosystem: "npm"},
		},
	}); err != nil {
		t.Fatalf("seed site config: %v", err)
	}
	t.Cleanup(func() { _, _ = srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{}) })

	spec := dispatchAndCaptureSpec(t, srv, fr)
	if len(spec.ProxyConfig.Injection) == 0 {
		t.Fatalf("no injection rule authored; ProxyConfig = %+v", spec.ProxyConfig)
	}
	if spec.ProxyConfig.Injection[0].Rule.RequireTLS {
		t.Error("an http:// redirect's injection rule set require_tls; that refuses the plaintext " +
			"connector the operator asked for")
	}
}

// TestDispatch_RedirectMITMHostsCarryNoDuplicateBareHost is the PRODUCER-side
// guard the B10-F9 deferral rests on (R-02).
//
// The proxy keys mitmHosts/mitmPorts on the BARE host, so two entries for one
// host collapse to the last authored port and the other port silently tunnels
// opaque. Re-keying those maps on "host:port" is deferred — and the reason it is
// SAFE to defer is a producer invariant, not a proxy one: planArtifactRedirect
// dedupes by bare host (its seenHost map), so no dispatch can author the
// colliding shape. That invariant belongs here, over a real dispatched
// ProxyConfig, because a SECOND producer appending to plan.mitmHosts would break
// it while every proxy-side test kept passing.
func TestDispatch_RedirectMITMHostsCarryNoDuplicateBareHost(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"npm-artifactory-token": []byte("s3cr3t-npm-token")}}

	// Two redirects whose `to` is the SAME host on DIFFERENT ports, in two
	// ecosystems the run reaches — the exact shape that would author a colliding
	// pair if the dedupe were ever dropped.
	if _, err := srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{
		EgressRedirects: []types.EgressRedirect{
			{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm", TokenSecretRef: "npm-artifactory-token", Ecosystem: "npm"},
			{From: "https://pypi.org/", To: "https://artifactory.corp:8443/pypi", TokenSecretRef: "npm-artifactory-token", Ecosystem: "pip"},
		},
	}); err != nil {
		t.Fatalf("seed site config: %v", err)
	}
	t.Cleanup(func() { _, _ = srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{}) })

	spec := dispatchAndCaptureSpec(t, srv, fr)

	seen := map[string]string{}
	for _, entry := range spec.ProxyConfig.MITMHosts {
		bare := entry
		if h, _, err := net.SplitHostPort(entry); err == nil {
			bare = h
		}
		if prev, dup := seen[bare]; dup {
			t.Fatalf("ProxyConfig.MITMHosts carries %q and %q — two entries for one bare host. "+
				"The proxy keys mitmHosts/mitmPorts on the bare host, so the LAST one wins and the "+
				"other port tunnels opaque, never offered the operator's token. B10-F9's re-keying "+
				"is no longer safe to defer.", prev, entry)
		}
		seen[bare] = entry
	}
	if len(seen) == 0 {
		t.Fatal("no MITM host authored; the redirect never applied and this guard proved nothing")
	}
}
