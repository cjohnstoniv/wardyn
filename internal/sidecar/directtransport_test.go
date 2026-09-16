// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sidecar

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// B11a-F13. The upload target is the run's OWN wardyn-proxy — a known
// on-segment address — yet this client used http.DefaultTransport, whose Proxy
// is ProxyFromEnvironment. Every sandbox carries HTTP_PROXY=$WARDYN_PROXY_URL,
// so with the DEFAULT proxy URL the control-plane PUT went to the proxy anyway
// and the bug was invisible; with `--proxy-url http://<other-host>:3128` the
// sandbox's HTTP_PROXY names a DIFFERENT host, and this PUT — the one that
// delivers a scan result or an SSO token capture — was forwarded down the
// egress lane to it instead of reaching the local brokered route. The git
// helper already set Proxy: nil for exactly this reason; sidecar and toolgate
// did not.
//
// A nil Transport.Proxy is the whole property (net/http never proxies when it
// is nil), and asserting it structurally is deterministic — unlike an
// environment-driven test, because net/http captures the proxy environment ONCE
// per process.
func TestUploadClient_DoesNotConsultProxyEnvironment(t *testing.T) {
	tr, ok := uploadClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("upload transport = %T, want *http.Transport", uploadClient().Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("upload transport has a Proxy func — a control-plane PUT to the run's own proxy must never be forwarded through another proxy")
	}
}

// End to end: the PUT reaches the server directly even when the environment
// names a proxy that would black-hole it. Self-validating — if net/http has
// already cached an empty proxy environment for this process the behavioural
// half cannot run, and the structural test above is the gate.
func TestUpload_ReachesLocalRouteDespiteProxyEnv(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// An address nothing listens on: a request that honoured it would fail.
	const blackhole = "http://127.0.0.1:1"
	t.Setenv("HTTP_PROXY", blackhole)
	t.Setenv("HTTPS_PROXY", blackhole)
	t.Setenv("http_proxy", blackhole)

	probeURL, _ := url.Parse(srv.URL)
	if p, err := http.ProxyFromEnvironment(&http.Request{URL: probeURL}); err != nil || p == nil {
		t.Skip("net/http already cached the proxy environment for this process; the structural test is the gate")
	}

	if err := Upload(srv.URL, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Upload must reach the local route directly, not through $HTTP_PROXY: %v", err)
	}
	if !hit {
		t.Fatal("the server was never reached — the PUT went through the environment proxy")
	}
}
