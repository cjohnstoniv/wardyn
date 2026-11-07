// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

// A run's network carries no address on the Docker host.
//
// Internal=true takes away the run network's route off the host, but Docker still puts the network's
// gateway address on the host's side of the bridge, and a container on the network reaches whatever
// the host serves at that address. Gateway mode "isolated" on an internal network creates the bridge
// with no address at all. It needs Docker Engine 28.0.0 or later: the bridge's gateway modes are
// documented at https://docs.docker.com/engine/network/port-publishing/ ("isolated can only be used
// when the network is also created with --internal ... No address is assigned to the bridge"), and the
// 28.0.0 release notes (https://docs.docker.com/engine/release-notes/28/) add the mode. Engines before
// it refuse the mode, or, before 27.1, ignore the option and create a network that still has the
// address, so the engine's version is checked before the create instead of trusting the create to
// refuse. com.docker.network.bridge.inhibit_ipv4 also leaves the bridge without an address, but its
// page names no engine version, so no version gate could stand behind it.
//
// IPv6 stays off on the network (EnableIPv6 false, which wins over a daemon's default-network-opts):
// an IPv6 run network would put an IPv6 gateway address on the host's side of the bridge the same way.
const (
	bridgeGatewayModeIPv4 = "com.docker.network.bridge.gateway_mode_ipv4"
	gatewayModeIsolated   = "isolated"
	// minIsolatedEngine is the first Docker Engine release with gateway mode "isolated".
	minIsolatedEngine      = "28.0.0"
	minIsolatedEngineMajor = 28

	// EnvAllowHostReachableRunNetwork is Config.AllowHostReachableRunNetwork's setting.
	EnvAllowHostReachableRunNetwork = "WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK"
)

// AllowHostReachableRunNetworkFromEnv is Config.AllowHostReachableRunNetwork from
// EnvAllowHostReachableRunNetwork, off unless set; a value that is not a boolean exits 2 naming it.
func AllowHostReachableRunNetworkFromEnv() bool {
	on := cliutil.EnvBool(EnvAllowHostReachableRunNetwork, false)
	if on {
		slog.Warn("docker: "+EnvAllowHostReachableRunNetwork+" is set: on a Docker Engine older than "+minIsolatedEngine+
			", runs start on a network with an address on the Docker host, so their agent can reach services on the Docker host",
			EnvAllowHostReachableRunNetwork, on)
	}
	return on
}

// runNetworkOptions is the create request for a run's internal network: no IPv6, and, when isolated,
// gateway mode "isolated" so the bridge has no address on the Docker host.
func runNetworkOptions(labels map[string]string, isolated bool) client.NetworkCreateOptions {
	ipv6 := false
	opts := client.NetworkCreateOptions{Driver: "bridge", Internal: true, EnableIPv6: &ipv6, Labels: labels}
	if isolated {
		opts.Options = map[string]string{bridgeGatewayModeIPv4: gatewayModeIsolated}
	}
	return opts
}

// runNetworkIsolation reports whether this engine can create a run network with no address on the
// Docker host. An older or unreadable engine version refuses the run unless
// Config.AllowHostReachableRunNetwork is set, when the network is created without the mode.
func (d *Driver) runNetworkIsolation(ctx context.Context) (bool, error) {
	res, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return false, fmt.Errorf("docker: info: %w", err)
	}
	version := res.Info.ServerVersion
	if engineMajor(version) >= minIsolatedEngineMajor {
		return true, nil
	}
	if d.cfg.AllowHostReachableRunNetwork {
		slog.Warn("docker: creating a run network with an address on the Docker host, because "+EnvAllowHostReachableRunNetwork+" is set",
			"engine", version, "needs", minIsolatedEngine)
		return false, nil
	}
	return false, fmt.Errorf("docker: this Docker Engine is %q, and a run's network needs Docker Engine %s or later so that it has no address on the Docker host: "+
		"upgrade Docker Engine, or set %s=1 to start runs whose agent can reach services on the Docker host (docs/OPERATIONS.md, \"Docker host: run networks\")",
		version, minIsolatedEngine, EnvAllowHostReachableRunNetwork)
}

// engineMajor is the major release of a Docker Engine version such as "29.4.2" or "28.0.0-rc.1"; 0
// when it does not start with one.
func engineMajor(version string) int {
	major, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// runNetworkShapeProblem says why n, a run's network, may have an address on the Docker host, or ""
// when it was created with gateway mode "isolated" and without IPv6.
func runNetworkShapeProblem(n network.Inspect) string {
	switch {
	case n.EnableIPv6:
		return "has IPv6 on"
	case n.Options[bridgeGatewayModeIPv4] != gatewayModeIsolated:
		return "was created without gateway mode \"isolated\""
	}
	return ""
}

// checkRunNetwork refuses a run whose network may have an address on the Docker host, unless
// Config.AllowHostReachableRunNetwork is set; remedy is the refusal's advice. A network created before
// this check existed, or kept from before an upgrade, keeps the shape it was created with: Docker
// cannot change a network's gateway mode, and recreating it would cut off the containers already on
// it, so such a run is refused (remedyNewRun) and a new run gets a new network.
func (d *Driver) checkRunNetwork(ctx context.Context, name, remedy string) error {
	res, err := d.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return fmt.Errorf("docker: inspect the run's network %s: %w", name, err)
	}
	problem := runNetworkShapeProblem(res.Network)
	if problem == "" {
		return nil
	}
	if d.cfg.AllowHostReachableRunNetwork {
		slog.Warn("docker: using a run network that may have an address on the Docker host, because "+EnvAllowHostReachableRunNetwork+" is set",
			"network", name, "because", problem)
		return nil
	}
	return fmt.Errorf("docker: the run's network %s %s, so it may have an address on the Docker host, and its agent could reach services there: "+
		"%s, or set %s=1 to accept it (docs/OPERATIONS.md, \"Docker host: run networks\")",
		name, problem, remedy, EnvAllowHostReachableRunNetwork)
}

// The advice checkRunNetwork's refusal gives: for a network kept from before, and for one just created.
const (
	remedyNewRun = "start a new run, which gets a new network"
	remedyDaemon = "check that the bridge driver's default-network-opts in the Docker daemon's daemon.json do not turn IPv6 on"
)
