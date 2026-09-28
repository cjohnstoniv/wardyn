// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// TestRefuseBedrockAddrsOnSubnets (#1198): the pure decision behind the
// control-plane-subnet refusal — given a resolved subnet list, an address
// inside one is refused naming the variable/address/subnet/remedy, and an
// address outside every subnet is fine.
func TestRefuseBedrockAddrsOnSubnets(t *testing.T) {
	cpNet := netip.MustParsePrefix("172.30.0.0/16")

	for name, c := range map[string]struct {
		addrs   []netip.Addr
		wantErr bool
		wantMsg []string // substrings the error must contain
	}{
		"address inside the subnet is refused": {
			addrs:   []netip.Addr{netip.MustParseAddr("172.30.5.5")},
			wantErr: true,
			wantMsg: []string{"172.30.5.5", `"wardyn-internal"`, "172.30.0.0/16", "never lifts its own subnet", "point this at an endpoint outside"},
		},
		"address outside every subnet is fine": {
			addrs: []netip.Addr{netip.MustParseAddr("10.0.0.5")},
		},
		"no addresses is fine": {},
	} {
		t.Run(name, func(t *testing.T) {
			err := refuseBedrockAddrsOnSubnets("https://vpce.example.com", c.addrs, "wardyn-internal", []netip.Prefix{cpNet})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			for _, want := range c.wantMsg {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not contain %q", err, want)
				}
			}
		})
	}
}

// TestBedrockResolvedAddrs (#1198): the shared resolution step — an IP
// literal needs no lookup, a resolved hostname is converted, and an
// unresolvable hostname WARNs and reports resolved=false rather than
// refusing boot over a resolver hiccup. Also pins F4: the resolver is called
// with the CALLER's ctx (bootCtx's 30s budget), not an unbounded one of its
// own.
func TestBedrockResolvedAddrs(t *testing.T) {
	for name, c := range map[string]struct {
		host       string
		resolved   []net.IP
		resolveErr error
		wantOK     bool
		wantAddrs  []netip.Addr
	}{
		"IP literal needs no resolver": {
			host:      "172.30.5.5",
			wantOK:    true,
			wantAddrs: []netip.Addr{netip.MustParseAddr("172.30.5.5")},
		},
		"resolved hostname": {
			host:      "bedrock-vpce.example.com",
			resolved:  []net.IP{net.ParseIP("100.64.1.1")},
			wantOK:    true,
			wantAddrs: []netip.Addr{netip.MustParseAddr("100.64.1.1")},
		},
		"unresolvable hostname WARNs and reports not-ok": {
			host:       "does-not-resolve.example.com",
			resolveErr: errors.New("no such host"),
			wantOK:     false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			prevResolve := resolveBedrockHost
			var gotCtx context.Context
			var resolverCalled bool
			resolveBedrockHost = func(ctx context.Context, host string) ([]net.IP, error) {
				resolverCalled = true
				gotCtx = ctx
				return c.resolved, c.resolveErr
			}
			t.Cleanup(func() { resolveBedrockHost = prevResolve })

			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			ctx := context.Background()
			addrs, ok := bedrockResolvedAddrs(ctx, "https://"+c.host, c.host)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (len(addrs) != len(c.wantAddrs) || addrs[0] != c.wantAddrs[0]) {
				t.Errorf("addrs = %v, want %v", addrs, c.wantAddrs)
			}
			if c.resolveErr != nil {
				if !strings.Contains(buf.String(), "could not resolve") {
					t.Errorf("expected a WARN naming the unresolvable host; log = %s", buf.String())
				}
			}
			if !resolverCalled {
				return // an IP literal is expected to skip the resolver entirely
			}
			if gotCtx != ctx {
				t.Error("resolveBedrockHost was not called with the caller's ctx (F4: bootCtx's timeout must bound it)")
			}
		})
	}
}

// TestResolveBedrockHost_HonoursContext (#1198 F4): exercises the REAL
// (unswapped) resolveBedrockHost — TestBedrockResolvedAddrs above substitutes
// it entirely, so it cannot catch a regression to the production body itself
// (e.g. reverting to net.LookupIP, or plumbing a fresh context.Background()
// through instead of the caller's ctx). An already-cancelled ctx must fail
// FAST with ctx's own error; net.LookupIP (no context parameter at all) or a
// discarded ctx would instead either hang or fail on a real, uncancelled
// lookup — this box's DNS behaviour, not ctx's cancellation.
func TestResolveBedrockHost_HonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := resolveBedrockHost(ctx, "example.com")
	if err == nil {
		t.Fatal("resolveBedrockHost with an already-cancelled ctx: got nil error, want one naming the cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("resolveBedrockHost error = %v, want it to wrap context.Canceled (proves ctx, not an internal timeout, aborted it)", err)
	}
}

// TestDockerDefaultAddressPool (#1198 F1): the static membership check behind
// the default-address-pool WARN — inside one of Docker's own built-in pools
// (172.17-172.31/16, 192.168.0.0/16) is a hit; a routable public address, and
// an RFC1918 range docker's pools do NOT include (172.16.0.0/16, which sits
// just below the pool range, and 10.0.0.0/8), are not.
func TestDockerDefaultAddressPool(t *testing.T) {
	for name, c := range map[string]struct {
		addr   string
		wantOK bool
	}{
		"172.17.0.0/16 (first pool /16)":          {addr: "172.17.0.1", wantOK: true},
		"172.31.255.254 (last pool /16)":          {addr: "172.31.255.254", wantOK: true},
		"192.168.0.0/16":                          {addr: "192.168.50.50", wantOK: true},
		"172.16.0.0/16 is NOT a default pool":     {addr: "172.16.0.1", wantOK: false},
		"172.32.0.0 is outside the pool range":    {addr: "172.32.0.1", wantOK: false},
		"10.0.0.0/8 is not a docker default pool": {addr: "10.0.0.1", wantOK: false},
		"a public address is not a pool member":   {addr: "8.8.8.8", wantOK: false},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := dockerDefaultAddressPool(netip.MustParseAddr(c.addr))
			if ok != c.wantOK {
				t.Errorf("dockerDefaultAddressPool(%s) ok = %v, want %v", c.addr, ok, c.wantOK)
			}
		})
	}
}

// TestWarnBedrockOnDockerDefaultPool (#1198 F1): an address inside a default
// pool WARNs naming the remedy; one outside logs nothing.
func TestWarnBedrockOnDockerDefaultPool(t *testing.T) {
	for name, c := range map[string]struct {
		addr     string
		wantWarn bool
	}{
		"inside a default pool WARNs": {addr: "172.20.5.5", wantWarn: true},
		"outside every pool is quiet": {addr: "10.0.0.5", wantWarn: false},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			warnBedrockOnDockerDefaultPool(context.Background(), "https://vpce.example.com", []netip.Addr{netip.MustParseAddr(c.addr)})

			warned := strings.Contains(buf.String(), "default-address-pools")
			if warned != c.wantWarn {
				t.Errorf("warned = %v, want %v; log = %s", warned, c.wantWarn, buf.String())
			}
		})
	}
}

// TestRefuseBedrockOnProxySubnet_Skips (#1198): the substrate-dispatch
// wrapper never even looks at a substrate when there is nothing configured to
// check, and it WARNs (never refuses) on the substrates whose subnet it
// cannot know from cmd/wardynd's own boot alone. The "docker" runnerTarget
// itself is NOT exercised here (F3): controlPlaneNetworkSubnets differs by
// build tag — a real daemon dial under -tags docker, an always-unknown stub
// otherwise (bedrock_subnet_nodocker.go) — so a docker-target case here would
// be non-hermetic under -tags docker (it would hit whatever daemon and DNS
// answer this host happens to have). That coverage lives in
// bedrock_subnet_docker_test.go, where newNetworkInspector is faked
// identically under either build tag.
func TestRefuseBedrockOnProxySubnet_Skips(t *testing.T) {
	for name, c := range map[string]struct {
		bedrockBaseURL string
		runnerTarget   string
		wantWarn       string
	}{
		"empty base URL: no-op, no warning": {
			bedrockBaseURL: "",
			runnerTarget:   "docker",
		},
		"none runner: nothing dispatches a proxy, no warning": {
			bedrockBaseURL: "https://vpce-bedrock.example.com",
			runnerTarget:   "none",
		},
		"k8s runner: documented gap, WARNs": {
			bedrockBaseURL: "https://172.20.1.1",
			runnerTarget:   "k8s",
			wantWarn:       "pod's CIDR is not known",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			err := refuseBedrockOnProxySubnet(context.Background(), c.bedrockBaseURL, c.runnerTarget)
			if err != nil {
				t.Fatalf("refuseBedrockOnProxySubnet: unexpected refusal: %v", err)
			}
			if c.wantWarn != "" && !strings.Contains(buf.String(), c.wantWarn) {
				t.Errorf("expected log to contain %q; log = %s", c.wantWarn, buf.String())
			}
			if c.wantWarn == "" && buf.Len() != 0 {
				t.Errorf("expected no log output; got %s", buf.String())
			}
		})
	}
}

// TestBedrockBaseURLHost covers the host-extraction helper directly, since
// refuseBedrockOnProxySubnet silently no-ops on a parse failure (unreachable
// via a validated WARDYN_BEDROCK_BASE_URL, but worth pinning on its own).
func TestBedrockBaseURLHost(t *testing.T) {
	if h, err := bedrockBaseURLHost("https://vpce-bedrock.example.com/prefix"); err != nil || h != "vpce-bedrock.example.com" {
		t.Errorf("got (%q, %v)", h, err)
	}
	if _, err := bedrockBaseURLHost("https:///no-host"); err == nil {
		t.Error("expected an error for an empty host")
	}
}
