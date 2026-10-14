// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/netip"
)

// hostGateway is the proxy's default hosts target for the control-plane name: Docker's host-gateway.
const hostGateway = "host-gateway"

var relayLoopback = netip.AddrFrom4([4]byte{127, 0, 0, 1})

// engineFacts is what serve and setup learn from the Docker engine before deciding where the run relay
// listens (relayPlan). Gathering them never fails on a missing host-side bridge: that is a fact
// (hostBridge false, a VM-backed engine), judged with any --relay-listen afterwards.
type engineFacts struct {
	os, name string // docker info's OperatingSystem and Name
	// desktop is Docker Desktop, told by its OperatingSystem: a WSL2 distribution may hold a docker0
	// of its own beside Docker Desktop, so the bridge test cannot tell it.
	desktop bool
	// gateway is the default bridge's gateway, what a native engine resolves host-gateway to.
	gateway netip.Addr
	// hostBridge reports that the default bridge's interface is on this host; bridge is what it holds.
	hostBridge bool
	bridge     []netip.Addr
}

// relayPlan decides where the run relay listens (bind) and what the proxy's hosts entry maps the
// control-plane name to (target), from the engine's facts and the --relay-listen and
// --relay-host-target values (zero and "" when unset).
func relayPlan(f engineFacts, explicitBind netip.Addr, explicitTarget string) (netip.Addr, string, error) {
	var bind netip.Addr
	switch {
	case f.desktop:
		bind = relayLoopback
	case f.hostBridge:
		bind = f.gateway
	default:
		// TODO(design): the VM-backed engine policy (Rancher Desktop, Colima and others) is being verified.
		return netip.Addr{}, "", fmt.Errorf("relay: Docker reports OperatingSystem %q, Name %q, and its bridge is not an interface of this host: "+
			"a VM-backed engine other than Docker Desktop is not supported yet (--relay-listen and --relay-host-target do not change this)", f.os, f.name)
	}
	if explicitBind.IsValid() {
		if err := explicitRelayBind(explicitBind, f); err != nil {
			return netip.Addr{}, "", err
		}
	} else if !f.desktop {
		if err := relayBind(bind, f.bridge); err != nil {
			return netip.Addr{}, "", err
		}
	}
	target := hostGateway
	if explicitTarget != "" {
		target = explicitTarget
	}
	return bind, target, nil
}

// checkRelayHostTarget admits a --relay-host-target value: host-gateway, or one literal IP address a
// container can be sent to (never unspecified, multicast or loopback, which is the container's own).
func checkRelayHostTarget(v string) error {
	if v == "" || v == hostGateway {
		return nil
	}
	ip, err := netip.ParseAddr(v)
	switch {
	case err != nil:
		return fmt.Errorf("--relay-host-target: %q is neither host-gateway nor an IP address", v)
	case ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback():
		return fmt.Errorf("--relay-host-target: refusing %s: it must be an address a container reaches this host at", v)
	}
	return nil
}

// probeRelay checks, from a throwaway container, that a proxy reaches a listener on bind through target.
// TODO(L9a): wire the Docker driver's ProbeRelay; until then nothing is probed.
var probeRelay = func(context.Context, netip.Addr, string) error { return nil }
