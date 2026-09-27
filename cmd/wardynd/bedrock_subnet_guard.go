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
// subnet — not even for an operator-declared internal host — so a
// WARDYN_BEDROCK_BASE_URL whose PrivateLink endpoint happens to resolve onto
// the subnet the proxy sidecar is itself attached to would have EVERY model
// call on this deployment denied, with the SDK misreading the proxy's denial
// page as a malformed Bedrock response. Better to refuse boot with a named
// reason than let every run discover this one dispatch at a time.
//
// Only ONE subnet is knowable at boot on any substrate today:
//
//   - docker: the fixed control-plane bridge network (WARDYN_INTERNAL_NETWORK,
//     "wardyn-internal" by default) that every wardyn-proxy sidecar joins
//     after its per-run network is created (driver_network.go NetworkConnect).
//     Looked up via the Docker API — controlPlaneNetworkSubnets, split by
//     build tag (bedrock_subnet_docker.go / bedrock_subnet_nodocker.go) the
//     same way envbuild is, so a tagless wardynd carries no docker-client
//     code (the parity rule). The per-run internal network is deliberately
//     NOT checked: Docker assigns its subnet fresh at CreateSandbox time, so
//     it cannot be known ahead of any run, and it is Internal=true
//     (gatewayless) — unroutable off-host regardless of what address lands
//     there.
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

	switch runnerTarget {
	case "docker":
		internalNetwork := dockerInternalNetworkName()
		subnets, ok := controlPlaneNetworkSubnets(ctx, internalNetwork)
		if !ok {
			slog.WarnContext(ctx, "wardynd: could not determine the docker control-plane network's subnet; skipping the WARDYN_BEDROCK_BASE_URL/proxy-subnet overlap check — its address is UNVERIFIED against the wardyn-proxy sidecar's own subnet",
				slog.String("network", internalNetwork))
			return nil
		}
		return refuseBedrockHostOnSubnets(bedrockBaseURL, host, internalNetwork, subnets)
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

// resolveBedrockHost is net.LookupIP behind a seam so tests can exercise the
// "unresolvable" path without depending on a real resolver. An IP literal
// host needs no lookup at all.
var resolveBedrockHost = net.LookupIP

// refuseBedrockHostOnSubnets is the pure decision: resolve host (an IP
// literal or a DNS name) to its address(es) and refuse, naming the variable,
// the resolved address and the subnet, the moment one falls inside subnets.
// An unresolvable host WARNs and returns nil — DNS not yet answering for a
// freshly-declared private endpoint is not grounds to refuse boot, and this
// same value is re-validated (fail closed) the next time it changes, at
// wardynd's next restart.
func refuseBedrockHostOnSubnets(bedrockBaseURL, host, networkName string, subnets []netip.Prefix) error {
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		ips, lerr := resolveBedrockHost(host)
		if lerr != nil {
			slog.Warn("wardynd: could not resolve WARDYN_BEDROCK_BASE_URL's host to check it against the docker control-plane network's subnet; boot proceeds with this UNVERIFIED",
				slog.String("bedrock_base_url", bedrockBaseURL), slog.String("host", host), slog.String("error", lerr.Error()))
			return nil
		}
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip); ok {
				addrs = append(addrs, a.Unmap())
			}
		}
	}

	for _, addr := range addrs {
		for _, n := range subnets {
			if n.Contains(addr) {
				return fmt.Errorf("WARDYN_BEDROCK_BASE_URL %q resolves to %s, which falls inside the "+
					"control-plane network %q's subnet %s — the per-run wardyn-proxy sidecar is itself "+
					"attached to that subnet, and its SSRF guard never lifts its own subnet "+
					"(onOwnSubnetOrControlPlane, internal/egress/proxy/egress_target.go), so every Bedrock "+
					"model call would be denied at the proxy",
					bedrockBaseURL, addr, networkName, n)
			}
		}
	}
	return nil
}
