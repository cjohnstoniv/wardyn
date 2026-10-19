// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// localTrustedPeers resolves api.Config.LocalTrustedPeers, the peers
// -local-trust-forwarder trusts: WARDYN_LOCAL_TRUSTED_PEERS when set (IPs or
// CIDRs, comma-separated), else the addresses a loopback-published port's
// traffic reaches wardynd's container from. That is the default route's
// gateway (Docker; rootful Podman) and wardynd's own addresses (rootless
// Podman's port forwarder connects from inside the container's namespace).
func localTrustedPeers(raw string) ([]netip.Prefix, error) {
	if raw = strings.TrimSpace(raw); raw != "" {
		var out []netip.Prefix
		for _, e := range strings.Split(raw, ",") {
			p, err := parsePeer(strings.TrimSpace(e))
			if err != nil {
				return nil, fmt.Errorf("refusing to start: WARDYN_LOCAL_TRUSTED_PEERS entry %q: %w", e, err)
			}
			out = append(out, p)
		}
		return out, nil
	}
	var out []netip.Prefix
	if route, err := os.ReadFile("/proc/net/route"); err == nil {
		if gw, ok := defaultGateway(string(route)); ok {
			out = append(out, netip.PrefixFrom(gw, gw.BitLen()))
		}
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && !ip.Unmap().IsLoopback() && !ip.IsLinkLocalUnicast() {
				ip = ip.Unmap()
				out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("refusing to start: -local-trust-forwarder found no gateway or own address to trust; set WARDYN_LOCAL_TRUSTED_PEERS to the address the published port's traffic arrives from")
	}
	return out, nil
}

func parsePeer(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

// defaultGateway reads the IPv4 default route's gateway from a
// /proc/net/route table: destination 00000000 with the gateway flag (0x2) set.
// The gateway column is the address's bytes in host (little-endian) order.
func defaultGateway(table string) (netip.Addr, bool) {
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != "00000000" {
			continue
		}
		flags, ferr := strconv.ParseUint(f[3], 16, 32)
		gw, gerr := strconv.ParseUint(f[2], 16, 32)
		if ferr != nil || gerr != nil || flags&0x2 == 0 {
			continue
		}
		return netip.AddrFrom4([4]byte{byte(gw), byte(gw >> 8), byte(gw >> 16), byte(gw >> 24)}), true
	}
	return netip.Addr{}, false
}
