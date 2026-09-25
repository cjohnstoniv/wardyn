// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// NO_PROXY hardcoded the literal "wardyn-proxy" while WARDYN_PROXY_URL
// is operator-overridable (-proxy-url / WARDYN_PROXY_URL_OVERRIDE). With an
// override the sandbox's own HTTP_PROXY names a host that is NOT in its
// NO_PROXY, so anything inside the sandbox reaching the proxy's local
// /wardyn/... routes through a proxy-aware client tried to reach the proxy
// THROUGH the proxy — while "wardyn-proxy", a name that no longer resolves to
// anything, sat in the bypass list.
func TestBuildBaseSandboxEnv_NoProxyDerivesFromProxyURL(t *testing.T) {
	run := types.AgentRun{CreatedBy: "op", Agent: "claude-code"}

	cases := []struct {
		name, proxyURL, wantHost string
	}{
		{"default sidecar alias", "http://wardyn-proxy:3128", "wardyn-proxy"},
		{"operator override", "http://other-host:3128", "other-host"},
		{"no port", "http://proxy.corp.internal", "proxy.corp.internal"},
		{"ipv4 literal", "http://10.1.2.3:3128", "10.1.2.3"},
		{"ipv6 literal", "http://[2001:db8::1]:3128", "2001:db8::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := buildBaseSandboxEnv(run, c.proxyURL, nil)
			for _, key := range []string{"NO_PROXY", "no_proxy"} {
				got := env[key]
				entries := strings.Split(got, ",")
				if !slices.Contains(entries, c.wantHost) {
					t.Fatalf("%s = %q, want it to bypass the configured proxy host %q", key, got, c.wantHost)
				}
				// Loopback must survive: the sandbox's own local routes are
				// never proxied either.
				for _, must := range []string{"localhost", "127.0.0.1", "::1"} {
					if !slices.Contains(entries, must) {
						t.Fatalf("%s = %q, want it to keep %q", key, got, must)
					}
				}
			}
			if env["NO_PROXY"] != env["no_proxy"] {
				t.Fatalf("NO_PROXY (%q) and no_proxy (%q) must stay identical — curl honours only the lowercase form for plain http",
					env["NO_PROXY"], env["no_proxy"])
			}
		})
	}
}

// NEGATIVE CONTROL for on the DEFAULT proxy URL the value is
// byte-identical to what shipped, so no deployment's bypass list changes
// underneath it.
func TestBuildBaseSandboxEnv_NoProxyDefaultIsUnchanged(t *testing.T) {
	run := types.AgentRun{CreatedBy: "op", Agent: "claude-code"}
	env := buildBaseSandboxEnv(run, "http://wardyn-proxy:3128", nil)
	const want = "wardyn-proxy,localhost,127.0.0.1,::1"
	if env["NO_PROXY"] != want || env["no_proxy"] != want {
		t.Fatalf("default NO_PROXY = %q / %q, want %q unchanged", env["NO_PROXY"], env["no_proxy"], want)
	}
}

// An unparseable or host-less proxy URL must not produce an EMPTY first entry:
// a bypass list beginning with "," is a list the sandbox's HTTP clients read
// differently from each other. Fall back to the shipped default name.
func TestBuildBaseSandboxEnv_NoProxyFallsBackWhenProxyURLIsUnusable(t *testing.T) {
	run := types.AgentRun{CreatedBy: "op", Agent: "claude-code"}
	for _, bad := range []string{"", "://nonsense", "not a url at all"} {
		env := buildBaseSandboxEnv(run, bad, nil)
		got := env["NO_PROXY"]
		if strings.HasPrefix(got, ",") || strings.Contains(got, ",,") {
			t.Fatalf("proxyURL %q produced NO_PROXY %q with an empty entry", bad, got)
		}
		if !slices.Contains(strings.Split(got, ","), "wardyn-proxy") {
			t.Fatalf("proxyURL %q produced NO_PROXY %q, want the default sidecar alias as a fallback", bad, got)
		}
	}
}
