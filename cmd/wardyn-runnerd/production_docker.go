// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/keystore"
	"github.com/cjohnstoniv/wardyn/internal/runner/offline"
	"github.com/cjohnstoniv/wardyn/internal/runner/posture"
	"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"
	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/google/uuid"
)

func productionRunnerHooks(ctx context.Context, identity runneridentity.Identity, stateDir string, opts serveOptions) (runnerRuntime, error) {
	if opts.proxyImage == "" {
		return runnerRuntime{}, errors.New("runner serve requires --proxy-image or WARDYN_PROXY_IMAGE")
	}
	record, err := opts.recordingEnabled()
	if err != nil {
		return runnerRuntime{}, err
	}
	gateway, bridge, err := dockerHostGateway(ctx)
	if err != nil {
		slog.WarnContext(ctx, "runner relay unavailable: every run create is refused until it can listen", "error", err)
	}
	relay := newLocalRelay(ctx, gateway, bridge, err)
	sub, err := docker.New(docker.Config{ProxyImage: opts.proxyImage, InternalNetwork: opts.network, Record: record, RunnerStateDir: stateDir,
		ControlPlaneRelay: relay.Ensure})
	if err != nil {
		return runnerRuntime{}, err
	}
	return localRunnerRuntime(ctx, identity, stateDir, sub, docker.SandboxRef, relay)
}

// dockerHostGateway is the address a native engine resolves host-gateway to
// (the default bridge's gateway, unless the daemon sets host-gateway-ip, when
// the relay is simply unreachable) and the addresses that bridge's interface
// holds on this host. Docker Desktop's host-gateway is its VM's host address,
// not an interface here, so it has no relay.
func dockerHostGateway(ctx context.Context) (netip.Addr, []netip.Addr, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return netip.Addr{}, nil, err
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return netip.Addr{}, nil, err
	}
	if info.Info.OperatingSystem == "Docker Desktop" {
		return netip.Addr{}, nil, errors.New("relay: Docker Desktop's host-gateway is its VM's host address, not an interface of this host")
	}
	insp, err := cli.NetworkInspect(ctx, "bridge", client.NetworkInspectOptions{})
	if err != nil {
		return netip.Addr{}, nil, err
	}
	var gateway netip.Addr
	for _, c := range insp.Network.IPAM.Config {
		if c.Gateway.Is4() {
			gateway = c.Gateway
			break
		}
	}
	iface, err := net.InterfaceByName(insp.Network.Options["com.docker.network.bridge.name"])
	if err != nil {
		return netip.Addr{}, nil, fmt.Errorf("relay: the Docker bridge interface: %w", err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Addr{}, nil, err
	}
	var held []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				held = append(held, ip.Unmap())
			}
		}
	}
	return gateway, held, nil
}

// postureInterval is how often a connected runner re-reads its posture.
const postureInterval = 15 * time.Minute

func localRunnerRuntime(ctx context.Context, identity runneridentity.Identity, stateDir string, sub substrate.Substrate, ref func(uuid.UUID) string, relay *localRelay) (_ runnerRuntime, retErr error) {
	streams, err := runnerio.New(sub, identity.RunnerID.String(), stateDir)
	if err != nil {
		return runnerRuntime{}, err
	}
	manager, err := offline.New(stateDir, identity.RunnerID, sub, ref, streams.OutputSettled, streams.EraseOutput)
	if err != nil {
		return runnerRuntime{}, err
	}
	streams.HoldSandboxes(manager.HoldsSandbox)
	manager.ReleaseSettledOutput(streams.ReleaseOutput)
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, manager.Close())
		}
	}()
	resident, err := runneragent.NewResident(runneragent.Keystore{Set: keystore.Set, Get: keystore.Get, Delete: keystore.Delete}, stateDir)
	if err != nil {
		return runnerRuntime{}, err
	}
	reporter := posture.NewReporter()
	dispatcher, err := runneragent.New(runneragent.Config{
		RunnerID: identity.RunnerID.String(), Substrate: sub,
		BeforeCreate: manager.BeforeCreate, Created: manager.Created, CreateFailed: manager.CreateFailed,
		BeforeRefCall: manager.BeforeRefCall, AfterRefCall: manager.AfterRefCall,
		MutationFinished: manager.MutationFinished,
		ExecutionResult:  manager.ExecutionResult,
		Streams:          streams.Call, Output: streams.Output, BindOutput: streams.BindOutput, ForgetOutput: streams.ForgetOutput,
		Resident: resident,
	})
	if err != nil {
		return runnerRuntime{}, err
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = manager.CheckLapses(startup)
	cancel()
	if err != nil {
		// Run retries transient containment errors. BeforeReady refuses
		// admission until required containment actually succeeds.
		slog.WarnContext(ctx, "runner startup containment will retry", "error", err)
	}
	hooks := runnerwire.RunnerHooks{
		Call: dispatcher.Call,
		// The org never opens a stream toward a runner: a relay is the
		// runner's, opened outbound by localRelay.
		Open: func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error {
			return runnerwire.Refuse("unsupported runner stream")
		},
		Clock: manager.Clock, Pending: manager.Pending, StatePages: manager.StatePages,
		BeforeReady: manager.BeforeReady, Lease: manager.Lease, Revoked: manager.Revoked, Offline: manager.Offline,
		Replay: func(ctx context.Context, peer *runnerwire.Peer) error {
			support, err := sub.Classes(ctx)
			if err != nil {
				return err
			}
			data, err := runnerwire.Marshal(runnerwire.Caps{Support: support, Version: version.String()})
			if err != nil {
				return err
			}
			if err := peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
				return err
			}
			// Posture only gates runner_resident delivery; a refused report
			// must not keep the runner from READY.
			if err := reporter.Send(ctx, peer); err != nil {
				slog.WarnContext(ctx, "runner posture report failed", "error", err)
			}
			return manager.Replay(ctx, peer)
		},
		Online: func(peer *runnerwire.Peer, _ bool) {
			manager.Online(peer)
			streams.Online(peer)
			relay.Online(peer)
			go reporter.Watch(peer, postureInterval)
		},
		StreamOffline: func(peer *runnerwire.Peer) {
			streams.Offline(peer)
			relay.Offline(peer)
		},
	}
	run := func(ctx context.Context) error {
		go resident.SweepExpired(ctx, time.Minute)
		return manager.Run(ctx)
	}
	return runnerRuntime{hooks: hooks, run: run, close: func() error { return errors.Join(manager.Close(), relay.Close()) }}, nil
}
