// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// An organisation older than this runner names a proxy image that refuses allowed_peers at config load.
// With peers-only on only by default the run's proxy is started again without them and serves any
// peer, as before peers-only, on create and on revive, with one warning and one report; with the switch
// set on the run is refused before its agent exists. A proxy that takes allowed_peers gets them, and
// nothing is reported.
func TestProxyPeersOnly_OlderOrganisation(t *testing.T) {
	const image = "wardyn-proxy:0.9.8"
	for _, tc := range []struct {
		name                string
		byDefault, olderOrg bool
		wantErr             string
	}{
		{name: "on by default, older organisation: serves any peer", byDefault: true, olderOrg: true},
		{name: "set on, older organisation: refused", olderOrg: true, wantErr: "the proxy image is older than this runner (it refused setting allowed_peers)"},
		{name: "on by default, organisation as new: peers only", byDefault: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			if tc.olderOrg {
				f.olderProxyRefuses = "allowed_peers"
			}
			var reported []string
			d := newWithClient(f, Config{ProxyImage: image, InternalNetwork: "wardyn-internal",
				ProxyPeersOnlyByDefault: tc.byDefault, PeersOnlyFallback: func(img string) { reported = append(reported, img) }})
			ctx := context.Background()
			spec := testSpec()
			name := proxyContainerName(spec.RunID)
			sb, err := d.CreateSandbox(ctx, spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("CreateSandbox = %v, want %q", err, tc.wantErr)
				}
				if slices.Contains(f.startedNames, agentContainerName(spec.RunID)) {
					t.Errorf("the agent started behind a proxy that refused allowed_peers (started: %v)", f.startedNames)
				}
				if len(reported) != 0 || strings.Contains(logs.String(), "peers-only not in effect") {
					t.Errorf("a refused run reported a fallback: %v\n%s", reported, logs.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			f.mu.Lock()
			subnet := f.subnets[internalNetName(spec.RunID)]
			f.mu.Unlock()
			wantPeers, wantReported := []netip.Prefix{subnet}, []string(nil)
			if tc.olderOrg {
				wantPeers, wantReported = nil, []string{image}
			}
			if got := allowedPeersOf(t, f.stdinOf(name)); !slices.Equal(got, wantPeers) {
				t.Errorf("created proxy allowed_peers = %v, want %v", got, wantPeers)
			}
			if err := d.StopProxy(ctx, sb.Ref); err != nil {
				t.Fatalf("StopProxy: %v", err)
			}
			if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
				t.Fatalf("ReplaceProxy: %v", err)
			}
			if got := allowedPeersOf(t, f.stdinOf(name)); !slices.Equal(got, wantPeers) {
				t.Errorf("revived proxy allowed_peers = %v, want %v", got, wantPeers)
			}
			if !slices.Equal(reported, wantReported) {
				t.Errorf("reported %q, want %q", reported, wantReported)
			}
			wantWarnings := len(wantReported)
			if n := strings.Count(logs.String(), "peers-only not in effect"); n != wantWarnings || (n > 0 && !strings.Contains(logs.String(), image)) {
				t.Errorf("%d warnings naming the image, want %d:\n%s", n, wantWarnings, logs.String())
			}
		})
	}
}

// WARDYN_DOCKER_PROXY_SANDBOX_PEERS_ONLY unset is peers-only by default, which falls back for an older
// organisation; any value set is the operator's, and an older organisation's run is refused.
func TestProxyPeersOnlyByDefaultFromEnv(t *testing.T) {
	for env, want := range map[string]bool{"": true, " ": true, "true": false, "1": false, "false": false} {
		t.Setenv(EnvProxySandboxPeersOnly, env)
		if got := ProxyPeersOnlyByDefaultFromEnv(); got != want {
			t.Errorf("%s=%q: ProxyPeersOnlyByDefaultFromEnv = %v, want %v", EnvProxySandboxPeersOnly, env, got, want)
		}
	}
}
