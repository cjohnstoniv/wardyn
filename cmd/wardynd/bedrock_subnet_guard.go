// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
)

// refuseBedrockOnProxySubnet (#1198) is the boot-time counterpart of the
// per-run wardyn-proxy's own SSRF clamp
// (onOwnSubnetOrControlPlane/liftInternalHost, internal/egress/proxy/
// egress_target.go): that clamp NEVER lifts an address on the proxy's own
// subnet — not even for an operator-declared internal host, and not only the
// control-plane network below: the proxy's OWN interfaces (localSubnets,
// internal/egress/proxy/proxy.go) also cover the per-run network it and the
// agent share for that one run — so a WARDYN_BEDROCK_BASE_URL whose
// PrivateLink endpoint happens to resolve onto whichever subnet a proxy
// sidecar is attached to would have EVERY model call on that run denied,
// with the SDK misreading the proxy's denial page as a malformed Bedrock
// response. Better to refuse boot with a named reason than let a run
// discover this at dispatch.
//
// Only ONE subnet is knowable and FIXED at boot on any substrate today — the
// docker control-plane network below — so that is the one this refuses over.
// The per-run network is a real instance of the SAME failure (see its own
// note below), just not one this boot can prove or refuse over; it gets a
// WARN instead.
//
//   - docker: the fixed control-plane bridge network (WARDYN_INTERNAL_NETWORK,
//     "wardyn-internal" by default) that every wardyn-proxy sidecar joins
//     after its per-run network is created (driver_network.go NetworkConnect).
//     Looked up via the Docker API — controlPlaneNetworkSubnets, split by
//     build tag (bedrock_subnet_docker.go / bedrock_subnet_nodocker.go) the
//     same way envbuild is, so a tagless wardynd carries no docker-client
//     code (the parity rule).
//
//     The per-run internal network each sandbox gets is NOT refused over,
//     but it is not exempt from the failure either: Docker allocates its
//     subnet fresh at CreateSandbox time from the daemon's default (or
//     operator-configured) address pools — cleared only of other local
//     networks, on-link routes and resolvers on THIS host, none of which
//     rules out a PrivateLink address reached through a routed VPC
//     peering/Transit Gateway. A run whose per-run network happens to draw
//     the same range as the endpoint sees the identical denial, just on
//     some runs and not others — an intermittent version of the exact bug
//     #1198 set out to remove, and one no boot-time check can predict (the
//     allocation happens per run, after boot). warnBedrockOnDockerDefaultPool
//     below WARNs, naming the remedy, when the resolved address falls in
//     Docker's own BUILT-IN default pools — the ones an unconfigured daemon
//     draws from — rather than refusing: an operator who has set
//     daemon.json's own default-address-pools has already moved off them.
//
//   - k8s: the pod CIDR the per-run proxy pod attaches to depends on the
//     cluster's CNI and is not reliably surfaced to a workload at boot without
//     extra node/API access this daemon is not guaranteed to hold. Rather than
//     guess, this logs a clear WARN and defers to docs/OPERATIONS.md "Bedrock
//     on a private endpoint" — a documented gap, not a silent one.
//
// Every other runnerTarget ("none", or a test-harness override naming neither)
// never dispatches a proxy sidecar at all, so there is nothing to check.
func refuseBedrockOnProxySubnet(ctx context.Context, bedrockBaseURL, runnerTarget string) error {
	if bedrockBaseURL == "" {
		return nil
	}
	host, err := bedrockBaseURLHost(bedrockBaseURL)
	if err != nil {
		// ValidateBedrockBaseURL (called just before this, in
		// validateModelEndpoints) already parsed and vetted this same URL —
		// unreachable in practice. Never block boot on a re-parse of an
		// already-validated value.
		return nil
	}

	// Resolved lazily, only for the ONE runnerTarget that actually checks an
	// address below: "k8s" WARNs unconditionally (it has no subnet to compare
	// against) and every other target no-ops, so neither needs a live DNS
	// lookup or resolver-failure WARN at all.
	switch runnerTarget {
	case "docker":
		addrs, resolved := bedrockResolvedAddrs(ctx, bedrockBaseURL, host)
		if !resolved {
			// bedrockResolvedAddrs already WARNed naming the host and the
			// error — DNS not yet answering for a freshly-declared private
			// endpoint is not grounds to refuse boot, and this same value is
			// re-validated (fail closed) the next time it changes, at
			// wardynd's next restart.
			return nil
		}
		warnBedrockOnDockerDefaultPool(ctx, bedrockBaseURL, addrs)
		internalNetwork := dockerInternalNetworkName()
		subnets, ok := controlPlaneNetworkSubnets(ctx, internalNetwork)
		if !ok {
			slog.WarnContext(ctx, "wardynd: could not determine the docker control-plane network's subnet; skipping the WARDYN_BEDROCK_BASE_URL/proxy-subnet overlap check — its address is UNVERIFIED against the wardyn-proxy sidecar's own subnet",
				slog.String("network", internalNetwork))
			return nil
		}
		return refuseBedrockAddrsOnSubnets(bedrockBaseURL, addrs, internalNetwork, subnets)
	case "k8s":
		slog.WarnContext(ctx, "wardynd: the per-run wardyn-proxy pod's CIDR is not known at wardynd's own "+
			"boot on Kubernetes, so WARDYN_BEDROCK_BASE_URL cannot be checked against it here — see "+
			"docs/OPERATIONS.md \"Bedrock on a private endpoint\". If the endpoint's resolved address falls "+
			"inside the cluster's pod CIDR, every Bedrock model call will be denied at the proxy the same "+
			"way a docker deployment would refuse to boot over it.")
		return nil
	default:
		return nil
	}
}

// dockerInternalNetworkName mirrors internal/runner/docker's own
// WARDYN_INTERNAL_NETWORK read + "wardyn-internal" default (register.go,
// Config.withDefaults) — read independently here because this boot check runs
// in cmd/wardynd, one layer above the substrate constructor that owns the
// authoritative copy.
func dockerInternalNetworkName() string {
	if n := os.Getenv("WARDYN_INTERNAL_NETWORK"); n != "" {
		return n
	}
	return "wardyn-internal"
}

// bedrockBaseURLHost extracts the host WARDYN_BEDROCK_BASE_URL's sandbox SDK
// actually dials, matching api.ValidateBedrockBaseURL's own url.Parse.
func bedrockBaseURLHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("empty host")
	}
	return host, nil
}

// resolveBedrockHost is net.DefaultResolver.LookupIP behind a seam so tests
// can exercise the "unresolvable" path without depending on a real resolver.
// Bound by ctx (bootCtx's 30s budget, main.go) rather than net.LookupIP's own
// unbounded default, so a dead resolver cannot stall boot past that budget.
// An IP literal host needs no lookup at all (bedrockResolvedAddrs below).
var resolveBedrockHost = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// bedrockResolvedAddrs resolves host (an IP literal needs no lookup) to its
// address(es), returning resolved=false — having already WARNed naming the
// host and the error — when DNS fails to answer. This is the ONE resolution
// both the control-plane-subnet refusal and the default-address-pool WARN
// below share, so a name that resolves differently between the two calls
// (a TTL expiring mid-boot) cannot happen.
func bedrockResolvedAddrs(ctx context.Context, bedrockBaseURL, host string) ([]netip.Addr, bool) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, true
	}
	ips, err := resolveBedrockHost(ctx, host)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not resolve WARDYN_BEDROCK_BASE_URL's host to check it against the docker control-plane network's subnet; boot proceeds with this UNVERIFIED",
			slog.String("bedrock_base_url", bedrockBaseURL), slog.String("host", host), slog.String("error", err.Error()))
		return nil, false
	}
	var addrs []netip.Addr
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip); ok {
			addrs = append(addrs, a.Unmap())
		}
	}
	return addrs, true
}

// refuseBedrockAddrsOnSubnets is the pure decision behind the control-plane
// refusal: the moment one of addrs falls inside subnets, refuse naming the
// variable, the resolved address, the subnet, why (the proxy's SSRF guard
// never lifts its own subnet) and the remedy (an endpoint outside that
// range, or re-address the control-plane network itself).
func refuseBedrockAddrsOnSubnets(bedrockBaseURL string, addrs []netip.Addr, networkName string, subnets []netip.Prefix) error {
	for _, addr := range addrs {
		for _, n := range subnets {
			if n.Contains(addr) {
				return fmt.Errorf("WARDYN_BEDROCK_BASE_URL %q resolves to %s, which falls inside the "+
					"control-plane network %q's subnet %s — the per-run wardyn-proxy sidecar is itself "+
					"attached to that subnet, and its SSRF guard never lifts its own subnet "+
					"(onOwnSubnetOrControlPlane, internal/egress/proxy/egress_target.go), so every Bedrock "+
					"model call would be denied at the proxy; point this at an endpoint outside that range, "+
					"or re-address the control-plane network (WARDYN_INTERNAL_NETWORK) away from it",
					bedrockBaseURL, addr, networkName, n)
			}
		}
	}
	return nil
}

// dockerDefaultAddressPool reports whether addr falls inside one of Docker's
// own BUILT-IN default-address-pools — 172.17.0.0/16 through 172.31.0.0/16,
// and 192.168.0.0/16 (moby/libnetwork's ipamutils predefined local-scope
// networks) — the set an UNCONFIGURED daemon draws a bridge network's subnet
// from, per-run networks included (driver_network.go's internalNetName, one
// per run). Membership here does not mean a run WILL collide (a specific
// run's draw is decided at CreateSandbox time, after boot, and is cleared of
// whatever is already in use) — only that it CAN, on a daemon whose operator
// has not moved default-address-pools elsewhere. An operator who HAS
// reconfigured it has already left this set entirely, which is exactly the
// remedy warnBedrockOnDockerDefaultPool names.
func dockerDefaultAddressPool(addr netip.Addr) (netip.Prefix, bool) {
	if p := netip.MustParsePrefix("192.168.0.0/16"); p.Contains(addr) {
		return p, true
	}
	for third := 17; third <= 31; third++ {
		p := netip.MustParsePrefix(fmt.Sprintf("172.%d.0.0/16", third))
		if p.Contains(addr) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// warnBedrockOnDockerDefaultPool is F1's WARN (never a refusal — the pools
// are daemon-configurable, so this is a possibility, not a fact this boot can
// disprove or refuse over): a resolved WARDYN_BEDROCK_BASE_URL address inside
// Docker's own default-address-pools may be handed to some FUTURE run's
// per-run network, which would then see the same denial the control-plane
// subnet check above refuses boot over — just intermittently, one run and not
// the next. Runs unconditionally (independent of whether the control-plane
// network's own subnet was resolvable at all): the two checks answer
// different questions and neither implies the other.
func warnBedrockOnDockerDefaultPool(ctx context.Context, bedrockBaseURL string, addrs []netip.Addr) {
	for _, addr := range addrs {
		pool, ok := dockerDefaultAddressPool(addr)
		if !ok {
			continue
		}
		slog.WarnContext(ctx, "wardynd: WARDYN_BEDROCK_BASE_URL resolves into one of Docker's own built-in "+
			"default-address-pools — a future run's per-run network could be allocated the same range and "+
			"collide with this endpoint the way the control-plane network can, denying that run's model calls "+
			"at the proxy while other runs are unaffected. Remedy: set default-address-pools in this daemon's "+
			"daemon.json away from this range (or use an endpoint outside it).",
			slog.String("bedrock_base_url", bedrockBaseURL),
			slog.String("resolved_address", addr.String()),
			slog.String("default_pool", pool.String()),
		)
	}
}
