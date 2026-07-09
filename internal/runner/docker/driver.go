// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Config configures the Docker driver.
type Config struct {
	// ProxyImage is the OCI image for the wardyn-proxy sidecar. For v0 it may
	// be a minimal image with the locally-built wardyn-proxy binary
	// bind-mounted in via ProxyBinaryHostPath.
	ProxyImage string
	// ProxyBinaryHostPath, when set, bind-mounts a host path to
	// /usr/local/bin/wardyn-proxy inside the sidecar (v0 dev convenience). The
	// sidecar's entrypoint is then forced to run it.
	ProxyBinaryHostPath string
	// ProxyCmd, when set, overrides the proxy sidecar container command. Empty in
	// production (the wardyn-proxy image's own long-running entrypoint runs). Used
	// by the conformance gate to keep a bare placeholder proxy image (e.g. busybox,
	// whose default `sh` exits immediately) alive long enough to obtain a per-run
	// network IP, so the runner contract can be exercised without a real proxy.
	ProxyCmd []string
	// InternalNetwork is the name of the pre-existing bridge network that
	// connects the wardyn-proxy sidecars to the control plane. The proxy joins
	// it; the agent never does. Defaults to "wardyn-internal".
	InternalNetwork string
	// CastDir is where wardyn-rec writes session recordings inside the agent
	// container (passed through to the recorder). Defaults to "/var/log/wardyn".
	CastDir string
	// Record enables PTY session recording: Exec wraps the agent argv with
	// wardyn-rec (which execs asciinema or falls back to a .log).
	Record bool
	// RecorderBinary is the path to wardyn-rec inside the agent image.
	// Defaults to "wardyn-rec" (resolved on PATH).
	RecorderBinary string
	// RecordingMount, when set, is a named Docker volume (or an absolute host
	// path, which is bind-mounted) shared with the control plane's recording
	// store. It is mounted at RecordingMountTarget inside the agent container
	// and wardyn-rec delivers the finished cast there (-out-dir). Single-host
	// delivery only; multi-node delivery (upload via proxy) lands in v0.5.
	RecordingMount string
	// StopTimeout is the graceful stop timeout. Defaults to 10s.
	StopTimeout time.Duration
	// ConfinementRuntimes optionally pins, per Confinement Class, the exact
	// Docker runtime family that must back it — the operator knob that makes CC3
	// substrate-pluggable across OCI runtimes (e.g. {CC3: "kata-qemu"} to force
	// QEMU Kata over a Cloud-Hypervisor one, or {CC2: "runsc"}). An empty/absent
	// entry uses the built-in default mapping (CC2->runsc, CC3->kata*); a pinned
	// runtime is still probed against `docker info` and FAILS CLOSED when absent
	// (never downgrades). Non-OCI VMM substrates (SmolVM/Firecracker) are a future
	// Runner driver, not a runtime name here.
	ConfinementRuntimes map[types.ConfinementClass]string
}

// RecordingMountTarget is where RecordingMount appears inside the agent
// container; wardyn-rec's -out-dir points here.
const RecordingMountTarget = "/wardyn/recordings"

// recordingSourceProbeTarget is a known-allowed workspace target used ONLY to
// exercise runner.ValidateMount's host-SOURCE deny-list against a host-bind
// RecordingMount. The real in-container target is RecordingMountTarget, a fixed
// Wardyn-owned path (never attacker-chosen), so the workspace target-prefix rule
// ValidateMount also enforces is not the relevant control here — the host source
// deny-list (/, /proc, docker.sock, symlink escapes, ...) is.
const recordingSourceProbeTarget = "/work"

func (c *Config) withDefaults() {
	if c.InternalNetwork == "" {
		c.InternalNetwork = "wardyn-internal"
	}
	if c.CastDir == "" {
		c.CastDir = "/var/log/wardyn"
	}
	if c.RecorderBinary == "" {
		c.RecorderBinary = "wardyn-rec"
	}
	if c.StopTimeout <= 0 {
		c.StopTimeout = 10 * time.Second
	}
}

// Driver implements runner.Runner against the Docker Engine API.
type Driver struct {
	cli dockerAPI
	cfg Config

	// mu guards agentExecs, pending, and mainProc. The driver is safe for
	// concurrent use; these maps are the only mutable state.
	mu sync.Mutex
	// agentExecs maps a sandbox ref (agent container id) to the exec id of the
	// agent process started by Exec. Wait inspects this exec id to observe the
	// agent's completion + exit code. One entry per ref (the latest Exec wins).
	agentExecs map[string]string
	// pending holds the deferred agent-container config for EXEC-LESS runtimes
	// (krun microVMs): CreateSandbox cannot pre-create a keep-alive container to
	// exec into, so it stashes the built config here (keyed by ref == agent NAME)
	// and Exec creates the container with the workload as its MAIN process.
	pending map[string]*pendingAgent
	// mainProc marks refs whose workload runs as the container main process (the
	// exec-less path), so Wait blocks on container exit instead of an exec.
	mainProc map[string]bool
}

// pendingAgent is the fully-built agent-container config CreateSandbox defers for
// an exec-less runtime; Exec sets its Cmd to the (recorder-wrapped) workload and
// creates the container.
type pendingAgent struct {
	cfg    *container.Config
	host   *container.HostConfig
	netcfg *network.NetworkingConfig
}

// agentImageHome is the home directory of the Wardyn agent-image user (USER
// agent). It is set as HOME on the exec-less (krun) path because libkrun runs the
// guest as root with HOME=/, so ~-relative paths in the agent contract (~/work,
// ~/.claude) would otherwise resolve under / and fail for a non-writable root.
const agentImageHome = "/home/agent"

// agentIdleScript is the agent container's main process: it installs the per-run
// TLS-MITM CA (when delivered) and then idles. This is REQUIRED for INTERACTIVE
// runs, which never invoke agent-run (the human drives claude in the attach
// shell) — without it, NODE_EXTRA_CA_CERTS points at a CA file that was never
// written, so claude cannot trust the proxy's TLS termination of api.anthropic.com
// (breaking subscription proxy-side injection). It writes the EXACT path
// internal/api pins for NODE_EXTRA_CA_CERTS (/home/agent/.wardyn/mitm-ca.pem), not
// $HOME (this Cmd may run as root). Idempotent for non-interactive runs, where
// agent-run re-installs the CA before exec'ing the task. No-op when the run did
// not opt into TLS-MITM (WARDYN_MITM_CA_PEM unset).
const agentIdleScript = `d=/home/agent/.wardyn
if [ -n "${WARDYN_MITM_CA_PEM:-}" ]; then
  mkdir -p "$d"
  printf '%s\n' "$WARDYN_MITM_CA_PEM" > "$d/mitm-ca.pem"
  chmod 0644 "$d/mitm-ca.pem"
  if command -v update-ca-certificates >/dev/null 2>&1; then
    cp "$d/mitm-ca.pem" /usr/local/share/ca-certificates/wardyn-mitm.crt 2>/dev/null && update-ca-certificates >/dev/null 2>&1 || true
  fi
fi
exec sleep infinity`

// ensureEnv appends key=val to env unless key is already present (an explicit
// policy-set value wins).
func ensureEnv(env *[]string, key, val string) {
	prefix := key + "="
	for _, e := range *env {
		if strings.HasPrefix(e, prefix) {
			return
		}
	}
	*env = append(*env, prefix+val)
}

// mainProcCastDir is the agent-writable (tmpfs) cast directory used on the
// exec-less path: the agent runs as a non-root user with no root exec to
// pre-create the (root-owned) default cast dir, so wardyn-rec stages the cast
// here and delivers it via the proxy upload route.
const mainProcCastDir = "/tmp/wardyn-rec"

// Driver is the OCI/Docker confinement substrate; the orchestrator wraps it to
// present the runner.Runner surface to the control plane.
var _ substrate.Substrate = (*Driver)(nil)

// New constructs a Driver against the host Docker daemon, negotiating the API
// version with the server (forward/backward compatibility).
func New(cfg Config) (*Driver, error) {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker: new client: %w", err)
	}
	return newWithClient(cli, cfg), nil
}

// newWithClient is the seam unit tests use to inject a fake dockerAPI.
func newWithClient(cli dockerAPI, cfg Config) *Driver {
	cfg.withDefaults()
	return &Driver{
		cli:        cli,
		cfg:        cfg,
		agentExecs: make(map[string]string),
		pending:    make(map[string]*pendingAgent),
		mainProc:   make(map[string]bool),
	}
}

func (d *Driver) Name() string { return driverName }

// Classes probes the daemon for available runtimes and reports the Confinement
// Classes this host can actually enforce, with the per-class substrate label.
func (d *Driver) Classes(ctx context.Context) (substrate.ClassSupport, error) {
	info, err := d.cli.Info(ctx)
	if err != nil {
		return substrate.ClassSupport{}, fmt.Errorf("docker: info: %w", err)
	}
	c := capabilitiesForWith(info, d.cfg.ConfinementRuntimes)
	return substrate.ClassSupport{
		Classes:          c.ConfinementClasses,
		Resolved:         c.Resolved,
		StructuralEgress: c.StructuralEgress,
		SessionRecording: c.SessionRecording,
	}, nil
}

// CreateSandbox provisions the per-run network, the wardyn-proxy sidecar, and
// the agent container with L0 confinement. Order matters for fail-closed
// teardown: anything created before an error is rolled back.
func (d *Driver) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	info, err := d.cli.Info(ctx)
	if err != nil {
		return runner.Sandbox{}, fmt.Errorf("docker: info: %w", err)
	}

	// Resolve confinement runtime FIRST and fail closed before creating
	// anything if the demanded class cannot be enforced (invariant 5).
	runtimeName, _, err := resolveRuntime(spec.ConfinementClass, info, d.cfg.ConfinementRuntimes)
	if err != nil {
		return runner.Sandbox{}, err
	}
	enforced := spec.ConfinementClass
	if enforced == "" {
		enforced = types.CC1
	}

	if d.cfg.ProxyImage == "" {
		return runner.Sandbox{}, errProxyImageUnset
	}

	// Best-effort image presence: pull the agent image if absent. Proxy image
	// is assumed locally present (operator-provided) to avoid surprise pulls.
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return runner.Sandbox{}, err
	}

	// (1) Per-run internal network. Internal=true => Docker provisions no
	// gateway, so the network cannot route off-host: this is what upholds L0
	// even though the agent is *connected* to it.
	intNet, err := d.cli.NetworkCreate(ctx, internalNetName(spec.RunID), network.CreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels:   wardynLabels(spec.RunID, "network", spec.Labels),
	})
	if err != nil {
		return runner.Sandbox{}, fmt.Errorf("docker: create internal network: %w", err)
	}
	// rollback collects teardown steps to run on any later failure.
	var rollback []func()
	rollback = append(rollback, func() { _ = d.cli.NetworkRemove(context.Background(), intNet.ID) })
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
	proxyCfg := &container.Config{
		Image:        d.cfg.ProxyImage,
		Hostname:     "wardyn-proxy",
		Labels:       wardynLabels(spec.RunID, componentProxy, spec.Labels),
		Env:          proxyEnv(spec.RunID, spec.ProxyConfig, proxyListenPort),
		ExposedPorts: nil,
	}
	if d.cfg.ProxyBinaryHostPath != "" {
		proxyCfg.Entrypoint = []string{"/usr/local/bin/wardyn-proxy"}
	}
	if len(d.cfg.ProxyCmd) > 0 {
		proxyCfg.Cmd = d.cfg.ProxyCmd
	}
	proxyHost := &container.HostConfig{
		// Proxy attaches to the internal net at create; it is NOT NetworkMode
		// none — it must bridge out via the wardyn-internal network joined
		// below. Hardened the same way as the agent.
		NetworkMode:    container.NetworkMode(internalNetName(spec.RunID)),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: false,
		Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=64m"},
		AutoRemove:     false,
		// Map host.docker.internal to the docker host gateway so the brokered
		// control-plane forward (resolveTrustedURL) can reach a wardynd running on
		// the host in host mode. Docker Desktop injects this alias automatically;
		// native docker needs the explicit host-gateway mapping. Scoped to the
		// proxy — only it forwards to the control plane, and the alias is consulted
		// ONLY by the trusted forward path, never by the agent's policy-governed
		// egress (which still denies host IPs via the private-IP guard). General
		// egress is NOT broadened.
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		// The proxy only relays HTTP, so a tight resource envelope still leaves
		// ample headroom while bounding a compromised proxy: its own PID cap
		// (fork-bomb guard) and a modest memory cap (MemorySwap pinned so the
		// cap is not silently doubled via swap).
		Resources: proxyResources(),
	}
	if d.cfg.ProxyBinaryHostPath != "" {
		proxyHost.Binds = []string{d.cfg.ProxyBinaryHostPath + ":/usr/local/bin/wardyn-proxy:ro"}
	}
	proxyResp, err := d.cli.ContainerCreate(ctx, proxyCfg, proxyHost, nil, nil, proxyContainerName(spec.RunID))
	if err != nil {
		return fail(fmt.Errorf("docker: create proxy: %w", err))
	}
	rollback = append(rollback, func() {
		_ = d.cli.ContainerRemove(context.Background(), proxyResp.ID, container.RemoveOptions{Force: true})
	})

	// Connect the proxy to the control-plane-facing network so it can reach
	// the control plane. This network is the ONLY route off the per-run
	// segment, and only the proxy is on it.
	if err := d.cli.NetworkConnect(ctx, d.cfg.InternalNetwork, proxyResp.ID, &network.EndpointSettings{}); err != nil {
		return fail(fmt.Errorf("docker: connect proxy to %s: %w", d.cfg.InternalNetwork, err))
	}

	if err := d.cli.ContainerStart(ctx, proxyResp.ID, container.StartOptions{}); err != nil {
		return fail(fmt.Errorf("docker: start proxy: %w", err))
	}

	// Resolve the proxy's IP on the per-run internal network so the agent can
	// reach it via a static /etc/hosts entry instead of Docker's embedded DNS.
	// gVisor's netstack (CC2/runsc) does NOT traverse the embedded resolver at
	// 127.0.0.11, so resolving the "wardyn-proxy" container alias fails under
	// runsc (the agent then cannot reach its only egress path). A static hosts
	// entry works under every runtime and weakens nothing: the agent still has no
	// default route — the proxy remains its sole path off the gatewayless segment.
	proxyInspect, err := d.cli.ContainerInspect(ctx, proxyResp.ID)
	if err != nil {
		return fail(fmt.Errorf("docker: inspect proxy for its network IP: %w", err))
	}
	proxyIP := ""
	if proxyInspect.NetworkSettings != nil {
		if ep := proxyInspect.NetworkSettings.Networks[internalNetName(spec.RunID)]; ep != nil {
			proxyIP = ep.IPAddress
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
	idleCmd := []string{"sh", "-c", agentIdleScript}
	if spec.Interactive {
		idleCmd = []string{"agent-run", "--idle"}
	}
	agentCfg := &container.Config{
		Image:    spec.Image,
		Hostname: "agent",
		Env:      envSlice(spec.Env),
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
	if d.cfg.RecordingMount != "" {
		// Cast delivery: wardyn-rec writes the finished recording to this
		// shared mount (-out-dir), where the control plane's FSStore reads it.
		mtype := mount.TypeVolume
		if strings.HasPrefix(d.cfg.RecordingMount, "/") {
			mtype = mount.TypeBind
			// A host-path RecordingMount is a real host bind: subject its SOURCE to
			// the same deny-list as workspace binds and FAIL CLOSED (a source that
			// is/traverses /, /proc, docker.sock, ... must never be bound in). Only
			// binds are validated — a named volume is Docker-managed, not a host path
			// (mirrors how the workspace-bind loop treats host paths).
			if err := runner.ValidateMount(runner.Mount{Source: d.cfg.RecordingMount, Target: recordingSourceProbeTarget}); err != nil {
				return fail(fmt.Errorf("docker: denied recording mount %q -> %q: %w", d.cfg.RecordingMount, RecordingMountTarget, err))
			}
		}
		agentHost.Mounts = append(agentHost.Mounts, mount.Mount{
			Type:   mtype,
			Source: d.cfg.RecordingMount,
			Target: RecordingMountTarget,
		})
	}

	// Operator/policy-controlled host bind mounts (e.g. a host repo at ~/work).
	// SECURITY MODEL (documented here and in runner.SandboxSpec.Mounts):
	//   - These come ONLY from a policy's RunPolicySpec.WorkspaceMounts, copied
	//     into spec.Mounts by internal/api dispatch. The create-run HTTP request
	//     has no mounts field, so a prompt-injected agent / malicious requester
	//     can NEVER choose a host mount (invariants 1 & 3).
	//   - DENY-LIST DEFENSE-IN-DEPTH: even though the values came from policy
	//     (already validated at policy-write time), we re-run runner.ValidateMount
	//     here and FAIL CLOSED — any denied Source (/, /proc, /sys, /dev, /run,
	//     /var/run, /var/lib/docker, any docker.sock, /etc, /boot, /root, or a
	//     non-absolute/non-cleaned path) or a Target outside the allowed
	//     in-container prefixes errors the whole CreateSandbox (rollback runs).
	//   - DEFAULT READ-ONLY: a mount is read-only unless the policy explicitly set
	//     ReadOnly=false, so a workspace bind cannot grant host write by default.
	for _, m := range spec.Mounts {
		if err := runner.ValidateMount(m); err != nil {
			return fail(fmt.Errorf("docker: denied workspace mount %q -> %q: %w", m.Source, m.Target, err))
		}
		agentHost.Mounts = append(agentHost.Mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly, // default false in Go == RW only when policy opted in via ReadOnly=false
		})
	}

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
		d.pending[name] = &pendingAgent{cfg: agentCfg, host: agentHost, netcfg: agentNetCfg}
		d.mu.Unlock()
		return runner.Sandbox{Ref: name, Driver: driverName, EnforcedClass: enforced}, nil
	}

	agentResp, err := d.cli.ContainerCreate(ctx, agentCfg, agentHost, agentNetCfg, nil, agentContainerName(spec.RunID))
	if err != nil {
		return fail(fmt.Errorf("docker: create agent: %w", err))
	}
	rollback = append(rollback, func() {
		_ = d.cli.ContainerRemove(context.Background(), agentResp.ID, container.RemoveOptions{Force: true})
	})

	if err := d.cli.ContainerStart(ctx, agentResp.ID, container.StartOptions{}); err != nil {
		return fail(fmt.Errorf("docker: start agent: %w", err))
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
		Ref:           agentResp.ID,
		Driver:        driverName,
		EnforcedClass: enforced,
	}, nil
}

// recordingChmodDirs returns the directories prepareRecordingDirs makes agent-
// writable (chmod 0777). CastDir is ALWAYS included: it is container-local scratch
// created fresh under a root-owned prefix, so loosening it only touches the
// container fs. The RecordingMount target is included ONLY for a NAMED VOLUME (a
// fresh volume is root-owned 0755, so the non-root agent otherwise cannot deliver
// its cast, and the volume is Docker-managed — the relax is contained to it). A
// HOST-BIND RecordingMount is deliberately EXCLUDED: chmod 0777 on it would make
// the operator's HOST directory world-writable. A host-bind mount must be
// provisioned agent-writable by the operator; otherwise the shared-mount fallback
// EPERMs and delivery uses the masked proxy-upload path. (The "/"-prefix split is
// the same one that decides bind vs volume when the mount is attached.)
func recordingChmodDirs(cfg Config) []string {
	dirs := []string{cfg.CastDir}
	if cfg.RecordingMount != "" && !strings.HasPrefix(cfg.RecordingMount, "/") {
		dirs = append(dirs, RecordingMountTarget)
	}
	return dirs
}

// prepareRecordingDirs creates+chmods the cast dir and (for a named-volume
// RecordingMount) the recording-mount target to 0777 via a one-shot root exec, so
// a non-root agent process can both write its in-progress cast and deliver it to
// the shared mount. Best-effort: any failure is swallowed.
func (d *Driver) prepareRecordingDirs(ctx context.Context, ref string) {
	dirs := recordingChmodDirs(d.cfg)
	// `mkdir -p <each> && chmod 0777 <each>` — idempotent; the mount already
	// exists (chmod still applies), the cast dir is created fresh.
	args := append([]string{"-p"}, dirs...)
	created, err := d.cli.ContainerExecCreate(ctx, ref, container.ExecOptions{
		User: "0:0", // root, regardless of the image's default USER
		Cmd:  append([]string{"mkdir"}, args...),
	})
	if err != nil {
		return
	}
	if err := d.cli.ContainerExecStart(ctx, created.ID, container.ExecStartOptions{}); err != nil {
		return
	}
	d.waitExec(ctx, created.ID)

	chmodCreated, err := d.cli.ContainerExecCreate(ctx, ref, container.ExecOptions{
		User: "0:0",
		Cmd:  append([]string{"chmod", "0777"}, dirs...),
	})
	if err != nil {
		return
	}
	if err := d.cli.ContainerExecStart(ctx, chmodCreated.ID, container.ExecStartOptions{}); err != nil {
		return
	}
	d.waitExec(ctx, chmodCreated.ID)
}

// waitExec briefly polls a one-shot exec to completion so a subsequent Exec
// (which races right after) observes the prepared directories. Bounded so a
// stuck exec cannot stall sandbox bring-up.
func (d *Driver) waitExec(ctx context.Context, execID string) {
	for i := 0; i < 50; i++ {
		insp, ierr := d.cli.ContainerExecInspect(ctx, execID)
		if ierr != nil || !insp.Running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordCmd wraps argv with the recorder for a Record-mode launch, shared by
// the exec (Exec) and exec-less (runAsMainProcess) launch paths. Default
// delivery is upload through the proxy's brokered recording route (run-token
// injected proxy-side; cross-run uploads 403 at the control plane), which is
// MASKED control-plane-side before the cast is persisted. HIGH-finding: only
// fall back to the UNMASKED, cross-run-writable shared mount (-out-dir) when
// there is NO masked upload path — recorderArgv enforces the same mutual
// exclusion as defense in depth, so an unmasked cast can never land in the
// API-served replay store when uploads work. castDir differs per caller (the
// root exec path's default cast dir vs the exec-less path's agent-writable
// tmpfs dir); runID recovery also differs per caller's label source, so it is
// passed in already resolved (uuid.Nil if it could not be recovered).
func (d *Driver) recordCmd(runID uuid.UUID, castDir string, argv []string) []string {
	uploadURL := ""
	if runID != uuid.Nil {
		uploadURL = fmt.Sprintf("http://wardyn-proxy:%d/wardyn/v1/recordings/%s", proxyListenPort, runID)
	}
	outDir := ""
	if d.cfg.RecordingMount != "" && uploadURL == "" {
		outDir = RecordingMountTarget
	}
	return recorderArgv(d.cfg.RecorderBinary, castDir, outDir, uploadURL, runID, argv, true)
}

// Exec launches the agent process inside the sandbox with a TTY attached.
// When recording, the argv is wrapped by wardyn-rec (which execs asciinema or
// falls back to a .log). Returns once the process is started, not finished.
func (d *Driver) Exec(ctx context.Context, ref string, argv []string) error {
	if len(argv) == 0 {
		return errors.New("docker: exec: empty argv")
	}
	// EXEC-LESS path: a deferred (krun) agent is created NOW with the workload as
	// its main process — there is no exec to attach.
	d.mu.Lock()
	p, isPending := d.pending[ref]
	d.mu.Unlock()
	if isPending {
		return d.runAsMainProcess(ctx, ref, p, argv)
	}
	cmd := argv
	if d.cfg.Record {
		// Recover the run id from the container label so wardyn-rec names the
		// recording deterministically. If we cannot, record under a nil id
		// rather than failing the exec.
		runID := uuid.Nil
		if insp, err := d.cli.ContainerInspect(ctx, ref); err == nil && insp.Config != nil {
			if id, perr := parseRunID(insp.Config.Labels[labelRun]); perr == nil {
				runID = id
			}
		}
		cmd = d.recordCmd(runID, d.cfg.CastDir, argv)
	}
	execCfg := container.ExecOptions{
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	}
	created, err := d.cli.ContainerExecCreate(ctx, ref, execCfg)
	if err != nil {
		return fmt.Errorf("docker: exec create: %w", err)
	}
	// Track this exec id as the agent process for ref so Wait can observe its
	// completion + exit code. The latest Exec for a ref wins (re-exec replaces).
	d.mu.Lock()
	d.agentExecs[ref] = created.ID
	d.mu.Unlock()
	// Attach (TTY hijack) so output can be wired to wardyn-rec / a sink. The
	// caller (control plane) owns the lifecycle of the returned stream; for v0
	// we start detached after establishing the attach to confirm liveness.
	resp, err := d.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return fmt.Errorf("docker: exec attach: %w", err)
	}
	// We do not block on the process; drain in the background so the PTY does
	// not stall. A real recorder pipeline replaces this drain.
	go func() {
		defer resp.Close()
		_, _ = io.Copy(io.Discard, resp.Reader)
	}()
	return nil
}

// runAsMainProcess is the exec-less agent-launch path (krun microVMs): it creates
// the deferred agent container with the (recorder-wrapped) workload as its MAIN
// process and starts it. Unlike the exec path, there is no separate process to
// attach — dockerd drains the main-process PTY into its log driver, and Wait
// blocks on the CONTAINER's exit. Returns once the container is started.
func (d *Driver) runAsMainProcess(ctx context.Context, ref string, p *pendingAgent, argv []string) error {
	cmd := argv
	if d.cfg.Record {
		runID := uuid.Nil
		if p.cfg != nil {
			if id, perr := parseRunID(p.cfg.Labels[labelRun]); perr == nil {
				runID = id
			}
		}
		// Exec-less agents run as a non-root user and have no root exec to create
		// the root-owned default cast dir; record into an agent-writable tmpfs dir
		// instead (mainProcCastDir) — recordCmd's upload-vs-shared-mount mutual
		// exclusion is otherwise identical to Exec's.
		cmd = d.recordCmd(runID, mainProcCastDir, argv)
	}
	p.cfg.Cmd = cmd
	created, err := d.cli.ContainerCreate(ctx, p.cfg, p.host, p.netcfg, nil, ref)
	if err != nil {
		return fmt.Errorf("docker: create main-process agent: %w", err)
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("docker: start main-process agent: %w", err)
	}
	d.mu.Lock()
	delete(d.pending, ref)
	d.mainProc[ref] = true
	d.mu.Unlock()
	return nil
}

// Wait blocks until the agent process started by Exec for ref has exited and
// returns its exit code. It is only valid after a successful Exec on the same
// ref: it inspects the agent exec id Exec recorded. Unlike waitExec (a bounded
// best-effort poll used for one-shot setup execs), Wait is unbounded and bound
// only by ctx — the agent process may run for as long as the run is alive.
// Returns an error if no agent exec is tracked for ref, or if ctx is cancelled
// before the process exits.
func (d *Driver) Wait(ctx context.Context, ref string) (int, error) {
	d.mu.Lock()
	isMain := d.mainProc[ref]
	execID, ok := d.agentExecs[ref]
	d.mu.Unlock()
	// EXEC-LESS path: the workload IS the container's main process, so its exit is
	// the container's exit.
	if isMain {
		return d.waitMainProcess(ctx, ref)
	}
	if !ok {
		return 0, fmt.Errorf("docker: wait: no agent exec tracked for ref %q (Exec not called?)", ref)
	}

	// Poll the exec to completion. ExecInspect.Running flips to false once the
	// process exits; ExitCode is then authoritative. The poll cadence matches
	// waitExec; the unbounded loop is what distinguishes Wait from it.
	const pollInterval = 200 * time.Millisecond
	for {
		insp, err := d.cli.ContainerExecInspect(ctx, execID)
		if err != nil {
			return 0, fmt.Errorf("docker: wait: exec inspect: %w", err)
		}
		if !insp.Running {
			return insp.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("docker: wait: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// waitMainProcess blocks on the exec-less agent container's exit and returns its
// code. ContainerWait with WaitConditionNotRunning returns the code even if the
// container has already exited, so there is no create/exit race.
func (d *Driver) waitMainProcess(ctx context.Context, ref string) (int, error) {
	statusCh, errCh := d.cli.ContainerWait(ctx, ref, container.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
		return 0, fmt.Errorf("docker: wait (main process): %w", ctx.Err())
	case err := <-errCh:
		return 0, fmt.Errorf("docker: wait (main process): %w", err)
	case res := <-statusCh:
		if res.Error != nil {
			return int(res.StatusCode), fmt.Errorf("docker: wait (main process): %s", res.Error.Message)
		}
		return int(res.StatusCode), nil
	}
}

func (d *Driver) Status(ctx context.Context, ref string) (runner.Status, error) {
	insp, err := d.cli.ContainerInspect(ctx, ref)
	if err != nil {
		if isNotFound(err) {
			return runner.Status{State: types.RunStopped, Message: "container not found"}, nil
		}
		return runner.Status{}, fmt.Errorf("docker: inspect: %w", err)
	}
	return statusFromInspect(insp), nil
}

// StopSandbox is the graceful path: SIGTERM, then SIGKILL after the timeout,
// then remove. Idempotent on a missing sandbox.
func (d *Driver) StopSandbox(ctx context.Context, ref string) error {
	timeout := int(d.cfg.StopTimeout.Seconds())
	if err := d.cli.ContainerStop(ctx, ref, container.StopOptions{Timeout: &timeout}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: stop: %w", err)
	}
	return d.teardown(ctx, ref)
}

// KillSandbox is the kill-switch path: immediate SIGKILL + force remove. The
// control plane cascades identity/credential revocation around this call.
func (d *Driver) KillSandbox(ctx context.Context, ref string) error {
	if err := d.cli.ContainerKill(ctx, ref, "KILL"); err != nil && !isNotFound(err) {
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
	d.mu.Lock()
	delete(d.agentExecs, agentRef)
	delete(d.pending, agentRef)
	delete(d.mainProc, agentRef)
	d.mu.Unlock()

	var id uuid.UUID
	insp, err := d.cli.ContainerInspect(ctx, agentRef)
	switch {
	case err == nil:
		var runID string
		if insp.Config != nil {
			runID = insp.Config.Labels[labelRun]
		}
		if parsed, perr := parseRunID(runID); perr == nil {
			id = parsed
		} else if insp.ContainerJSONBase != nil {
			// Label missing OR corrupt (non-UUID): recover the run id from the
			// deterministic agent container name so the sibling proxy (routable
			// network, run token) and per-run network are not orphaned.
			if nid, nerr := runIDFromAgentName(insp.Name); nerr == nil {
				id = nid
			}
		}
	case !isNotFound(err):
		return fmt.Errorf("docker: inspect for teardown: %w", err)
	}

	if err := d.cli.ContainerRemove(ctx, agentRef, container.RemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove agent: %w", err)
	}

	if id == uuid.Nil {
		// A not-found agent means the sandbox was already gone (nothing to tear
		// down): idempotent success. But if we DID find/remove an agent yet could
		// not resolve its run id (label absent AND name unparseable), its proxy +
		// network may now be orphaned — surface that honestly rather than
		// reporting a false success (fail closed). NOTE: a retry then sees the
		// agent not-found and returns success, so callers must act on this FIRST
		// error; a second teardown cannot re-detect the orphan.
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("docker: teardown of agent %s: %w", agentRef, errTeardownUnresolved)
	}

	if err := d.cli.ContainerRemove(ctx, proxyContainerName(id), container.RemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove proxy: %w", err)
	}
	if err := d.cli.NetworkRemove(ctx, internalNetName(id)); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove internal network: %w", err)
	}
	return nil
}

// ensureImage pulls ref if it is not already present locally. Pull output is
// drained and discarded; failures to pull surface as errors (fail closed —
// never run a sandbox we could not provision).
func (d *Driver) ensureImage(ctx context.Context, ref string) error {
	present, err := d.imagePresent(ctx, ref)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	// imagePresent said false, so a pull failure means the image is genuinely
	// absent locally (not a stale tag). The demo agent tags (wardyn/agent-*:demo)
	// live in no registry, so name the fix rather than leaking a bare
	// "registry: denied".
	if err := dockerutil.PullImage(ctx, d.cli, ref, "docker"); err != nil {
		return fmt.Errorf("%w (image %q not present locally and pull failed — for the demo images run: make agent-images)", err, ref)
	}
	return nil
}

func (d *Driver) imagePresent(ctx context.Context, ref string) (bool, error) {
	f := filters.NewArgs()
	f.Add("reference", ref)
	summaries, err := d.cli.ImageList(ctx, image.ListOptions{Filters: f})
	if err != nil {
		return false, fmt.Errorf("docker: image list: %w", err)
	}
	return len(summaries) > 0, nil
}

// statusFromInspect maps Docker container state to a Wardyn RunState.
func statusFromInspect(insp container.InspectResponse) runner.Status {
	st := runner.Status{State: types.RunRunning}
	if insp.State == nil {
		st.State = types.RunStopped
		return st
	}
	s := insp.State
	switch {
	case s.Running:
		st.State = types.RunRunning
	case s.OOMKilled:
		st.State = types.RunFailed
		st.Message = "OOM killed"
	case s.Status == "created":
		st.State = types.RunStarting
	case s.Status == "exited", s.Status == "dead":
		ec := s.ExitCode
		st.ExitCode = &ec
		if ec == 0 {
			st.State = types.RunStopped
		} else {
			st.State = types.RunFailed
			st.Message = fmt.Sprintf("exit code %d", ec)
		}
	default:
		st.State = types.RunStopped
	}
	if s.Error != "" {
		st.Message = strings.TrimSpace(st.Message + " " + s.Error)
	}
	return st
}

// envSlice converts a non-secret env map to Docker's KEY=VALUE slice form.
// Secrets never pass here (invariant 1) — the spec contract forbids it.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// proxyEnv builds the environment handed to the wardyn-proxy sidecar: the
// full proxy config (incl. the run's egress policy — a proxy without a policy
// fails closed and the sandbox has no egress) as one JSON env var, plus the
// individual values for operator inspection. The run token is verifiable but
// not a usable secret outside the platform; env visibility is part of the
// documented daemon-trust tradeoff.
func proxyEnv(runID uuid.UUID, pc runner.ProxyConfig, port int) []string {
	inj := make([]proxy.InjectionConfig, 0, len(pc.Injection))
	for _, g := range pc.Injection {
		inj = append(inj, proxy.InjectionConfig{InjectionRule: g.Rule, GrantID: g.GrantID})
	}
	cfg := proxy.Config{
		RunID:            runID,
		ControlPlaneURL:  pc.ControlPlaneURL,
		RunToken:         pc.RunToken,
		Policy:           pc.Policy,
		Injection:        inj,
		Listen:           ":" + strconv.Itoa(port),
		MITMCACertPEM:    pc.MITMCACertPEM,
		MITMCAKeyPEM:     pc.MITMCAKeyPEM,
		MITMHosts:        pc.MITMHosts,
		MITMLLM:          pc.MITMLLM,
		UpstreamProxyURL: pc.UpstreamProxyURL,
	}
	cfgJSON, _ := json.Marshal(cfg)
	return []string{
		"WARDYN_PROXY_CONFIG_JSON=" + string(cfgJSON),
		"WARDYN_RUN_ID=" + runID.String(),
		"WARDYN_CONTROL_PLANE_URL=" + pc.ControlPlaneURL,
	}
}

// parseRunID parses a run-id label back to a UUID.
func parseRunID(s string) (uuid.UUID, error) { return uuid.Parse(s) }

// isNotRunning detects the daemon's "container not running" error so Kill can
// proceed to force-remove an already-stopped container.
func isNotRunning(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "is not running")
}
