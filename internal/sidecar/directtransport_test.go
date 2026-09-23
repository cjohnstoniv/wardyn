// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sidecar

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// The upload target is the run's OWN wardyn-proxy — a known
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
// WHY THIS IS NOT AN END-TO-END PROXY TEST. It cannot be, in process:
// httpproxy's matcher exempts every LOOPBACK destination from proxying, and an
// httptest server is always 127.0.0.1, so no client configuration makes a local
// request proxied. A "does it reach the server" test therefore cannot tell
// Proxy: nil apart from DefaultTransport — measured: the first version of this
// file passed with Proxy: nil DROPPED. What discriminates is the transport's
// own proxy DECISION on a PRODUCTION-shaped, non-loopback URL, which is what
// this test drives, with a control proving the assertion is not vacuous.
func TestUploadClient_NeverProxiesTheControlPlanePUT(t *testing.T) {
	// The real thing: WARDYN_PROXY_URL's shape, which is never loopback.
	req, err := http.NewRequest(http.MethodPut,
		"http://wardyn-proxy:3128/wardyn/v1/scan-results/2f1c8d4e-0000-4000-8000-000000000001", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Owning a transport is half the property: a client with NO transport falls
	// back to http.DefaultTransport, whose Proxy IS ProxyFromEnvironment. This
	// assertion is what reds when Proxy: nil is dropped.
	c := uploadClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("upload client transport = %T, want its own *http.Transport — a nil transport inherits http.DefaultTransport, which proxies from the environment", c.Transport)
	}
	if tr.Proxy != nil {
		got, _ := tr.Proxy(req)
		t.Fatalf("upload transport decided to proxy %s via %v — a control-plane PUT to the run's own proxy must never be forwarded through another proxy", req.URL, got)
	}
	if c.Timeout == 0 {
		t.Fatal("upload client must keep its request timeout")
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
// performs a real PUT — and that a genuinely proxied client cannot, which is
// what makes "not proxied" an observable difference at all.
func TestUpload_StillPerformsARealPUT(t *testing.T) {
	var hit atomic.Bool
	var method atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		method.Store(r.Method)
		w.WriteHeader(http.StatusOK)
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
	if resp, cerr := ctrl.Get(srv.URL); cerr == nil {
		_ = resp.Body.Close()
		t.Fatal("control: a client proxied at 127.0.0.1:1 reached the server anyway")
	}

	if err := Upload(srv.URL, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Upload must still perform a real PUT after the transport swap: %v", err)
	}
	if !hit.Load() {
		t.Fatal("the server was never reached")
	}
	if m, _ := method.Load().(string); m != http.MethodPut {
		t.Fatalf("method = %q, want PUT", m)
	}
}
