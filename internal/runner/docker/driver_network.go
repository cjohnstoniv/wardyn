// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CreateSandbox provisions the per-run network, the wardyn-proxy sidecar, and
// the agent container with L0 confinement. Order matters for fail-closed
// teardown: anything created before an error is rolled back.
//
//nolint:funlen // Deliberate: a single linear container-assembly sequence (network → proxy sidecar → hardening → mounts → sandbox container) whose teardown-on-failure compensations must stay in one scope to be verifiably complete; low branching (passes gocyclo/gocognit), just long.
func (d *Driver) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	infoRes, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return runner.Sandbox{}, fmt.Errorf("docker: info: %w", err)
	}
	info := infoRes.Info

	// Resolve confinement runtime FIRST and fail closed before creating
	// anything if the demanded class cannot be enforced (invariant 5).
	runtimeName, _, err := resolveRuntime(spec.ConfinementClass, info, d.cfg.ConfinementRuntimes)
	if err != nil {
		return runner.Sandbox{}, err
	}

	enforced := spec.ConfinementClass
	if enforced == "" {
		// Class-less floor for direct driver callers only: every wardynd
		// dispatch path resolves a concrete class from the policy's
		// min_confinement_class before reaching here (the shipped default
		// policy floor is CC2).
		enforced = types.CC1
	}

	if err := d.preflightSpec(spec); err != nil {
		return runner.Sandbox{}, err
	}

	// Best-effort image presence: pull the agent image if absent.
	if err := d.ensureImage(ctx, spec.Image, func() { spec.NotifyWaiting("image: Pulling: " + spec.Image) }); err != nil {
		return runner.Sandbox{}, err
	}
	// ...and the proxy sidecar image, which nothing else fetches: only a repo
	// checkout or CI (which build it) have it resident already.
	// deploy/compose/docker-compose.yaml parks the proxy-image stanza in
	// profiles: ["build-only"], so `docker compose pull` resolves three images and
	// never this one, and `--no-build` cannot build it — so without this pull, the
	// one-line install and the desktop tier reach a healthy stack and console and
	// then fail the FIRST RUN at sandbox creation with an unresolvable ref.
	//
	// One line at the chokepoint that already has the right semantics ("failures
	// to pull surface as errors — fail closed, never run a sandbox we could not
	// provision"). It no-ops where the image is already resident, so a checkout
	// and CI (which builds it) are unaffected, and k8s is untouched — kubelet
	// pulls the sidecar there.
	if err := d.ensureImage(ctx, d.cfg.ProxyImage, func() { spec.NotifyWaiting("image: Pulling: " + d.cfg.ProxyImage) }); err != nil {
		return runner.Sandbox{}, err
	}

	// (1) Per-run internal network. Internal=true => Docker provisions no
	// gateway, so the network cannot route off-host: this is what upholds L0
	// even though the agent is *connected* to it.
	intNet, err := d.cli.NetworkCreate(ctx, internalNetName(spec.RunID), client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels:   wardynLabels(spec.RunID, "network", spec.Labels),
	})
	if err != nil {
		return runner.Sandbox{}, fmt.Errorf("docker: create internal network: %w", err)
	}
	// rollback collects teardown steps to run on any later failure.
	var rollback []func()
	rollback = append(rollback, func() { _, _ = d.cli.NetworkRemove(context.Background(), intNet.ID, client.NetworkRemoveOptions{}) })
	fail := func(err error) (runner.Sandbox, error) {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
		return runner.Sandbox{}, err
	}

	// (2) wardyn-proxy sidecar: attached to the per-run internal network now;
	// the control-plane-facing network is joined after create. It carries the
	// run token + control-plane URL as non-secret env (the token is verifiable
	// but not usable outside the platform, per runner.ProxyConfig).
	proxyID, err := d.startProxy(ctx, spec.RunID, wardynLabels(spec.RunID, componentProxy, spec.Labels),
		proxyEnv(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort), netip.Addr{})
	if err != nil {
		return fail(err)
	}
	rollback = append(rollback, func() {
		_, _ = d.cli.ContainerRemove(context.Background(), proxyID, client.ContainerRemoveOptions{Force: true})
	})

	// Resolve the proxy's IP on the per-run internal network so the agent can
	// reach it via a static /etc/hosts entry instead of Docker's embedded DNS.
	// gVisor's netstack (CC2/runsc) does NOT traverse the embedded resolver at
	// 127.0.0.11, so resolving the "wardyn-proxy" container alias fails under
	// runsc (the agent then cannot reach its only egress path). A static hosts
	// entry works under every runtime and weakens nothing: the agent still has no
	// default route — the proxy remains its sole path off the gatewayless segment.
	proxyInspectRes, err := d.cli.ContainerInspect(ctx, proxyID, client.ContainerInspectOptions{})
	if err != nil {
		return fail(fmt.Errorf("docker: inspect proxy for its network IP: %w", err))
	}
	proxyInspect := proxyInspectRes.Container
	proxyIP := ""
	if proxyInspect.NetworkSettings != nil {
		// v29: EndpointSettings.IPAddress is a netip.Addr (was string). Guard on
		// IsValid so the zero Addr maps to "" (fail closed below), not the
		// "invalid IP" string a zero Addr would stringify to.
		if ep := proxyInspect.NetworkSettings.Networks[internalNetName(spec.RunID)]; ep != nil && ep.IPAddress.IsValid() {
			proxyIP = ep.IPAddress.String()
		}
	}
	if proxyIP == "" {
		return fail(fmt.Errorf("docker: proxy has no IP on the per-run internal network %q", internalNetName(spec.RunID)))
	}

	// (3) Agent container: attached ONLY to the per-run internal network. Because
	// that network is Internal=true it has NO gateway, so the agent gets no
	// default route (L0): its sole egress path is the wardyn-proxy sidecar on the
	// same segment. The agent is placed on the internal network AT CREATE TIME via
	// NetworkMode + NetworkingConfig rather than created in "none" mode and then
	// connected: Docker (29.x) refuses to NetworkConnect a container whose primary
	// NetworkMode is the private "none" mode ("cannot be connected to multiple
	// networks with one of the networks in private (none) mode"). Attaching at
	// create keeps the agent off the host bridge entirely while preserving the no-
	// default-route guarantee, so L0 is structurally identical to the old path.
	// Idle main process (holds the container open for Exec/attach). For an
	// INTERACTIVE run — which never Exec's agent-run — use `agent-run --idle`, which
	// prepares the workspace (installs the MITM CA, clones the repo into ~/work, …)
	// then idles, so the attach shell isn't empty. Every other run keeps the minimal
	// CA-only idle script: agent-run's task exec does the preparation, and cloning on
	// the shared idle PID would drop the clone out of the task's recording.
	idleCmd := []string{"sh", "-c", runner.AgentIdleScript}
	if spec.Interactive {
		idleCmd = []string{"agent-run", "--idle"}
	}
	env := envSlice(spec.Env)
	// SandboxSpec.SecretEnv rides plain container env HERE, and that is the
	// honest posture on this substrate rather than an oversight: the k8s driver
	// must route it through a Secret because a Pod spec is readable by anyone
	// holding pods/get in the namespace, whereas a docker container's config is
	// reachable only through the daemon socket — the same root-equivalent trust
	// boundary proxyEnv already documents for the run token. Concatenation needs
	// no dedup: dispatch's splitSecretEnv keeps the two maps disjoint.
	env = append(env, envSlice(spec.SecretEnv)...)
	if d.cfg.Record {
		// The one in-sandbox signal that session recording is configured.
		// boot_seed_rec_wrap (agent-run-lib.sh) keys its wardyn-rec wrap on this
		// rather than on the /var/log/wardyn dir the one-shot root exec creates:
		// that exec races the interactive boot-seed pane (an ephemeral
		// workspace's prep is faster than the post-start exec), and losing the
		// race silently shipped an unrecorded seed session.
		// Split literal: the envdoc guard scans raw file bytes for the
		// quote-delimited var name alone, and this is its one Go-side appearance.
		env = append(env, "WARDYN_RECORDING"+"=1")
	}
	agentCfg := &container.Config{
		Image:    spec.Image,
		Hostname: "agent",
		Env:      env,
		Labels:   wardynLabels(spec.RunID, componentAgent, spec.Labels),
		Tty:      true, // keep a TTY so Exec can attach a PTY for recording.
		// Hold the container open; the agent process is launched by Exec.
		Cmd: idleCmd,
	}
	// NetworkMode is the per-run internal network (NOT "none"): it is gatewayless,
	// so it provides no default route. The endpoint carries the "agent" alias so
	// the proxy can identify the agent's segment deterministically.
	agentHost := hardenedHostConfig(internalNetName(spec.RunID), runtimeName, spec.Resources, info)
	// Pin the proxy's IP so the agent resolves "wardyn-proxy" without the embedded
	// DNS (required under gVisor; harmless under runc). This is the ONLY host entry
	// the agent gets — NOT host.docker.internal, which stays proxy-only.
	agentHost.ExtraHosts = append(agentHost.ExtraHosts, "wardyn-proxy:"+proxyIP)
	// The drive's read-only bind asks for a RECURSIVELY read-only mount only
	// where the runtime this container will actually run on declares support —
	// runtimeName as resolved above, never the daemon's default. Under a runtime
	// that does not (gVisor, which CC2 requires), asking refuses the create
	// outright; see driveBindOptions.
	agentMounts, err := d.agentMounts(ctx, spec, runtimeSupportsRecursiveReadOnly(info, runtimeName))
	if err != nil {
		return fail(err)
	}
	agentHost.Mounts = append(agentHost.Mounts, agentMounts...)

	agentNetCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			internalNetName(spec.RunID): {
				NetworkID: intNet.ID,
				Aliases:   []string{"agent"},
			},
		},
	}
	// EXEC-LESS runtimes (krun/libkrun) cannot `docker exec` a workload into a
	// running microVM, so there is no keep-alive container to create here: the
	// workload must be the container's MAIN process. Defer agent creation to Exec
	// (which knows the argv), stashing the fully-built + hardened config. The proxy
	// and per-run network ARE up (created above), so egress + recording upload work
	// the moment the agent starts. The ref is the deterministic agent NAME; Status
	// and Teardown already resolve the agent by name.
	if !runtimeSupportsExec(runtimeName) {
		// libkrun runs the guest init as ROOT with HOME=/ — it does NOT apply the
		// image's USER (a documented libkrun trait; the microVM boundary, not the
		// uid, is CC3's isolation guarantee). An agent that resolves ~/… (agent-run's
		// ~/work, ~/.claude) then breaks ("mkdir //work: permission denied"). Pin HOME
		// to the Wardyn agent-image home so ~ resolves; docker-exec runtimes get this
		// for free from /etc/passwd. (Alternative permanent fix: ENV HOME in the agent
		// images.) Respect an explicit HOME from policy env.
		ensureEnv(&agentCfg.Env, "HOME", agentImageHome)
		name := agentContainerName(spec.RunID)
		d.mu.Lock()
		d.pending[name] = &pendingAgent{cfg: agentCfg, host: agentHost, netcfg: agentNetCfg, managed: spec.ManagedFiles}
		d.mu.Unlock()
		return runner.Sandbox{Ref: name, Driver: driverName, EnforcedClass: enforced}, nil
	}

	agentResp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           agentCfg,
		HostConfig:       agentHost,
		NetworkingConfig: agentNetCfg,
		Name:             agentContainerName(spec.RunID),
	})
	if err != nil {
		return fail(fmt.Errorf("docker: create agent: %w", err))
	}
	rollback = append(rollback, func() {
		_, _ = d.cli.ContainerRemove(context.Background(), agentResp.ID, client.ContainerRemoveOptions{Force: true})
	})

	// Fail closed if the daemon DISCARDED a resource limit we requested (a cgroup
	// controller missing / not delegated) — an untrusted sandbox must not run
	// effectively uncapped. Authoritative post-CREATE signal (the create-response
	// discard warning), correct on both Moby and Podman; opt out on a trusted host
	// with WARDYN_ALLOW_UNENFORCEABLE_CAPS=1. rollback tears down the agent + proxy
	// + per-run network on failure.
	//
	// Gated BEFORE ContainerStart, not after: the warning is already in the create
	// response, so starting first would run the untrusted workload uncapped for the
	// lifetime of the check and only then kill it. "Refuses to launch" has to mean
	// never launched, not launched-and-reaped.
	if capErr := verifyCapsEnforced(agentResp.Warnings); capErr != nil {
		if d.cfg.AllowUnenforceableCaps {
			slog.Warn("wardynd: the daemon discarded a resource limit — proceeding because WARDYN_ALLOW_UNENFORCEABLE_CAPS=1; the sandbox may run without CPU/memory/pids limits",
				slog.String("detail", capErr.Error()))
		} else {
			return fail(capErr)
		}
	}

	if err := d.deliverManagedFilesAndStart(ctx, agentResp.ID, spec.ManagedFiles); err != nil {
		return fail(err)
	}

	// Recording requires two agent-writable directories that do NOT exist
	// (writably) for a non-root agent user (uid 1000 per the image contract):
	//   - CastDir (default /var/log/wardyn): wardyn-rec MkdirAll's this and dies
	//     with EPERM under /var/log (root-owned 0755) before recording anything.
	//   - RecordingMount target (/wardyn/recordings): a fresh named volume the
	//     daemon creates root-owned 0755; wardyn-rec -out-dir delivery EPERMs.
	// Both are prepared via a single one-shot ROOT exec right after start so any
	// agent uid can write. Best-effort: a failure here is never fatal to the
	// sandbox; it would only impair recording, which surfaces separately.
	if d.cfg.Record {
		d.prepareRecordingDirs(ctx, agentResp.ID)
	}

	return runner.Sandbox{
		// The deterministic name, not agentResp.ID: Docker's API accepts either
		// for every call this driver makes on Ref (name and ID are
		// interchangeable path params), but teardown's not-found fallback
		// (runIDFromAgentName) can only recover a run id from THIS form — a raw
		// daemon-assigned ID carries no run id at all, so that fallback would
		// silently no-op for every exec-capable (runc/gVisor) runtime — the
		// common case — leaking the proxy sidecar + per-run network whenever the
		// agent container was already gone at teardown.
		Ref:           agentContainerName(spec.RunID),
		Driver:        driverName,
		EnforcedClass: enforced,
	}, nil
}

// StopSandbox is the graceful path: SIGTERM, then SIGKILL after the timeout,
// then remove. Idempotent on a missing sandbox.
func (d *Driver) StopSandbox(ctx context.Context, ref string) error {
	timeout := int(stopTimeout.Seconds())
	if _, err := d.cli.ContainerStop(ctx, ref, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: stop: %w", err)
	}
	return d.teardown(ctx, ref)
}

// EndSandbox is the lease end (runner.SandboxEnder): stop the agent as
// StopSandbox does but leave the container in place, so its writable layer
// (the checkout, the harness transcript) survives, then stop the proxy
// sidecar. Agent first, as StopSandbox orders it, so a recorder flushing on
// SIGTERM still delivers through the proxy. The per-run network stays for
// teardown to remove with the agent. Fails closed: a run id it cannot resolve
// is an error, never a success that left the proxy up.
func (d *Driver) EndSandbox(ctx context.Context, ref string) error {
	timeout := int(stopTimeout.Seconds())
	if _, err := d.cli.ContainerStop(ctx, ref, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: stop agent: %w", err)
	}
	return d.stopProxy(ctx, ref)
}

// StopProxy stops the agent ref's proxy sidecar and nothing else
// (runner.ProxyStopper): the agent keeps running with no network path. The
// stopped container is kept, because its env is where the run's rendered
// proxy config (and the MITM CA key inside it) rests and ReplaceProxy reads it
// back from there; teardown removes it. Fails closed like EndSandbox: when the
// proxy can be neither stopped nor killed, the agent is stopped too (kept, not
// removed), so no work runs while its egress is unconfirmed, and the proxy's
// error is still returned (#1060).
func (d *Driver) StopProxy(ctx context.Context, ref string) error {
	err := d.stopProxy(ctx, ref)
	if err == nil {
		return nil
	}
	timeout := int(stopTimeout.Seconds())
	if _, aerr := d.cli.ContainerStop(ctx, ref, client.ContainerStopOptions{Timeout: &timeout}); aerr != nil && !isNotFound(aerr) {
		return errors.Join(err, fmt.Errorf("docker: stop agent: %w", aerr))
	}
	return err
}

// stopProxy stops the agent ref's proxy sidecar, escalating to SIGKILL when
// the graceful stop fails (#1060). Neither call removes the container, so its
// env survives for ReplaceProxy. A kill refused because the proxy is already
// stopped is that stop confirmed.
func (d *Driver) stopProxy(ctx context.Context, ref string) error {
	id, err := d.proxyRunID(ctx, ref)
	if err != nil {
		return err
	}
	name := proxyContainerName(id)
	timeout := int(stopTimeout.Seconds())
	_, err = d.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout})
	if err == nil || isNotFound(err) {
		return nil
	}
	if _, kerr := d.cli.ContainerKill(ctx, name, client.ContainerKillOptions{Signal: "KILL"}); kerr != nil && !isNotFound(kerr) && !isNotRunning(kerr) {
		return fmt.Errorf("docker: stop proxy: %w; kill proxy: %w", err, kerr)
	}
	return nil
}

// proxyRunID resolves the run id of the agent ref's proxy sidecar: from the
// deterministic agent name, else from the agent's run-id label.
func (d *Driver) proxyRunID(ctx context.Context, ref string) (uuid.UUID, error) {
	if id, err := runIDFromAgentName(ref); err == nil {
		return id, nil
	}
	res, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil || res.Container.Config == nil {
		return uuid.Nil, fmt.Errorf("docker: proxy of agent %s: %w", ref, errTeardownUnresolved)
	}
	id, err := parseRunID(res.Container.Config.Labels[labelRun])
	if err != nil {
		return uuid.Nil, fmt.Errorf("docker: proxy of agent %s: %w", ref, errTeardownUnresolved)
	}
	return id, nil
}

// FreezeSandbox is runner Freeze/Thaw's pause half (runner.Freezer,
// long-holds design rev 4 §3.1): ContainerPause on the AGENT ref only — never
// the proxy sidecar, which is not addressed here and keeps renewing and
// answering egress decisions while the agent is frozen. Verified against runc
// (cgroup v2): a paused container refuses a new exec, its established TCP
// stays ESTABLISHED and ACKed, and its timers fire at thaw. Idempotent: a
// missing container, or one already paused, is not an error — a retried
// freeze after a lost response must not surface as a failure.
func (d *Driver) FreezeSandbox(ctx context.Context, ref string) error {
	if _, err := d.cli.ContainerPause(ctx, ref, client.ContainerPauseOptions{}); err != nil && !isNotFound(err) && !isAlreadyPaused(err) {
		return fmt.Errorf("docker: pause: %w", err)
	}
	return nil
}

// ThawSandbox is runner Freeze/Thaw's resume half (runner.Freezer):
// ContainerUnpause on the agent ref. Idempotent: a missing container, or one
// that is not paused, is not an error.
func (d *Driver) ThawSandbox(ctx context.Context, ref string) error {
	if _, err := d.cli.ContainerUnpause(ctx, ref, client.ContainerUnpauseOptions{}); err != nil && !isNotFound(err) && !isNotPaused(err) {
		return fmt.Errorf("docker: unpause: %w", err)
	}
	return nil
}

// KillSandbox is the kill-switch path: immediate SIGKILL + force remove. The
// control plane cascades identity/credential revocation around this call.
func (d *Driver) KillSandbox(ctx context.Context, ref string) error {
	if _, err := d.cli.ContainerKill(ctx, ref, client.ContainerKillOptions{Signal: "KILL"}); err != nil && !isNotFound(err) {
		// A stopped container cannot be killed; treat "not running" as benign
		// and proceed to force-remove below.
		if !isNotRunning(err) {
			return fmt.Errorf("docker: kill: %w", err)
		}
	}
	return d.teardown(ctx, ref)
}

// teardown force-removes the agent container, then the proxy and per-run
// internal network for the same run. The run id is recovered from the agent's
// run-id label, or — if that label is missing OR corrupt — from the
// deterministic agent container name (daemon-set), so the sibling proxy and
// network are never orphaned. A found-and-removed agent whose run id cannot be
// resolved either way returns errTeardownUnresolved (fail closed, no false
// success). Every step is idempotent: missing objects are not errors.
func (d *Driver) teardown(ctx context.Context, agentRef string) error {
	// Drop any tracked agent exec for this ref: the container is going away, so
	// a pending Wait (if any) will observe the inspect error/ctx and return.
	// This also keeps agentExecs from growing without bound across runs.
	// Dropping creating[agentRef] is what tells an in-flight runAsMainProcess that
	// its ref was torn down mid-create: the container it is about to make is not
	// visible to the ContainerRemove below, so IT must remove it.
	d.mu.Lock()
	delete(d.agentExecs, agentRef)
	delete(d.pending, agentRef)
	delete(d.mainProc, agentRef)
	delete(d.creating, agentRef)
	d.mu.Unlock()

	var id uuid.UUID
	res, err := d.cli.ContainerInspect(ctx, agentRef, client.ContainerInspectOptions{})
	switch {
	case err == nil:
		insp := res.Container
		var runID string
		if insp.Config != nil {
			runID = insp.Config.Labels[labelRun]
		}
		if parsed, perr := parseRunID(runID); perr == nil {
			id = parsed
		} else if insp.Name != "" {
			// Label missing OR corrupt (non-UUID): recover the run id from the
			// deterministic agent container name so the sibling proxy (routable
			// network, run token) and per-run network are not orphaned. (v29
			// inlined the old ContainerJSONBase fields, so a present Name is the
			// "we got a real inspect body" guard.)
			if nid, nerr := runIDFromAgentName(insp.Name); nerr == nil {
				id = nid
			}
		}
	case !isNotFound(err):
		return fmt.Errorf("docker: inspect for teardown: %w", err)
	}

	if _, err := d.cli.ContainerRemove(ctx, agentRef, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove agent: %w", err)
	}

	if id == uuid.Nil && isNotFound(err) {
		// The agent container is already gone (crashed, OOM-killed, or a
		// concurrent teardown beat us to it), so ContainerInspect never ran the
		// label/name recovery above at all — id stayed uuid.Nil. Reporting
		// success here without resolving the run id would leak the sibling
		// proxy sidecar (still holding the run's credentials) and the per-run
		// network, so recover it from agentRef before giving up: it IS the
		// deterministic agent name on BOTH substrates — the exec-less (krun)
		// path always used it (see Exec), and CreateSandbox's exec-based
		// (runc/gVisor) return names it too, instead of the daemon's opaque
		// agentResp.ID. A ref this parse still can't recognize (e.g. an older
		// persisted ref predating deterministic naming) falls through to the
		// prior idempotent-success behavior unchanged.
		if nid, nerr := runIDFromAgentName(agentRef); nerr == nil {
			id = nid
		} else {
			return nil
		}
	}
	if id == uuid.Nil {
		// A found-and-removed agent whose run id could not be resolved (label
		// absent AND name unparseable): its proxy + network may now be
		// orphaned — surface that honestly rather than reporting a false
		// success (fail closed). NOTE: a retry then sees the agent not-found
		// and returns success, so callers must act on this FIRST error; a
		// second teardown cannot re-detect the orphan.
		return fmt.Errorf("docker: teardown of agent %s: %w", agentRef, errTeardownUnresolved)
	}

	if _, err := d.cli.ContainerRemove(ctx, proxyContainerName(id), client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove proxy: %w", err)
	}
	if _, err := d.cli.NetworkRemove(ctx, internalNetName(id), client.NetworkRemoveOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove internal network: %w", err)
	}
	return nil
}

// containerListerAPI is the narrow docker-client slice SweepOrphanedSandboxes
// needs beyond dockerAPI's own methods — kept separate (envbuild/reaper.go's
// pattern) so the main seam does not grow ContainerList, and every dockerAPI
// fake with it, until something else needs to list. A client that doesn't
// implement it (a narrower test fake) is simply not swept.
type containerListerAPI interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
}

// the real client must implement it.
var _ containerListerAPI = (*client.Client)(nil)

// SweepOrphanedSandboxes tears down the sandbox objects (agent container +
// sibling proxy + per-run internal network) of every run whose row no longer
// owns them — the residue of a crash BETWEEN CreateSandbox and SetSandboxRef,
// which leaves those containers running under a run row that carries no
// sandbox_ref, so no ref-keyed teardown (reconcile.go, SweepTerminalSandboxes)
// ever revisits them (D13). isOrphan — supplied by the control plane, the only
// layer that can read run rows — answers "does this run id still legitimately
// own live sandbox containers?"; minAge is the reaper AGE GATE
// (undispatchedGrace) below which a container is assumed to belong to a dispatch
// still in flight and is left alone. That age gate is the SAME deliberately-blunt
// multi-process safety net as envbuild's SweepOrphanedBuilds: a younger container
// may be another replica's live dispatch that has not yet written its ref, so
// only one older than any dispatch could still be provisioning is assumed
// abandoned. Reuses the existing deterministic-name teardown, so a partially-created
// sandbox (proxy up, agent still coming) is torn down whole from the run id alone.
//
// ponytail: keyed on the AGENT container (component=agent), whose deterministic
// name reconstructs the proxy + network — a crash in the ~ms between
// network-create and agent-create can leave a network with no container to key
// on; that vanishingly rare edge is left to `docker network prune`, not a
// second labeled-object listing.
func (d *Driver) SweepOrphanedSandboxes(ctx context.Context, minAge time.Duration, isOrphan func(runID uuid.UUID) bool) (int, error) {
	lister, ok := d.cli.(containerListerAPI)
	if !ok {
		return 0, nil
	}
	res, err := lister.ContainerList(ctx, client.ContainerListOptions{
		All:     true, // a crashed sandbox's agent may be exited, not running
		Filters: client.Filters{}.Add("label", labelComponent+"="+componentAgent),
	})
	if err != nil {
		return 0, fmt.Errorf("docker: list containers for orphan sweep: %w", err)
	}
	cutoff := time.Now().Add(-minAge)
	var swept int
	var errs []error
	for _, c := range res.Items {
		runID, perr := parseRunID(c.Labels[labelRun])
		if perr != nil {
			continue // not a wardyn agent container whose run id we can key teardown on
		}
		if time.Unix(c.Created, 0).After(cutoff) {
			continue // too young: a dispatch may still be about to SetSandboxRef
		}
		if !isOrphan(runID) {
			continue // a live run legitimately owns it
		}
		if terr := d.teardown(ctx, agentContainerName(runID)); terr != nil {
			errs = append(errs, fmt.Errorf("docker: teardown orphaned run %s: %w", runID, terr))
			continue
		}
		swept++
	}
	return swept, errors.Join(errs...)
}
