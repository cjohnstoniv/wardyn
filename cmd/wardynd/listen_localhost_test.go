// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// "localhost" in any case is loopback with no lookup; "localhost." is a name like any other and goes to the resolver,
// which here knows nothing.
func TestListenHostIPsLocalhostAnyCase(t *testing.T) {
	stubListenLookup(t, map[string][]string{})
	for _, host := range []string{"localhost", "LOCALHOST", "LocalHost"} {
		ips, ok := listenHostIPs(host)
		if !ok || len(ips) != 1 || !ips[0].IsLoopback() {
			t.Errorf("listenHostIPs(%q) = %v, %v; want 127.0.0.1 without a lookup", host, ips, ok)
		}
	}
	if ips, ok := listenHostIPs("localhost."); ok {
		t.Errorf("listenHostIPs(\"localhost.\") = %v, true; want it sent to the resolver, which fails here", ips)
	}
}
