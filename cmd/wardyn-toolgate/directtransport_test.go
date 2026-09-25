// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// -base defaults to $HTTP_PROXY — the gate's control plane IS the
// run's own wardyn-proxy — but the client used http.DefaultTransport, whose
// Proxy is ProxyFromEnvironment. With the default proxy URL those are the same
// address and the bug is invisible; under `--proxy-url http://<other-host>:3128`
// the sandbox's HTTP_PROXY names a DIFFERENT host, so every permission request
// was forwarded down the egress lane to it and every tool call denied. The git
// helper already set Proxy: nil for exactly this reason; toolgate and sidecar
// did not.
//
// Why this is not an end-to-end proxy test. It cannot be, in process:
// httpproxy's matcher exempts every LOOPBACK destination from proxying, and an
// httptest server is always 127.0.0.1, so no client configuration makes a local
// request proxied. A "does it reach the server" test therefore cannot tell
// Proxy: nil apart from DefaultTransport — measured: the first version of this
// file passed with Proxy: nil DROPPED. What discriminates is the transport's
// own proxy DECISION on a PRODUCTION-shaped, non-loopback URL, which is what
// this test drives, with a control proving the assertion is not vacuous.
func TestDirectClient_NeverProxiesTheControlPlaneCall(t *testing.T) {
	// The real thing: -base is $HTTP_PROXY, which is never loopback.
	req, err := http.NewRequest(http.MethodGet, "http://wardyn-proxy:3128/wardyn/v1/approvals", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Owning a transport is half the property: a client with NO transport falls
	// back to http.DefaultTransport, whose Proxy IS ProxyFromEnvironment. This
	// assertion is what reds when Proxy: nil is dropped.
	c := directClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("gate client transport = %T, want its own *http.Transport — a nil transport inherits http.DefaultTransport, which proxies from the environment", c.Transport)
	}
	if tr.Proxy != nil {
		got, _ := tr.Proxy(req)
		t.Fatalf("gate transport decided to proxy %s via %v — an approval POST to the run's own proxy must never be forwarded through another proxy", req.URL, got)
	}
	if c.Timeout == 0 {
		t.Fatal("gate client must keep its request timeout")
	}

	// CONTROL: the same request, through a transport shaped like the code
	// BEFORE this fix, really is proxied away to another host. Without this the
	// nil check above could pass for a URL nothing would ever proxy anyway.
	other, err := url.Parse("http://other-host:3128")
	if err != nil {
		t.Fatal(err)
	}
	ctrl := &http.Transport{Proxy: http.ProxyURL(other)}
	if got, _ := ctrl.Proxy(req); got == nil || got.Host != "other-host:3128" {
		t.Fatalf("control transport routed %s to %v, want other-host:3128 — the control is inert, so the assertion above proves nothing", req.URL, got)
	}
}

// Replacing http.DefaultTransport with a hand-built one is how a client quietly
// loses everything the default carried, so prove the swapped transport still
// performs a real request — and that a genuinely proxied client cannot, which
// is what makes "not proxied" an observable difference at all.
func TestDirectClient_StillPerformsARealRequest(t *testing.T) {
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusNotFound) // enough to prove the hop happened
	}))
	defer srv.Close()

	blackhole, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	// Nothing listens on the black hole, and http.ProxyURL is a FIXED proxy
	// func with no loopback exemption, so this client cannot reach the server.
	ctrl := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(blackhole)},
		Timeout:   5 * time.Second,
	}
	if resp, cerr := ctrl.Get(srv.URL + "/wardyn/v1/approvals"); cerr == nil {
		_ = resp.Body.Close()
		t.Fatal("control: a client proxied at 127.0.0.1:1 reached the server anyway")
	}

	resp, err := directClient().Get(srv.URL + "/wardyn/v1/approvals")
	if err != nil {
		t.Fatalf("the gate client must still perform a real request after the transport swap: %v", err)
	}
	_ = resp.Body.Close()
	if !hit.Load() {
		t.Fatal("the server was never reached")
	}
}
