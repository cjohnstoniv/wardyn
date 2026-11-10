// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// noNetworkProbeScript fetches a public URL twice: through the run's proxy, which with no_network must
// answer 403, and directly with no proxy, which the sandbox's network must refuse. wget's exit 127 is an
// image without it.
const noNetworkProbeScript = `command -v wget >/dev/null 2>&1 || { echo "wget=missing"; exit 0; }
out=$(http_proxy=http://wardyn-proxy:3128 wget -q -T 10 -O /dev/null http://example.com/ 2>&1); echo "proxied_rc=$?"
case "$out" in *403*) echo "proxied_refused=1" ;; *) echo "proxied_refused=0" ;; esac
env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY wget -q -T 5 -O /dev/null http://1.1.1.1/ >/dev/null 2>&1; echo "direct_rc=$?"
`

// testNoNetwork: a sandbox whose proxy config has no_network reaches nothing. A request through the
// proxy fails, refused 403 where the substrate runs the real proxy (Options.RealProxy), and a direct
// connect with no proxy fails.
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
	sb := createStrongestSandboxWith(t, ctx, r, caps, opts, "NoNetwork", func(spec *runner.SandboxSpec) {
		spec.ProxyConfig.NoNetwork = true
	})
	got := ExecKeyValues(t, ctx, r, sb.Ref, noNetworkProbeScript)
	if got["wget"] == "missing" {
		t.Skipf("sandbox image %q has no wget; the no-network case needs one: an image contract, not a substrate verdict", opts.image())
	}
	if got["proxied_rc"] == "" || got["proxied_rc"] == "0" {
		t.Errorf("through the proxy: rc %q, want it to fail", got["proxied_rc"])
	}
	if opts.RealProxy && got["proxied_refused"] != "1" {
		t.Errorf("through the real proxy: refused %q, want the proxy's 403", got["proxied_refused"])
	}
	if got["direct_rc"] == "" || got["direct_rc"] == "0" {
		t.Errorf("a direct connect: rc %q, want it to fail", got["direct_rc"])
	}
}
