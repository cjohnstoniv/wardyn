// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"testing"
)

// The wardyn-aws-sso half — and the one place the answer is the
// OPPOSITE of the other two clients.
//
// The finding names three in-sandbox clients that used http.DefaultTransport:
// toolgate, sidecar and this binary. Two of them talk to the run's own
// wardyn-proxy and had to stop consulting $HTTP_PROXY. This binary has two
// destinations, and they pull apart:
//
//   - The CONTROL-PLANE call (the token-capture upload) is sidecar.Upload,
//     whose client is now Proxy: nil. That is where this binary's half of the
//     fix landed — there is no second control-plane client here to fix.
//   - The PORTAL reads go to portal.sso.<region>.amazonaws.com, which is
//     EXTERNAL. Giving that client Proxy: nil would take AWS traffic off the
//     sandbox's one governed route out, which is a policy hole, not a fix.
//
// So this test pins the asymmetry in the direction that could be silently
// broken by someone applying "Proxy: nil to the three clients" literally.
func TestPortalClient_StillTraversesTheEgressProxy(t *testing.T) {
	c := portalClient()
	if c.Transport != nil {
		t.Fatalf("portal transport = %T, want nil (http.DefaultTransport): an EXTERNAL AWS endpoint must traverse the sandbox's governed route out, not bypass it", c.Transport)
	}
	if c.Timeout == 0 {
		t.Fatal("portal client must keep its request timeout")
	}
	// DefaultTransport is what honours $HTTP_PROXY; assert the thing the nil
	// above actually buys, so a future change to net/http's defaults is visible
	// here rather than at a customer.
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("http.DefaultTransport no longer consults the proxy environment — the portal reads would silently stop traversing the egress proxy")
	}
}
