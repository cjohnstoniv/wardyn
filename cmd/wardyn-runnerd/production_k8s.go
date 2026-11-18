// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && k8s

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/keystore"
	"github.com/cjohnstoniv/wardyn/internal/runner/offline"
	"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// serviceAccountNamespace is where the kubelet writes runnerd's own namespace.
const serviceAccountNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// errRelayServiceIPs: the relay Service must resolve to exactly one ClusterIP (§3.4 item 4).
var errRelayServiceIPs = errors.New("relay_service_not_one_ip")

// k8sOwnNamespaceAndRelayIP reads runnerd's own namespace, checks the runs namespaces against it, and
// resolves the relay Service to its one ClusterIP.
func k8sOwnNamespaceAndRelayIP(ctx context.Context, opts serveOptions) (ownNS, clusterIP string, err error) {
	raw, err := os.ReadFile(serviceAccountNamespace)
	if err != nil {
		return "", "", fmt.Errorf("read runnerd's own namespace: %w", err)
	}
	ownNS = strings.TrimSpace(string(raw))
	if err := checkRunsNamespaces(opts.runsNamespaces, ownNS); err != nil {
		return "", "", err
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, opts.relayService)
	if err != nil || len(addrs) != 1 {
		return "", "", fmt.Errorf("%w: %s resolved to %v (%v)", errRelayServiceIPs, opts.relayService, addrs, err)
	}
	return ownNS, addrs[0], nil
}

// k8sRunnerHooks builds the k8s runner (design K1 §3): the driver in runner mode over the runs
// namespace, the pod-IP relay with its fence, the own-namespace keystore and the self-lapse
// options, then the §3.4 startup checks. Each refusal is named.
func k8sRunnerHooks(ctx context.Context, identity runneridentity.Identity, stateDir string, opts serveOptions) (runnerRuntime, error) {
	if opts.proxyImage == "" {
		return runnerRuntime{}, errors.New("runner serve requires --proxy-image")
	}
	if len(opts.runsNamespaces) == 0 || opts.relayService == "" {
		return runnerRuntime{}, errors.New("--substrate=k8s requires --runs-namespace and --relay-service")
	}
	if err := errors.Join(checkArch(opts.arch), checkMaxPodLifetime(opts.maxPodLifetime)); err != nil {
		return runnerRuntime{}, err
	}
	record, err := opts.recordingEnabled()
	if err != nil {
		return runnerRuntime{}, err
	}
	env, err := readK8sEnv(os.Environ())
	if err != nil {
		return runnerRuntime{}, err
	}
	if opts.relayListen != env.podIP.String() {
		return runnerRuntime{}, fmt.Errorf("--relay-listen %q is not this pod's IP %s: the k8s relay binds only the pod IP", opts.relayListen, env.podIP)
	}
	ownNS, clusterIP, err := k8sOwnNamespaceAndRelayIP(ctx, opts)
	if err != nil {
		return runnerRuntime{}, err
	}
	hardDeny, err := k8s.NewHardDeny(env.clusterDomain, []string{clusterIP, env.podIP.String()}, append(env.apiserverPeers, env.hardDenyCIDRs...))
	if err != nil {
		return runnerRuntime{}, err
	}
	if err := upstreamProxyOutsideHardDeny(opts.upstreamProxy, hardDeny); err != nil {
		return runnerRuntime{}, err
	}
	relay := newK8sRelay(ctx, env.podIP, uint16(env.relayPort), uint16(env.relayServicePort), clusterIP)
	ev := &evidenceForwarder{}
	runnerID := identity.RunnerID.String()
	var drv *k8s.Driver
	// The decision sink. One that cannot start refuses boot like the other startup checks: without
	// it a held run's proxy cannot reconnect, drain or settle.
	sink, err := startDecisionSink(ctx, stateDir, env, clusterIP, spoolSinkOptions{
		OnLapsed: onSinkLapsed(func(id uuid.UUID) error { return drv.NoteValidatedLapse(id) }, func(id uuid.UUID, e string) error {
			if ev.m == nil {
				return errors.New("offline state not ready")
			}
			return ev.m.ProxyExited(id, e)
		}),
		// A session end is not evidence: it only brings the next proxy list forward.
		OnSessionEnd: func(uuid.UUID) {
			if m := ev.m; m != nil {
				go func() {
					lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
					defer cancel()
					_ = m.ObserveProxies(lctx)
				}()
			}
		},
	})
	if err != nil {
		return runnerRuntime{}, errors.Join(fmt.Errorf("decision sink did not start: %w", err), relay.Close())
	}
	drv, err = k8s.NewRunner(substrate.Deps{ProxyImage: opts.proxyImage, ConfinementRuntimes: env.confinement, Record: record}, func(c *k8s.Config) {
		c.Namespaces = opts.runsNamespaces
		c.RunnerID = runnerID
		c.ControlPlaneRelay = relay.Ensure
		c.RunnerdPeer = &k8s.RunnerdPeer{Namespace: ownNS, PodLabels: env.podSelector, RelayPort: env.relayPort, SinkPort: env.sink}
		c.TrustedCAPEM = opts.trustedCA
		c.UpstreamProxyURL = opts.upstreamProxy
		c.Arch = opts.arch
		c.MaxPodLifetime = opts.maxPodLifetime
		c.Evidence = ev
		c.Fence = relay.Fence
		c.Unfence = relay.Unfence
		c.IPFenced = relay.IPFenced
		c.RelayKey = relay.RelayKey
		c.FencedRuns = relay.FencedRuns
		c.DropRelayKey = relay.DropRelayKey
		c.HardDeny = hardDeny
		sinkURL := fmt.Sprintf("tls://%s", net.JoinHostPort(clusterIP, fmt.Sprint(env.sink)))
		c.Sink = sinkAdapter{s: sink, url: func() string { return sinkURL }}
	})
	if err != nil {
		return runnerRuntime{}, errors.Join(err, relay.Close(), sink.Close())
	}
	relay.lookup = drv.ProxyRunByIP
	var linuxVM *runnerwire.VMCaps
	if env.linuxVM {
		linuxVM = enableK8sLinuxVM(ctx, drv)
	}
	ks := keystore.NewK8s(drv.Clientset(), ownNS, runnerID)
	// The WardynWorkload watcher: nil unless the chart lists workload namespaces (design T3 §2.5).
	workloads, err := newClusterWorkloadWatcher(ctx, env.workloadNamespaces)
	if err != nil {
		return runnerRuntime{}, errors.Join(err, relay.Close(), sink.Close(), drv.Close())
	}
	started := func(ctx context.Context, m *offline.Manager) error {
		ev.m = m
		// A retired run's relay key goes with it; startup replays only unretired runs' keys.
		m.OnRetired(relay.DropRelayKey)
		// A live run in a namespace no longer listed refuses boot; the rest are mapped to theirs.
		if err := drv.CheckRunNamespaces(ctx, m.EvidenceRunIDs()); err != nil {
			return err
		}
		// Every fence, with its IP refusal, is in the relay before Online can set its peer (R4-M1).
		if err := drv.ReplayFences(m.EvidenceRunIDs()); err != nil {
			return err
		}
		// Every unfenced keyed run's relay key hash is back in the relay before Online too, so its
		// proxy reconnects after the restart; the fences are in first, so no fenced run gets one.
		if err := drv.ReplayRelayKeys(m.EvidenceRunIDs()); err != nil {
			return err
		}
		// Every non-retired run's sink key is registered again before Online, so its proxy
		// reconnects after the restart.
		if err := drv.ReregisterSinks(m.SinkRunIDs()); err != nil {
			return err
		}
		// A restart lists the proxies first: an exited or stuck one gets its obligation, the rest a
		// provisional cleanup deadline (§6.2).
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := m.ObserveProxies(lctx); err != nil {
			slog.WarnContext(ctx, "runner startup proxy list will retry", "error", err)
		}
		m.ProvisionalDeadlines()
		return nil
	}
	rt, err := runnerRuntimeWith(ctx, identity, stateDir, drv, k8s.SandboxRef, relay, unknownCapacity, runtimeExtras{
		keystore:       runneragent.Keystore{Set: ks.Set, Get: ks.Get, Delete: ks.Delete, DeleteAll: ks.DeleteAll, Precheck: ks.Precheck},
		noPosture:      true,
		arch:           opts.arch,
		options:        offline.Options{ProxySelfLapses: true, Tick: sink.Tick},
		started:        started,
		closeAlso:      func() error { return errors.Join(drv.Close(), sink.Close()) }, // drv.Close stops the canary re-check loop
		linuxVM:        linuxVM,
		report:         connectReport{log: opts.offline},
		runsNamespaces: opts.runsNamespaces,
		workloadStatus: workloads.statusFunc(),
		netPolWatch:    drv.OnNetPolChange,
	})
	if err != nil {
		return runnerRuntime{}, errors.Join(err, relay.Close(), sink.Close(), drv.Close())
	}
	rt = workloads.attach(rt)
	checks := errors.Join(
		drv.CheckRunnerAccess(ctx, ownNS, env.workloadNamespaces...),
		drv.CheckForeignObjects(ctx),
		drv.CheckKubeletFloor(ctx, opts.arch),
	)
	if checks == nil {
		if checks = relay.listen(); checks == nil {
			checks = drv.ProbeRelaySource(ctx, clusterIP, env.relayServicePort, relay.awaitProbe())
		}
	}
	if checks != nil {
		return runnerRuntime{}, errors.Join(checks, rt.close())
	}
	slog.InfoContext(ctx, "k8s runner ready to connect", "runs_namespaces", opts.runsNamespaces, "relay", clusterIP, "listen", env.podIP)
	return rt, nil
}

// enableK8sLinuxVM turns Linux VM sandboxes on (CC3 pods under the pinned RuntimeClass) and returns
// what the runner's caps offer. A RuntimeClass that does not resolve logs why and offers none;
// container runs are untouched either way.
func enableK8sLinuxVM(ctx context.Context, drv *k8s.Driver) *runnerwire.VMCaps {
	m, err := drv.EnableLinuxVM(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Linux VM sandboxes are off on this runner", "reason", err.Error())
		return nil
	}
	slog.InfoContext(ctx, "Linux VM sandboxes are on", "machine_type", m.MachineType)
	return &runnerwire.VMCaps{MachineType: m.MachineType, CPUModel: m.CPUModel, QEMUVersion: m.QEMUVersion}
}

// startDecisionSink serves the state directory's sink (stateDirSink) on the pod IP at the sink port.
func startDecisionSink(ctx context.Context, stateDir string, env k8sEnv, clusterIP string, opts spoolSinkOptions) (*spoolSink, error) {
	s, err := stateDirSink(stateDir, opts)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", netip.AddrPortFrom(env.podIP, uint16(env.sink)).String())
	if err != nil {
		return nil, fmt.Errorf("decision sink: %w", err)
	}
	go func() {
		if err := s.Serve(ln); err != nil {
			slog.ErrorContext(ctx, "decision sink stopped", "error", err)
		}
	}()
	return s, nil
}

// unknownCapacity: a k8s runner advertises no engine capacity; its limits are the registered ones.
func unknownCapacity(context.Context) placement.Capacity { return placement.Capacity{} }

// upstreamProxyOutsideHardDeny refuses an --upstream-proxy whose literal address is one the run's hard
// deny covers: an upstream proxy inside the cluster would reach the in-cluster addresses
// the deny set keeps every run away from, so the misconfiguration is refused at boot.
func upstreamProxyOutsideHardDeny(raw string, h *k8s.HardDeny) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("--upstream-proxy / WARDYN_UPSTREAM_PROXY: %w", err)
	}
	ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]"))
	if err != nil {
		return nil // a name: the proxy's own resolution and the hard deny's name check stand
	}
	ip = ip.Unmap()
	inside := slices.Contains(h.Hosts, ip.String())
	for _, c := range h.CIDRs {
		if p, perr := netip.ParsePrefix(c); perr == nil && p.Contains(ip) {
			inside = true
		}
	}
	if inside {
		return fmt.Errorf("--upstream-proxy / WARDYN_UPSTREAM_PROXY names %s, an address inside this runner's hard deny: an upstream proxy in the cluster would reach the in-cluster addresses every run is kept from", ip)
	}
	return nil
}
