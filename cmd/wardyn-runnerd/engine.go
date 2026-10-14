// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

const (
	// hostGateway is Docker's ExtraHosts keyword for the engine's own route to this host.
	hostGateway = "host-gateway"
	// rancherWSLVeth is the interface Rancher Desktop on WSL2 adds to the namespace every distribution
	// shares; its address is the one route from that engine's containers to this distribution.
	rancherWSLVeth = "veth-rd-wsl"
	// relayAcceptWait is how long a probe listener waits for the connection a passing probe made.
	relayAcceptWait = 2 * time.Second
)

// relayAttempts are when each probe of one pair starts, from the moment its listener opened: Docker
// Desktop on WSL2 was seen to reach a fresh loopback listener only 0 to 3 seconds after it opened.
var relayAttempts = []time.Duration{0, 1500 * time.Millisecond, 3 * time.Second, 5 * time.Second}

var relayLoopback = netip.AddrFrom4([4]byte{127, 0, 0, 1})

// relayPair is where the run relay listens (bind) and what a run's proxy hosts file maps the
// control-plane name to (target: host-gateway or an IP) to reach it.
type relayPair struct {
	bind   netip.Addr
	target string
}

// dockerEngine is what serve and setup learn from the engine (inspectEngine). Docker Desktop is told by
// its OperatingSystem; otherwise an engine whose bridge interface is on this host is native, and any
// other is VM-backed, which is never an error in itself.
type dockerEngine struct {
	desc string // OperatingSystem and Name as Docker reports them, for messages
	why  string // why the engine is VM-backed
	// native engines keep their default bridge's gateway and the addresses its interface holds here.
	native  bool
	gateway netip.Addr
	bridge  []netip.Addr
	// veth is what an interface named rancherWSLVeth holds on this host.
	veth []netip.Addr
}

// relayCandidates is the ordered list of pairs to probe. A native engine has one: its bridge gateway,
// reached as host-gateway. A VM-backed engine's containers reach this host by whatever route its VM
// gives, so the list holds the known routes: host-gateway (Docker Desktop, Colima), Rancher Desktop's
// veth-rd-wsl address under WSL2, and the Lima (192.168.5.2) and gvisor-tap-vsock (192.168.127.254)
// gateways that forward to this host's loopback. An explicit bind or target (want) narrows the list and
// is never replaced: when no pair matches, the one pair is the explicit values over the first pair's.
func relayCandidates(eng dockerEngine, want relayPair) []relayPair {
	var all []relayPair
	if eng.native {
		all = []relayPair{{eng.gateway, hostGateway}}
	} else {
		all = []relayPair{{relayLoopback, hostGateway}}
		for _, v := range eng.veth {
			all = append(all, relayPair{v, v.String()})
		}
		all = append(all, relayPair{relayLoopback, "192.168.5.2"}, relayPair{relayLoopback, "192.168.127.254"})
	}
	var kept []relayPair
	for _, p := range all {
		if (!want.bind.IsValid() || p.bind == want.bind) && (want.target == "" || p.target == want.target) {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		kept = []relayPair{{cmp.Or(want.bind, all[0].bind), cmp.Or(want.target, all[0].target)}}
	}
	return kept
}

// admitRelayBind is the relay's security rule, the same on every engine: the relay hands whatever
// connects to it a stream into the organisation's internal hop, so it listens only where no other
// computer reaches it. Loopback, an address of veth-rd-wsl, or a native engine's bridge gateway held by
// that bridge; never an unspecified, multicast or LAN address. Whether a run's proxy reaches the address
// is the probe's question (findRelay), not this one's.
func admitRelayBind(addr netip.Addr, eng dockerEngine) error {
	a := addr.Unmap()
	switch {
	case a.IsUnspecified() || a.IsMulticast():
		return fmt.Errorf("relay: refusing to listen on %s: it must be one address of this host", addr)
	case a.IsLoopback(), slices.Contains(eng.veth, a):
		return nil
	case eng.native && a == eng.gateway:
		return relayBind(eng.gateway, eng.bridge)
	}
	return fmt.Errorf("relay: refusing to listen on %s: the relay listens only on loopback, the native Docker bridge's gateway, "+
		"or Rancher Desktop's %s address under WSL2, never an address another computer can reach", addr, rancherWSLVeth)
}

// parseRelayTarget admits a --relay-host-target value: host-gateway, or an IP a container is sent to.
// Never unspecified, multicast or loopback: inside the proxy, loopback is the proxy itself.
func parseRelayTarget(s string) (string, error) {
	if s == "" || s == hostGateway {
		return s, nil
	}
	ip, err := netip.ParseAddr(s)
	switch {
	case err != nil:
		return "", fmt.Errorf("--relay-host-target: %q is neither %s nor an IP address", s, hostGateway)
	case ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback():
		return "", fmt.Errorf("--relay-host-target: refusing %s: it must be an address a container reaches this host at", s)
	}
	return ip.String(), nil
}

// parseRelayWant reads --relay-listen and --relay-host-target ("" when unset) as the pair they narrow
// the search to. The bind is admitted separately, once the engine is known (admitRelayBind).
func parseRelayWant(listen, target string) (relayPair, error) {
	var want relayPair
	if listen != "" {
		var err error
		if want.bind, err = netip.ParseAddr(listen); err != nil {
			return relayPair{}, fmt.Errorf("--relay-listen: %w", err)
		}
	}
	var err error
	want.target, err = parseRelayTarget(target)
	return want, err
}

// relayProbe has one container on the engine dial target:port, as a run's proxy dials the relay.
type relayProbe func(ctx context.Context, target string, port uint16) error

// findRelay returns the first candidate pair a run's proxy reaches: for each, a throwaway listener on
// its bind, and probe dials it through its target. A pair passes only when the probe passes AND that
// listener accepted, so an answer from anything else at the target never counts. The refusal names the
// engine, every pair tried and the flags that change them.
func findRelay(ctx context.Context, probe relayProbe, eng dockerEngine, want relayPair) (relayPair, error) {
	var tried []string
	for _, p := range relayCandidates(eng, want) {
		err := tryRelayPair(ctx, probe, eng, p)
		if err == nil {
			return p, nil
		}
		tried = append(tried, fmt.Sprintf("listen %s, reach at %s: %v", p.bind, p.target, err))
	}
	kind, hint := "VM-backed: "+eng.why, ""
	if eng.native {
		kind = "native"
		hint = " A host firewall (firewalld, ufw) dropping container-to-host traffic does this; if DOCKER_HOST names another engine than this host's own dockerd, pass both flags."
	}
	return relayPair{}, fmt.Errorf("relay: no container on Docker engine %s (%s) reached this runner:\n  %s\n"+
		"Set --relay-listen (WARDYN_RELAY_LISTEN) and --relay-host-target (WARDYN_RELAY_HOST_TARGET); see docs/runner-registration.md#requirements.%s",
		eng.desc, kind, strings.Join(tried, "\n  "), hint)
}

// tryRelayPair keeps one listener on p's bind open and probes it through p's target at each of
// relayAttempts, until a probe passes and the listener accepted its connection.
func tryRelayPair(ctx context.Context, probe relayProbe, eng dockerEngine, p relayPair) error {
	if err := admitRelayBind(p.bind, eng); err != nil {
		return err
	}
	ln, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(netip.AddrPortFrom(p.bind, 0)))
	if err != nil {
		return err
	}
	defer ln.Close()
	opened, port := time.Now(), uint16(ln.Addr().(*net.TCPAddr).Port)
	for _, at := range relayAttempts {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(opened.Add(at))):
		}
		if err = probe(ctx, p.target, port); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("%w (%d attempts over %s)", err, len(relayAttempts), relayAttempts[len(relayAttempts)-1])
	}
	if err := ln.SetDeadline(time.Now().Add(relayAcceptWait)); err != nil {
		return err
	}
	c, err := ln.Accept()
	if err != nil {
		return errors.New("a container connected, but not to this runner's listener")
	}
	return c.Close()
}
