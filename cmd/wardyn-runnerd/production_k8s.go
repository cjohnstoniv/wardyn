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
	"os"
	"path/filepath"
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
)

// serviceAccountNamespace is where the kubelet writes runnerd's own namespace.
const serviceAccountNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// errRelayServiceIPs: the relay Service must resolve to exactly one ClusterIP (§3.4 item 4).
var errRelayServiceIPs = errors.New("relay_service_not_one_ip")

// evidenceForwarder breaks the construction cycle: the driver needs the manager's evidence store,
// and the manager needs the driver. The driver reads no evidence before the manager exists.
type evidenceForwarder struct{ m *offline.Manager }

func (e *evidenceForwarder) LoadEvidence(id uuid.UUID) ([]byte, error) {
	if e.m == nil {
		return nil, errors.New("evidence store not ready")
	}
	return e.m.LoadEvidence(id)
}

func (e *evidenceForwarder) SaveEvidence(id uuid.UUID, b []byte) error {
	if e.m == nil {
		return errors.New("evidence store not ready")
	}
	return e.m.SaveEvidence(id, b)
}

// k8sRunnerHooks builds the k8s runner (design K1 §3): the driver in runner mode over the runs
// namespace, the pod-IP relay with its fence, the own-namespace keystore and the self-lapse
// options, then the §3.4 startup checks. Each refusal is named.
func k8sRunnerHooks(ctx context.Context, identity runneridentity.Identity, stateDir string, opts serveOptions) (runnerRuntime, error) {
	if opts.proxyImage == "" {
		return runnerRuntime{}, errors.New("runner serve requires --proxy-image")
	}
	if opts.runsNamespace == "" || opts.relayService == "" {
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
	raw, err := os.ReadFile(serviceAccountNamespace)
	if err != nil {
		return runnerRuntime{}, fmt.Errorf("read runnerd's own namespace: %w", err)
	}
	ownNS := strings.TrimSpace(string(raw))
	if ownNS == "" || ownNS == opts.runsNamespace {
		return runnerRuntime{}, fmt.Errorf("--runs-namespace %q must be a dedicated namespace, not runnerd's own (%q)", opts.runsNamespace, ownNS)
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, opts.relayService)
	if err != nil || len(addrs) != 1 {
		return runnerRuntime{}, fmt.Errorf("%w: %s resolved to %v (%v)", errRelayServiceIPs, opts.relayService, addrs, err)
	}
	clusterIP := addrs[0]
	hardDeny, err := k8s.NewHardDeny(env.clusterDomain, []string{clusterIP, env.podIP.String()}, append(env.apiserverPeers, env.hardDenyCIDRs...))
	if err != nil {
		return runnerRuntime{}, err
	}
	relay := newK8sRelay(ctx, env.podIP, uint16(env.relayPort), uint16(env.relayServicePort), clusterIP)
	ev := &evidenceForwarder{}
	runnerID := identity.RunnerID.String()
	var drv *k8s.Driver
	// The decision sink. If it cannot start, the runner still boots to contain what it holds, but
	// Sink stays nil and every sandbox create is refused.
	sink, sinkErr := startDecisionSink(ctx, stateDir, env, clusterIP, spoolSinkOptions{
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
	if sinkErr != nil {
		slog.ErrorContext(ctx, "decision sink did not start; this runner refuses new runs", "error", sinkErr)
	}
	drv, err = k8s.NewRunner(substrate.Deps{ProxyImage: opts.proxyImage, ConfinementRuntimes: env.confinement, Record: record}, func(c *k8s.Config) {
		c.Namespace = opts.runsNamespace
		c.RunnerID = runnerID
		c.ControlPlaneRelay = relay.Ensure
		c.RunnerdPeer = &k8s.RunnerdPeer{Namespace: ownNS, PodLabels: env.podSelector, RelayPort: env.relayPort, SinkPort: env.sink}
		c.TrustedCAPEM = opts.trustedCA
		c.MaxPodLifetime = opts.maxPodLifetime
		c.Evidence = ev
		c.Fence = relay.Fence
		c.IPFenced = relay.IPFenced
		c.HardDeny = hardDeny
		if sink != nil {
			c.Sink = sinkAdapter{s: sink, url: fmt.Sprintf("tls://%s", net.JoinHostPort(clusterIP, fmt.Sprint(env.sink)))}
		}
	})
	if err != nil {
		return runnerRuntime{}, errors.Join(err, relay.Close(), closeSink(sink))
	}
	relay.lookup = drv.ProxyRunByIP
	ks := keystore.NewK8s(drv.Clientset(), ownNS, runnerID)
	started := func(ctx context.Context, m *offline.Manager) error {
		ev.m = m
		// Every fence, with its IP refusal, is in the relay before Online can set its peer (R4-M1).
		if err := drv.ReplayFences(m.EvidenceRunIDs()); err != nil {
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
	var tick func(context.Context)
	if sink != nil {
		tick = sink.Tick
	}
	rt, err := runnerRuntimeWith(ctx, identity, stateDir, drv, k8s.SandboxRef, relay, unknownCapacity, runtimeExtras{
		keystore:  runneragent.Keystore{Set: ks.Set, Get: ks.Get, Delete: ks.Delete},
		noPosture: true,
		arch:      opts.arch,
		options:   offline.Options{ProxySelfLapses: true, Tick: tick},
		started:   started,
		closeAlso: func() error { return closeSink(sink) },
	})
	if err != nil {
		return runnerRuntime{}, errors.Join(err, relay.Close(), closeSink(sink))
	}
	checks := errors.Join(
		drv.CheckRunnerAccess(ctx, ownNS),
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
	slog.InfoContext(ctx, "k8s runner ready to connect", "runs_namespace", opts.runsNamespace, "relay", clusterIP, "listen", env.podIP)
	return rt, nil
}

// startDecisionSink loads the sink's key (stable across restarts, so its pin is), and serves the
// sink on the pod IP at the sink port.
func startDecisionSink(ctx context.Context, stateDir string, env k8sEnv, clusterIP string, opts spoolSinkOptions) (*spoolSink, error) {
	cert, _, err := loadOrCreateSinkCert(filepath.Join(stateDir, "decision-sink.key"))
	if err != nil {
		return nil, err
	}
	opts.Cert = cert
	opts.SpoolDir = func(id uuid.UUID) string { return filepath.Join(stateDir, "spools", id.String()) }
	opts.ActivityDir = opts.SpoolDir
	ln, err := net.Listen("tcp", netip.AddrPortFrom(env.podIP, uint16(env.sink)).String())
	if err != nil {
		return nil, fmt.Errorf("decision sink: %w", err)
	}
	s := newSpoolSink(opts)
	go func() {
		if err := s.Serve(ln); err != nil {
			slog.ErrorContext(ctx, "decision sink stopped", "error", err)
		}
	}()
	return s, nil
}

func closeSink(s *spoolSink) error {
	if s == nil {
		return nil
	}
	return s.Close()
}

// unknownCapacity: a k8s runner advertises no engine capacity; its limits are the registered ones.
func unknownCapacity(context.Context) placement.Capacity { return placement.Capacity{} }
