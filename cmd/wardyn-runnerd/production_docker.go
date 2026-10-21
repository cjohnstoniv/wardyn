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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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
	switch opts.substrate {
	case "k8s":
		return k8sRunnerHooks(ctx, identity, stateDir, opts)
	case "folders":
		return foldersRunnerHooks(ctx, identity, stateDir, opts)
	}
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
		return runnerRuntime{}, atStage(runnerwire.StageEngine, err)
	}
	relay := newLocalRelay(ctx)
	// The decision sink before the runtime: its startup containment stops the runs' proxies, and one
	// still running delivers what it holds while it stops only if the sink already listens.
	sink, err := newDockerSink(ctx, stateDir, eng)
	if err != nil {
		return runnerRuntime{}, fmt.Errorf("decision sink did not start: %w", err)
	}
	ev := &evidenceForwarder{}
	sub, err := docker.New(docker.Config{RunnerID: identity.RunnerID.String(), ProxyImage: opts.proxyImage, InternalNetwork: opts.network, Record: record,
		ControlPlaneRelay: relay.Ensure, TrustedCAPEM: opts.trustedCA, TrustedCAFile: opts.trustedCAPath, UpstreamProxyURL: opts.upstreamProxy,
		DecisionSink: sinkAdapter{s: sink.spoolSink, url: sink.proxyURL}, Evidence: ev,
		ProxySandboxPeersOnly: cliutil.EnvBool(docker.EnvProxySandboxPeersOnly, false)})
	if err != nil {
		return runnerRuntime{}, errors.Join(err, sink.Close())
	}
	local, err := opts.localAttachEnabled()
	if err != nil {
		return runnerRuntime{}, errors.Join(err, sink.Close())
	}
	extras := sink.extras(sub, ev)
	extras.linuxVM = enableLinuxVM(ctx, sub, stateDir, os.Getenv)
	extras.localAttach, extras.orgURL = local, opts.orgURL
	// The runtime first: its startup containment pass stops lapsed runs even when the check below fails.
	extras.lastFailure = opts.status.failure
	rt, err := localRunnerRuntime(ctx, identity, stateDir, sub, docker.SandboxRef, relay, dockerCapacity, extras)
	if err != nil {
		return runnerRuntime{}, errors.Join(err, sink.Close())
	}
	rt.hooks.ProxyImage = mirroredProxyImageHook(opts.proxyImageMirror, opts.proxyImagePinned, followOrgProxyImage(opts.proxyImage, opts.proxyImagePinned, sub.SetProxyImage))
	pair, err := chooseRelay(ctx, sub, opts.proxyImage, eng, want)
	if err != nil {
		return runnerRuntime{}, errors.Join(err, rt.close())
	}
	opts.status.relayChanged(true, "")
	if err := sink.use(pair.bind); err != nil {
		return runnerRuntime{}, errors.Join(fmt.Errorf("decision sink: %w", err), rt.close())
	}
	relay.use(pair)
	slog.InfoContext(ctx, "run relay reachable", "engine", eng.desc, "listen", pair.bind, "target", pair.target)
	rt.run = watchRelay(rt.run, relayWatch{
		check: func(ctx context.Context) error { return tryRelayPair(ctx, sub.ProbeRelay, eng, pair) }, every: relayRecheckEvery,
		proves: func(err error) bool { return errors.Is(err, docker.ErrRelayUnreachable) },
		changed: func(ctx context.Context, reachable bool, err error) {
			logRelayChange(ctx, reachable, err, pair)
			reason := ""
			if err != nil {
				reason = err.Error()
			}
			opts.status.relayChanged(reachable, reason)
		},
		send: rt.refreshCaps}, relay.sessionUp)
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
// free disk of Docker's data root when the engine is not VM-backed (a VM's disk is measured by
// measureVMDisk). 0 is unknown.
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
	return docker.MeasureInfo(info.Info, os.Getenv("DOCKER_HOST"), docker.FreeDiskMiB), nil
}

// dockerArch is the Docker engine's CPU architecture, "amd64" or "arm64", or "" when the engine does
// not answer within ten seconds or reports another: the runner then reports none, and the control plane
// keeps the one it had.
func dockerArch(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return ""
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return ""
	}
	return dockerutil.NormalizeArch(info.Info.Architecture)
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
// the service manager's restart policy is the retry. An engine that accepts
// the connection and never answers fails at engineCheckBound, so serve exits
// with a log line instead of waiting, silent, for ever.
func readyEngine(ctx context.Context, cli *client.Client, unreachable, network string, want relayPair) (dockerEngine, error) {
	ctx, cancel := context.WithTimeout(ctx, engineCheckBound)
	defer cancel()
	eng, err := admittedEngine(ctx, cli, unreachable, want)
	if err != nil {
		return dockerEngine{}, err
	}
	return eng, ensureInternalNetwork(ctx, cli, network)
}

// admittedEngine is readyEngine without its one write: the engine answers and the explicit relay bind, if
// any, is one the relay may listen on. doctor stops here.
func admittedEngine(ctx context.Context, cli *client.Client, unreachable string, want relayPair) (dockerEngine, error) {
	eng, err := inspectEngine(ctx, cli, unreachable)
	if err != nil {
		return dockerEngine{}, err
	}
	if want.bind.IsValid() {
		if err := admitRelayBind(want.bind, eng); err != nil {
			return dockerEngine{}, err
		}
	}
	return eng, nil
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
		return dockerEngine{}, fmt.Errorf("%s; Rancher Desktop must use the dockerd (moby) engine; runnerd follows DOCKER_HOST, else a docker context that is a local unix socket: %w", unreachable, err)
	}
	eng := dockerEngine{desc: fmt.Sprintf("%q (%s)", info.Info.OperatingSystem, info.Info.Name), version: info.Info.ServerVersion}
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

// engineCheckBound bounds readyEngine's calls to the engine.
var engineCheckBound = 30 * time.Second

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
		return relayPair{}, atStage(runnerwire.StageImage, fmt.Errorf("relay: the proxy image %s is not on this engine and could not be pulled (a registry that needs a login: docker pull it as this user first): %w", image, err))
	}
	pair, err := findRelay(ctx, d.ProbeRelay, eng, want)
	return pair, atStage(runnerwire.StageRelay, err)
}

// newLocalRelay is the relay for a Docker runtime; only the docker build has
// one. It listens nowhere until use and a run's Ensure.
func newLocalRelay(ctx context.Context) *localRelay {
	return &localRelay{ctx: ctx, listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{}, sessionUp: make(chan struct{}, 1)}
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

// runRelay is the relay a runtime drives: the Docker localRelay or the k8s k8sRelay.
type runRelay interface {
	Online(*runnerwire.Peer)
	Offline(*runnerwire.Peer)
	Close() error
}

// runtimeExtras are the substrate-specific parts of a runtime. Each zero field takes the default its
// comment names.
type runtimeExtras struct {
	// keystore holds runner_resident values; zero is the OS keyring.
	keystore runneragent.Keystore
	// noPosture: no posture reporter (k8s, design K1 §3.3).
	noPosture bool
	// arch is the reported architecture; "" reads the engine's.
	arch string
	// lastFailure is the failure the caps event reports; nil reports none.
	lastFailure func() *runnerwire.Failure
	// options are the manager's substrate options; started runs once the manager exists, before
	// the startup containment pass (the k8s runner's fence replay and first proxy list).
	options offline.Options
	started func(context.Context, *offline.Manager) error
	// closeAlso runs when the runtime closes (the decision sink).
	closeAlso func() error
	// linuxVM, set when the operator turned Linux VM sandboxes on and the probe passed, is what the
	// runner's caps offer; nil offers none and refuses create_vm.
	linuxVM *runnerwire.VMCaps
	// localAttach opens the local attach socket while the organisation allows it (--local-attach on,
	// Docker only); orgURL is the organisation its pointer file names.
	localAttach bool
	orgURL      string
}

func localRunnerRuntime(ctx context.Context, identity runneridentity.Identity, stateDir string, sub substrate.Substrate, ref func(uuid.UUID) string, relay *localRelay, capacity func(context.Context) placement.Capacity, extra runtimeExtras) (runnerRuntime, error) {
	// A re-registered computer starts clean: the earlier registration's runs are contained, then set aside.
	if err := offline.SetAsidePrevious(ctx, stateDir, identity.RunnerID, sub, ref); err != nil {
		return runnerRuntime{}, err
	}
	return runnerRuntimeWith(ctx, identity, stateDir, sub, ref, relay, capacity, extra)
}

func runnerRuntimeWith(ctx context.Context, identity runneridentity.Identity, stateDir string, sub substrate.Substrate, ref func(uuid.UUID) string, relay runRelay, capacity func(context.Context) placement.Capacity, extra runtimeExtras) (_ runnerRuntime, retErr error) {
	streams, err := runnerio.New(sub, identity.RunnerID.String(), stateDir)
	if err != nil {
		return runnerRuntime{}, err
	}
	manager, err := offline.New(stateDir, identity.RunnerID, sub, ref, streams.OutputSettled, streams.EraseOutput)
	if err != nil {
		return runnerRuntime{}, err
	}
	streams.HoldSandboxes(manager.HoldsSandbox)
	streams.AttachRuns(manager.AttachRef)
	local, err := newLocalSocket(extra.localAttach, identity.RunnerID.String(), extra.orgURL, stateDir, streams.ServeLocal)
	if err != nil {
		return runnerRuntime{}, errors.Join(err, manager.Close())
	}
	manager.ReleaseSettledOutput(streams.ReleaseOutput)
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, manager.Close())
		}
	}()
	ks := extra.keystore
	if ks.Set == nil {
		store := keystore.For(identity.RunnerID.String())
		ks = runneragent.Keystore{Set: store.Set, Get: store.Get, Delete: store.Delete, DeleteAll: store.DeleteAll, Precheck: store.Precheck}
	}
	resident, err := runneragent.NewResident(ks, stateDir, manager.AdmitResident, time.Now)
	if err != nil {
		return runnerRuntime{}, err
	}
	var reporter *posture.Reporter
	if !extra.noPosture {
		reporter = posture.NewReporter()
	}
	if err := manager.SetOptions(extra.options); err != nil {
		return runnerRuntime{}, err
	}
	if extra.started != nil {
		if err := extra.started(ctx, manager); err != nil {
			return runnerRuntime{}, err
		}
	}
	dispatcher, err := runneragent.New(runneragent.Config{
		RunnerID: identity.RunnerID.String(), Substrate: sub, HoldsSandbox: manager.HoldsSandbox,
		BeforeCreate: manager.BeforeCreate, Created: manager.Created, CreateFailed: manager.CreateFailed,
		BeforeRefCall: func(ctx context.Context, method, ref string, generation uuid.UUID) error {
			if method == runnerwire.MethodKill || method == runnerwire.MethodEnd {
				streams.EndLocalAttach(ref) // a local terminal never outlives its sandbox
			}
			return manager.BeforeRefCall(ctx, method, ref, generation)
		},
		AfterRefCall: manager.AfterRefCall, CheckSelftest: manager.CheckSelftest,
		MutationFinished: manager.MutationFinished,
		ExecutionResult:  manager.ExecutionResult,
		Streams:          streams.Call, Output: streams.Output, BindOutput: streams.BindOutput, ForgetOutput: streams.ForgetOutput,
		Resident: resident, LinuxVM: extra.linuxVM != nil,
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
	// sendCaps reports the runner's classes, capacity and last failure over peer: at each connect, and
	// again when the run relay is lost or regained.
	sendCaps := func(ctx context.Context, peer *runnerwire.Peer) error {
		_, keeps := sub.(runner.SandboxEnder)
		support, err := sub.Classes(ctx)
		if err != nil {
			return err
		}
		// A host that cannot say its name reports none, and keeps the machine name it had.
		machine, _ := os.Hostname()
		arch := extra.arch
		if arch == "" {
			arch = engineArch(ctx)
		}
		var failure *runnerwire.Failure
		if extra.lastFailure != nil {
			failure = extra.lastFailure()
		}
		data, err := runnerwire.Marshal(runnerwire.Caps{Support: support, Version: version.String(), MachineName: machine, Arch: arch, Capacity: capacity(ctx), LastFailure: failure, BYOISelftest: dispatcher.Selftests(), Retire: true, Resident: dispatcher.Residents(), EndsUnkept: !keeps, LinuxVM: extra.linuxVM, LocalAttach: local != nil})
		if err != nil {
			return err
		}
		return peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	}
	var live livePeer
	hooks := runnerwire.RunnerHooks{
		Call: dispatcher.Call,
		// The org never opens a stream toward a runner: a relay is the
		// runner's, opened outbound by localRelay.
		Open: func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error {
			return runnerwire.Refuse("unsupported runner stream")
		},
		Clock: manager.Clock, Pending: manager.Pending, StatePages: manager.StatePages,
		BeforeReady: manager.BeforeReady, Lease: manager.Lease, Offline: manager.Offline,
		// manager.Revoked has released the manager's lock before the resident
		// erase runs (lock order: Resident, then Manager).
		Revoked: func(ctx context.Context) error { return errors.Join(manager.Revoked(ctx), resident.EraseAll(ctx)) },
		Replay: func(ctx context.Context, peer *runnerwire.Peer) error {
			if err := sendCaps(ctx, peer); err != nil {
				return err
			}
			// Posture only gates runner_resident delivery; a refused report
			// must not keep the runner from READY.
			if reporter != nil {
				if err := reporter.Send(ctx, peer); err != nil {
					slog.WarnContext(ctx, "runner posture report failed", "error", err)
				}
			}
			return manager.Replay(ctx, peer)
		},
		Online: func(peer *runnerwire.Peer, _ bool) {
			live.set(peer)
			manager.Online(peer)
			streams.Online(peer)
			relay.Online(peer)
			if reporter != nil {
				go reporter.Watch(peer, postureInterval)
			}
		},
		LocalAttach: local.Set,
		StreamOffline: func(peer *runnerwire.Peer) {
			live.clear(peer)
			streams.Offline(peer)
			relay.Offline(peer)
		},
	}
	run := func(ctx context.Context) error {
		go func() {
			if manager.IsRevoked() {
				if err := resident.EraseAll(ctx); err != nil {
					slog.WarnContext(ctx, "runner resident erase after revocation will retry on the sweep", "error", err)
				}
			}
			resident.SweepExpired(ctx, time.Minute)
		}()
		return manager.Run(ctx)
	}
	refresh := func(ctx context.Context) error {
		peer := live.get()
		if peer == nil {
			// Offline, or a session not yet online: the report stays pending and goes out when the
			// session comes up, which a resumed one does not do by itself.
			return errOffline
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		err := sendCaps(ctx, peer)
		if err != nil {
			slog.WarnContext(ctx, "runner could not report its state to the organisation; it will try again at the next check", "error", err)
		}
		return err
	}
	return runnerRuntime{hooks: hooks, run: run, refreshCaps: refresh, close: func() error {
		err := errors.Join(local.Close(), manager.Close(), relay.Close())
		if extra.closeAlso != nil {
			err = errors.Join(err, extra.closeAlso())
		}
		return err
	}}, nil
}

// errOffline is refreshCaps with no session up; the relay watch keeps its report pending.
var errOffline = errors.New("no session is up")

// livePeer is the session that is up, for a report outside a connect.
type livePeer struct {
	mu   sync.Mutex
	peer *runnerwire.Peer
}

func (l *livePeer) set(p *runnerwire.Peer) { l.mu.Lock(); l.peer = p; l.mu.Unlock() }
func (l *livePeer) get() *runnerwire.Peer  { l.mu.Lock(); defer l.mu.Unlock(); return l.peer }
func (l *livePeer) clear(p *runnerwire.Peer) {
	l.mu.Lock()
	if l.peer == p {
		l.peer = nil
	}
	l.mu.Unlock()
}

// evidenceForwarder breaks the construction cycle: the driver needs the manager's evidence store,
// and the manager needs the driver. Before the manager exists the driver only stops an earlier
// registration's proxies (SetAsidePrevious), whose runs this runner holds no record of.
type evidenceForwarder struct{ m *offline.Manager }

func (e *evidenceForwarder) LoadEvidence(id uuid.UUID) ([]byte, error) {
	if e.m == nil {
		return nil, nil // an earlier registration's run: no sink key, so nothing to settle
	}
	return e.m.LoadEvidence(id)
}

func (e *evidenceForwarder) SaveEvidence(id uuid.UUID, b []byte) error {
	if e.m == nil {
		return errors.New("evidence store not ready")
	}
	return e.m.SaveEvidence(id, b)
}

// stateDirSink is the decision sink over the runner's state directory: its key there (stable across
// restarts, so its pin is) and each run's spool there.
func stateDirSink(stateDir string, opts spoolSinkOptions) (*spoolSink, error) {
	cert, _, err := loadOrCreateSinkCert(filepath.Join(stateDir, "decision-sink.key"))
	if err != nil {
		return nil, err
	}
	opts.Cert = cert
	opts.SpoolDir = func(id uuid.UUID) string { return filepath.Join(stateDir, "spools", id.String()) }
	opts.ActivityDir = opts.SpoolDir
	return newSpoolSink(opts), nil
}

// sinkAdapter is the decision sink as a driver drives it (k8s.DecisionSink, docker.DecisionSink): the
// per-run key, Register, Succeed (Docker), the proxy's decision_sink value, Drain and ProducerGone.
type sinkAdapter struct {
	s   *spoolSink
	url func() string // tls://host:port, as a run's proxy dials the sink
}

func (a sinkAdapter) NewKey() (string, [32]byte, error) { return newSinkKey() }

func (a sinkAdapter) Register(id uuid.UUID, hash [32]byte) error { return a.s.Register(id, hash) }

func (a sinkAdapter) Succeed(id uuid.UUID, hash [32]byte) error { return a.s.Succeed(id, hash) }

func (a sinkAdapter) ProxyConfig(key string) any {
	return proxy.DecisionSinkConfig{URL: a.url(), SPKISHA256: a.s.SPKIPin(), Key: key}
}

// Drain answers whatever the proxy did; its reply is the sink's own record (runnerspool.Drained).
// The proxy closes no open tunnel on drain: those end with the pod or
// container, which is why the caller removes it right after and never treats a drain as evidence.
func (a sinkAdapter) Drain(ctx context.Context, id uuid.UUID) error {
	_, err := a.s.Drain(ctx, id)
	return err
}

// ProducerGone settles the run's producer. A run the sink no longer holds (sealed at retirement, its
// spool erased, or retired before this restart) has nothing left to settle, so a later stop of it, such as
// the orphan sweep removing a kept agent, still succeeds.
func (a sinkAdapter) ProducerGone(ctx context.Context, id uuid.UUID) error {
	_, err := a.s.ProducerGone(ctx, id)
	if errors.Is(err, errSinkRunUnknown) {
		return nil
	}
	return err
}

// sinkAddrFile keeps where a Docker runner's decision sink last listened (bind:port), so a restart
// listens there again before its startup containment stops the runs' proxies: one still running
// reconnects and delivers what it holds while it stops, instead of losing it.
const sinkAddrFile = "decision-sink.addr"

// dockerSink is a Docker runner's decision sink, the one Kubernetes runners use, served on the relay's
// bind address. A run's proxy reaches it at host.docker.internal, which its hosts map to the relay's
// target, so runnerd is the spool's only writer on every Docker engine.
type dockerSink struct {
	*spoolSink
	ctx      context.Context
	stateDir string

	addrMu sync.Mutex
	bound  map[netip.Addr]uint16 // where it listens: address and port
	lns    []net.Listener
	url    string // decision_sink.url for new proxies; "" until use
}

// newDockerSink starts the sink, listening at once where it listened before a restart when that
// address is still free and one the relay may listen on (admitRelayBind): never every interface.
func newDockerSink(ctx context.Context, stateDir string, eng dockerEngine) (*dockerSink, error) {
	s, err := stateDirSink(stateDir, spoolSinkOptions{})
	if err != nil {
		return nil, err
	}
	d := &dockerSink{spoolSink: s, ctx: ctx, stateDir: stateDir, bound: map[netip.Addr]uint16{}}
	b, err := os.ReadFile(filepath.Join(stateDir, sinkAddrFile))
	if err != nil {
		return d, nil
	}
	if ap, perr := netip.ParseAddrPort(strings.TrimSpace(string(b))); perr == nil {
		if aerr := admitRelayBind(ap.Addr(), eng); aerr != nil {
			slog.WarnContext(ctx, "decision sink: decision-sink.addr names an address the relay may not listen on, so the sink does not listen there; a run's proxy from before this restart cannot deliver what it holds, and its decisions are counted as lost", "address", ap.String(), "error", aerr)
		} else if _, lerr := d.listen(ap); lerr != nil {
			slog.WarnContext(ctx, "decision sink: cannot listen where it did before this restart, so a run's proxy from before it cannot deliver what it holds; its decisions are counted as lost", "address", ap.String(), "error", lerr)
		}
	}
	return d, nil
}

func (d *dockerSink) listen(ap netip.AddrPort) (uint16, error) {
	ln, err := net.Listen("tcp", ap.String())
	if err != nil {
		return 0, err
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	d.addrMu.Lock()
	d.bound[ap.Addr()] = port
	d.lns = append(d.lns, ln)
	d.addrMu.Unlock()
	go func() {
		if err := d.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.ErrorContext(d.ctx, "decision sink stopped", "error", err)
		}
	}()
	return port, nil
}

// Close closes the sink and every listener it opened, one whose Serve has not started included, so
// its port is free once Close returns.
func (d *dockerSink) Close() error {
	err := d.spoolSink.Close()
	d.addrMu.Lock()
	lns := d.lns
	d.lns = nil
	d.addrMu.Unlock()
	for _, ln := range lns {
		if cerr := ln.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = errors.Join(err, cerr)
		}
	}
	return err
}

// use points new proxies at the sink on bind, the relay's address, listening there first unless it
// already does (at the port it had before a restart); a new address is kept in the state directory.
func (d *dockerSink) use(bind netip.Addr) error {
	d.addrMu.Lock()
	port, ok := d.bound[bind]
	d.addrMu.Unlock()
	if !ok {
		var err error
		if port, err = d.listen(netip.AddrPortFrom(bind, 0)); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(d.stateDir, sinkAddrFile), []byte(netip.AddrPortFrom(bind, port).String()+"\n"), 0o600); err != nil {
			return err
		}
	}
	d.addrMu.Lock()
	d.url = fmt.Sprintf("tls://host.docker.internal:%d", port)
	d.addrMu.Unlock()
	return nil
}

// proxyURL is decision_sink.url for a new proxy.
func (d *dockerSink) proxyURL() string {
	d.addrMu.Lock()
	defer d.addrMu.Unlock()
	return d.url
}

// extras are the sink's part of the runtime: Tick regrants credit after the organisation's acks and
// retries loss rows, and at startup, before the containment pass, the driver's record store becomes
// the manager's and every run's sink key is registered again.
func (d *dockerSink) extras(sub *docker.Driver, ev *evidenceForwarder) runtimeExtras {
	return runtimeExtras{
		options: offline.Options{Tick: d.Tick},
		started: func(_ context.Context, m *offline.Manager) error {
			ev.m = m
			return sub.ReregisterSinks(m.SinkRunIDs())
		},
		closeAlso: d.Close,
	}
}
