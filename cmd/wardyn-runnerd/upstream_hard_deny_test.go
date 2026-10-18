// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && k8s

package main

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
)

// TestUpstreamProxyInsideHardDenyRefused: an upstream proxy literal inside the hard deny's CIDRs or
// hosts refuses boot; one outside it, or a name, boots.
func TestUpstreamProxyInsideHardDenyRefused(t *testing.T) {
	h, err := k8s.NewHardDeny("cluster.local", []string{"10.96.10.20", "10.244.1.7"}, []string{"10.96.0.0/12", "172.20.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []string{"http://10.100.3.4:3128", "http://user:pw@172.20.0.1:8080", "http://10.244.1.7:3128", "http://[::ffff:10.96.0.9]:3128"} {
		if err := upstreamProxyOutsideHardDeny(refused, h); err == nil || !strings.Contains(err.Error(), "inside this runner's hard deny") {
			t.Errorf("%s: %v, want the hard-deny refusal", refused, err)
		}
		if err := upstreamProxyOutsideHardDeny(refused, h); err != nil && strings.Contains(err.Error(), "pw") {
			t.Errorf("the refusal repeats the credential: %v", err)
		}
	}
	for _, ok := range []string{"", "http://192.0.2.10:3128", "http://proxy.corp.example:3128", "http://10.112.0.1:3128"} {
		if err := upstreamProxyOutsideHardDeny(ok, h); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
}
