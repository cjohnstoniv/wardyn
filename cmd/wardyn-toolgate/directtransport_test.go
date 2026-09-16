// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// B11a-F13. -base defaults to $HTTP_PROXY — the gate's control plane IS the
// run's own wardyn-proxy — but the client used http.DefaultTransport, whose
// Proxy is ProxyFromEnvironment. With the default proxy URL those are the same
// address and the bug is invisible; under `--proxy-url http://<other-host>:3128`
// the sandbox's HTTP_PROXY names a DIFFERENT host, so every permission request
// was forwarded down the egress lane to it and every tool call denied. The git
// helper already set Proxy: nil for exactly this reason; toolgate and sidecar
// did not.
//
// A nil Transport.Proxy is the whole property — net/http never proxies when it
// is nil — and asserting it structurally is deterministic, unlike an
// environment-driven test, because net/http captures the proxy environment ONCE
// per process.
func TestDirectClient_DoesNotConsultProxyEnvironment(t *testing.T) {
	c := directClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("gate transport = %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("gate transport has a Proxy func — approval POSTs to the run's own proxy must never be forwarded through another proxy")
	}
	if c.Timeout == 0 {
		t.Fatal("gate client must keep its request timeout")
	}
}

// End to end: a control-plane request reaches the server directly even when the
// environment names a proxy that would black-hole it. Self-validating — if
// net/http has already cached an empty proxy environment for this process the
// behavioural half cannot run, and the structural test above is the gate.
func TestDirectClient_ReachesLocalRouteDespiteProxyEnv(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNotFound) // enough to prove the hop happened
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

	resp, err := directClient().Get(srv.URL + "/wardyn/v1/approvals")
	if err != nil {
		t.Fatalf("the gate client must reach the local route directly, not through $HTTP_PROXY: %v", err)
	}
	_ = resp.Body.Close()
	if !hit {
		t.Fatal("the server was never reached — the request went through the environment proxy")
	}
}
