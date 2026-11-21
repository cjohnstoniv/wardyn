// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"slices"
	"testing"
)

// TestCreateSandbox_ProxyURLHostIsAHostAlias: a sandbox proxy URL that names another host is a host alias of the
// agent pod's proxy entry, lower-cased, beside wardyn-proxy: the pod's only nameserver answers no query.
func TestCreateSandbox_ProxyURLHostIsAHostAlias(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	spec := testSandboxSpec()
	spec.Env["WARDYN_PROXY_URL"] = "http://Proxy.Corp.Example:3128"
	createSandboxOn(t, d, cs, spec)
	pod := findPod(t, cs, agentPodName(spec.RunID))
	if ha := pod.Spec.HostAliases; len(ha) != 1 || !slices.Equal(ha[0].Hostnames, []string{"wardyn-proxy", "proxy.corp.example"}) {
		t.Fatalf("agent hostAliases = %+v, want one entry naming wardyn-proxy and proxy.corp.example", ha)
	}
}
