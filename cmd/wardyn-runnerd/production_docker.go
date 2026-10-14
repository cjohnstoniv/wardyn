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
	"os"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/keystore"
	"github.com/cjohnstoniv/wardyn/internal/runner/offline"
	"github.com/cjohnstoniv/wardyn/internal/runner/posture"
	"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"
	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
	want, err := parseRelayWant(opts.relayListen, opts.relayHostTarget)
	if err != nil {
		return runnerRuntime{}, err
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return runnerRuntime{}, err
	}
	defer cli.Close()
	eng, err := readyEngine(ctx, cli, "docker engine not reachable (set DOCKER_HOST to the engine's socket if it is not the default)", opts.network, want)
	if err != nil {
		return runnerRuntime{}, err
	}
	relay := newLocalRelay(ctx)
	sub, err := docker.New(docker.Config{RunnerID: identity.RunnerID.String(), ProxyImage: opts.proxyImage, InternalNetwork: opts.network, Record: record, RunnerStateDir: stateDir,
		ControlPlaneRelay: relay.Ensure})
	if err != nil {
		return runnerRuntime{}, err
	}
	// The runtime first: its startup containment pass stops lapsed runs even when the check below fails.
	rt, err := localRunnerRuntime(ctx, identity, stateDir, sub, docker.SandboxRef, relay, dockerCapacity)
	if err != nil {
		return runnerRuntime{}, err
	}
	pair, err := chooseRelay(ctx, sub, opts.proxyImage, eng, want)
	if err != nil {
		return runnerRuntime{}, errors.Join(err, rt.close())
	}
	relay.use(pair)
	slog.InfoContext(ctx, "run relay reachable", "engine", eng.desc, "listen", pair.bind, "target", pair.target)
	return rt, nil
}

// dockerCapacity is the Docker engine's CPUs and memory as the runner's advertised capacity, the size
// placement holds one run on this runner to, read again on every connect. An engine that does not answer
// within ten seconds leaves the capacity unreported (unknown, never zero) until the next connect.
func dockerCapacity(ctx context.Context) placement.Capacity {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := readDockerCapacity(ctx)
	if err != nil {
		slog.WarnContext(ctx, "runner capacity unknown until the next connect: Docker did not report its size", "error", err)
	}
	return c
}

// hostMeasure is what this computer has for runs, as Docker reports it: its CPUs and memory, and the
// free disk of Docker's data root when the engine is local and not Docker Desktop's VM. 0 is unknown.
func hostMeasure(ctx context.Context) (types.RunnerAmounts, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return types.RunnerAmounts{}, err
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return types.RunnerAmounts{}, err
	}
	host := os.Getenv("DOCKER_HOST")
	return docker.MeasureInfo(info.Info, host == "" || strings.HasPrefix(host, "unix://"), docker.FreeDiskMiB), nil
}

func readDockerCapacity(ctx context.Context) (placement.Capacity, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return placement.Capacity{}, err
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return placement.Capacity{}, err
	}
	return engineCapacity(info.Info.NCPU, info.Info.MemTotal), nil
}

// readyEngine is serve's and setup's look at the engine before the relay
// search: it answers (unreachable is the refusal when it does not), an
// explicit --relay-listen passes admitRelayBind before anything on the engine
// changes, and the proxy sidecars' bridge exists. Any failure exits serve;
// the service manager's restart policy is the retry.
func readyEngine(ctx context.Context, cli *client.Client, unreachable, network string, want relayPair) (dockerEngine, error) {
	eng, err := inspectEngine(ctx, cli, unreachable)
	if err != nil {
		return dockerEngine{}, err
	}
	if want.bind.IsValid() {
		if err := admitRelayBind(want.bind, eng); err != nil {
			return dockerEngine{}, err
		}
	}
	return eng, ensureInternalNetwork(ctx, cli, network)
}

// inspectEngine tells native from VM-backed. Docker Desktop is VM-backed by
// its OperatingSystem: a WSL2 distribution may hold a docker0 of its own
// beside it. Any other engine is native when its bridge interface is on this
// host (dockerHostGateway), and VM-backed when not, which is never an error.
// It also records Rancher Desktop's veth-rd-wsl addresses when this host has
// that interface.
func inspectEngine(ctx context.Context, cli *client.Client, unreachable string) (dockerEngine, error) {
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return dockerEngine{}, fmt.Errorf("%s; Rancher Desktop must use the dockerd (moby) engine; runnerd reads DOCKER_HOST, never a docker context: %w", unreachable, err)
	}
	eng := dockerEngine{desc: fmt.Sprintf("%q (%s)", info.Info.OperatingSystem, info.Info.Name)}
	if iface, err := net.InterfaceByName(rancherWSLVeth); err == nil {
		held, _ := ifaceAddrs(iface) // unreadable: no veth candidate, as if absent
		for _, a := range held {
			if a.Is4() {
				eng.veth = append(eng.veth, a)
			}
		}
	}
	if info.Info.OperatingSystem == "Docker Desktop" {
		eng.why = "Docker Desktop"
		return eng, nil
	}
	if eng.gateway, eng.bridge, err = dockerHostGateway(ctx, cli); err != nil {
		eng.gateway, eng.bridge, eng.why = netip.Addr{}, nil, err.Error()
		return eng, nil
	}
	eng.native = true
	return eng, nil
}

// relayCheckBound bounds pulling the proxy image and the relay search together.
const relayCheckBound = 2 * time.Minute

// chooseRelay pulls the proxy image when it is missing, so the probe runs the
// image runs will, then finds the relay pair (findRelay) with the driver's
// probe container. Both share one bound; the probe is never skipped.
func chooseRelay(ctx context.Context, d *docker.Driver, image string, eng dockerEngine, want relayPair) (relayPair, error) {
	ctx, cancel := context.WithTimeout(ctx, relayCheckBound)
	defer cancel()
	present, err := d.ImagePresent(ctx, image)
	if err == nil && !present {
		slog.InfoContext(ctx, "pulling proxy image "+image)
		err = d.EnsureProxyImage(ctx)
	}
	if err != nil {
		return relayPair{}, fmt.Errorf("relay: the proxy image %s is not on this engine and could not be pulled (a registry that needs a login: docker pull it as this user first): %w", image, err)
	}
	return findRelay(ctx, d.ProbeRelay, eng, want)
}

// newLocalRelay is the relay for a Docker runtime; only the docker build has
// one. It listens nowhere until use and a run's Ensure.
func newLocalRelay(ctx context.Context) *localRelay {
	return &localRelay{ctx: ctx, listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{}}
}

// ensureInternalNetwork creates the proxy sidecars' bridge when it is missing,
// as deploy/runner/install.sh does.
func ensureInternalNetwork(ctx context.Context, cli *client.Client, name string) error {
	_, err := cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	switch {
	case err == nil:
		return nil
	case !errdefs.IsNotFound(err):
		return fmt.Errorf("docker network %s: %w", name, err)
	}
	if _, err := cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		return fmt.Errorf("create docker network %s: %w", name, err)
	}
	slog.InfoContext(ctx, "created the Docker network for proxy sidecars", "network", name)
	return nil
}

// dockerHostGateway is the address a native engine resolves host-gateway to
// (the default bridge's gateway, unless the daemon sets host-gateway-ip, when
// the relay is simply unreachable) and the addresses that bridge's interface
// holds on this host. An error means the engine is not native (inspectEngine).
func dockerHostGateway(ctx context.Context, cli *client.Client) (netip.Addr, []netip.Addr, error) {
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
		return netip.Addr{}, nil, fmt.Errorf("its bridge interface is not on this host: %w", err)
	}
	held, err := ifaceAddrs(iface)
	if err != nil {
		return netip.Addr{}, nil, err
	}
	return gateway, held, nil
}

// ifaceAddrs is the IP addresses an interface holds.
func ifaceAddrs(iface *net.Interface) ([]netip.Addr, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var held []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				held = append(held, ip.Unmap())
			}
		}
	}
	return held, nil
}

// postureInterval is how often a connected runner re-reads its posture.
const postureInterval = 15 * time.Minute

func localRunnerRuntime(ctx context.Context, identity runneridentity.Identity, stateDir string, sub substrate.Substrate, ref func(uuid.UUID) string, relay *localRelay, capacity func(context.Context) placement.Capacity) (_ runnerRuntime, retErr error) {
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
		RunnerID: identity.RunnerID.String(), Substrate: sub, HoldsSandbox: manager.HoldsSandbox,
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
			// A host that cannot say its name reports none, and keeps the machine name it had.
			machine, _ := os.Hostname()
			data, err := runnerwire.Marshal(runnerwire.Caps{Support: support, Version: version.String(), MachineName: machine, Capacity: capacity(ctx)})
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
