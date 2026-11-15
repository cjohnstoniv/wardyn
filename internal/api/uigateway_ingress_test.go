// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
)

// The browser's address and the sign-in a front proxy vouched for stay with the operator: no
// X-Forwarded-* name, X-Real-IP, or listed caller-address or access-proxy header reaches the sandbox,
// while the declared caller headers still do and an app's own header passes. REVERT PROOF: without
// uiStripIngressHeaders, X-Real-Ip and every vendor name below reach the app; stripping every X-
// header instead loses X-Wardyn-Caller and X-App-Trace.
func TestUIGateway_IngressHeadersNeverReachTheSandbox(t *testing.T) {
	sent := []string{"X-Real-Ip", "True-Client-Ip", "Cf-Connecting-Ip", "Cf-Connecting-Ipv6", "Fastly-Client-Ip", "X-Client-Ip",
		"X-Cluster-Client-Ip", "X-Original-Forwarded-For", "X-Azure-Clientip", "X-Azure-Socketip", "X-Envoy-External-Address",
		"X-Goog-Iap-Jwt-Assertion", "X-Forwarded-User", "X-Forwarded-Email", "X-Forwarded-Port", "X-Auth-Request-Email",
		"X-Ms-Client-Principal-Name", "X-Amzn-Oidc-Identity", "Cf-Access-Authenticated-User-Email", "X-Goog-Authenticated-User-Email"}
	h, seen := inboundHarness(t, "user:bob")
	rec := h.call("wdn_bob", func(r *http.Request) {
		for _, name := range sent {
			r.Header.Set(name, "203.0.113.42")
		}
		r.Header.Set("X-App-Trace", "kept")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("call = %d %s, want 200", rec.Code, rec.Body.String())
	}
	got := <-seen
	for _, name := range sent {
		if v := got.Values(name); len(v) != 0 {
			t.Errorf("%s reached the sandbox: %q", name, v)
		}
	}
	if got.Get("X-Wardyn-Caller") != "bob" || got.Get("X-Wardyn-Caller-Email") != "bob@example.com" {
		t.Errorf("caller headers = %q / %q, want bob", got.Get("X-Wardyn-Caller"), got.Get("X-Wardyn-Caller-Email"))
	}
	if got.Get("X-App-Trace") != "kept" {
		t.Errorf("an app's own header was lost: %v", got)
	}
}
