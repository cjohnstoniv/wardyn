// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// noNetworkProbeScript fetches a public URL twice: through the run's proxy, which with no_network must
// refuse it though the host is allowlisted, and directly with no proxy, which the sandbox's network must
// refuse. The proxied request is also sent raw with nc, so the refusal's X-Wardyn-Egress-Reason header can
// be read: wget prints no headers on an error status, and a 403 alone could be another deny (a name the
// proxy cannot resolve is one).
const noNetworkProbeScript = `command -v wget >/dev/null 2>&1 || { echo "wget=missing"; exit 0; }
out=$(http_proxy=http://wardyn-proxy:3128 wget -q -T 10 -O /dev/null http://example.com/ 2>&1); echo "proxied_rc=$?"
if command -v nc >/dev/null 2>&1; then
  resp=$(printf 'GET http://example.com/ HTTP/1.0\r\nHost: example.com\r\n\r\n' | nc -w 10 wardyn-proxy 3128 2>&1)
  case "$resp" in *"X-Wardyn-Egress-Reason: policy:no-network"*) echo "proxied_reason=no-network" ;; *) echo "proxied_reason=other" ;; esac
else
  echo "proxied_reason=nc-missing"
fi
env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY wget -q -T 5 -O /dev/null http://1.1.1.1/ >/dev/null 2>&1; echo "direct_rc=$?"
`

// testNoNetwork: a sandbox whose proxy config has no_network reaches nothing. A request through the
// proxy fails, refused with the no_network reason where the substrate runs the real proxy
// (Options.RealProxy), and a direct connect with no proxy fails.
func testNoNetwork(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()
	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no confinement classes; the no-network case needs a sandbox", r.Name())
	}
	// example.com is on the allowlist, so an empty allowlist's default deny cannot answer for no_network, and
	// the reason header tells no_network from every other deny. The docker suites' stand-in proxy relays
	// nothing: there the case checks only that a spec carrying the switch creates and execs, and that
	// nothing gets out.
	sb := createStrongestSandboxWith(t, ctx, r, caps, opts, "NoNetwork", func(spec *runner.SandboxSpec) {
		spec.ProxyConfig.NoNetwork = true
		spec.ProxyConfig.Policy.AllowedDomains = []string{"example.com"}
	})
	got := ExecKeyValues(t, ctx, r, sb.Ref, noNetworkProbeScript)
	if got["wget"] == "missing" {
		if opts.RealProxy { // the suite owns its agent image, as with nc below
			t.Fatalf("sandbox image %q has no wget; the no-network case needs one", opts.image())
		}
		t.Skipf("sandbox image %q has no wget; the no-network case needs one: an image contract, not a substrate verdict", opts.image())
	}
	if got["proxied_rc"] == "" || got["proxied_rc"] == "0" {
		t.Errorf("through the proxy: rc %q, want it to fail", got["proxied_rc"])
	}
	if got["direct_rc"] == "" || got["direct_rc"] == "0" {
		t.Errorf("a direct connect: rc %q, want it to fail", got["direct_rc"])
	}
	// A real-proxy suite owns its agent image, which ships nc (AgentCannotReachAPIServer needs it too), so a
	// missing nc fails the case rather than skipping what it proves.
	if opts.RealProxy && got["proxied_reason"] != "no-network" {
		t.Errorf("through the real proxy: reason %q, want the refusal's X-Wardyn-Egress-Reason to be policy:no-network "+
			"(nc-missing: the image %q has no nc to read it with)", got["proxied_reason"], opts.image())
	}
}
