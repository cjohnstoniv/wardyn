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
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

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
	// Record enables PTY session recording: Exec wraps the agent argv with
	// wardyn-rec (which execs asciinema or falls back to a .log).
	Record bool
	// RecordingMount, when set, is a named Docker volume (or an absolute host
	// path, which is bind-mounted) shared with the control plane's recording
	// store. It is mounted at RecordingMountTarget inside the agent container
	// and wardyn-rec delivers the finished cast there (-out-dir). Single-host
	// delivery only; multi-node delivery (upload via proxy) lands in v0.5.
	RecordingMount string
	// ConfinementRuntimes optionally pins, per Confinement Class, the exact
	// Docker runtime family that must back it — the operator knob that makes CC3
	// substrate-pluggable across OCI runtimes (e.g. {CC3: "kata-qemu"} to force
	// QEMU Kata over a Cloud-Hypervisor one, or {CC2: "runsc"}). An empty/absent
	// entry uses the built-in default mapping (CC2->runsc, CC3->kata*); a pinned
	// runtime is still probed against `docker info` and FAILS CLOSED when absent
	// (never downgrades). Non-OCI VMM substrates (SmolVM/Firecracker) are a future
	// Runner driver, not a runtime name here.
	ConfinementRuntimes map[types.ConfinementClass]string
	// AllowUnenforceableCaps downgrades the fail-closed resource-cap probe to a
	// warning: when the Docker daemon reports it cannot enforce CPU/memory/pids
	// limits (cgroup controller missing / not delegated), CreateSandbox proceeds
	// instead of refusing. OFF by default — an untrusted sandbox must not run
	// uncapped. Set only on a trusted host (WARDYN_ALLOW_UNENFORCEABLE_CAPS=1).
	AllowUnenforceableCaps bool
}

// RecordingMountTarget is where RecordingMount appears inside the agent
// container; wardyn-rec's -out-dir points here.
const RecordingMountTarget = "/wardyn/recordings"

// defaultCastDir is where wardyn-rec writes session recordings inside the
// agent container. recorderBinary is wardyn-rec's path inside the agent
// image (resolved on PATH). stopTimeout is the graceful StopSandbox timeout.
// Nobody in the repo overrides any of these (P3-RNR-2); re-add a
// Config knob if that changes.
const (
	defaultCastDir = "/var/log/wardyn"
	recorderBinary = "wardyn-rec"
	stopTimeout    = 10 * time.Second
)

// pollInterval is Wait's probe cadence, and waitMaxProbeErrors is how many
// CONSECUTIVE transient probe errors it tolerates before giving up (~1 min,
// matching the control plane's reconcileMaxProbeErrors budget).
//
// A transient daemon/API blip is NOT "the agent exited": Wait's error is terminal
// for its caller — the completion watcher stops watching and the run is stranded
// RUNNING with a live sandbox — so a blip must not surface as an error at all. A
// not-found is authoritative (the exec/container really is gone) and is never
// retried, so a kill/teardown still unblocks Wait immediately.
const (
	pollInterval       = 200 * time.Millisecond
	waitMaxProbeErrors = 300
)

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
}

// Driver implements runner.Runner against the Docker Engine API.
type Driver struct {
	cli dockerAPI
	cfg Config

	// mu guards agentExecs, pending, mainProc, and creating. The driver is safe
	// for concurrent use; these maps are the only mutable state.
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
	// creating marks exec-less refs whose agent container is being created RIGHT
	// NOW by runAsMainProcess (Exec claimed the pending entry, the ContainerCreate
	// has not returned yet). It is the tombstone that makes the pending->created
	// transition atomic w.r.t. teardown: teardown finds no container to remove in
	// that window, so it deletes the mark instead, and runAsMainProcess — seeing
	// its mark gone — removes the container it just made rather than leaving a
	// killed run's agent alive. Fail closed: the entry only ever means "this ref
	// is still live".
	creating map[string]bool
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

// agentIdleScript is the agent container's main (idle) process for NON-interactive
// runs: it installs the per-run TLS-MITM CA (when delivered) and then idles while
// agent-run's task Exec does the real work. (Interactive runs use `agent-run
// --idle` as their main process instead — see CreateSandbox below — which performs
// this same CA install plus workspace prep; the human then drives claude in the
// attach shell.) The CA install is REQUIRED either way: without it,
// NODE_EXTRA_CA_CERTS points at a CA file that was never written, so claude cannot
// trust the proxy's TLS termination of api.anthropic.com (breaking subscription
// proxy-side injection). It writes the EXACT paths
// internal/api pins (/tmp/wardyn — any-uid-writable, so it works regardless of
// the image's USER/HOME; this Cmd may run as root while agent-run later re-runs
// as the image user, hence the sticky-bit dir and the ||true rewrites: within
// one run the content is identical, so a failed rewrite over a correct file is
// harmless). It also assembles the COMBINED bundle (system roots + per-run CA)
// that SSL_CERT_FILE/REQUESTS_CA_BUNDLE/CURL_CA_BUNDLE point at — those vars
// REPLACE the client trust store, so the bare CA there would break non-MITM'd
// CONNECT-tunneled hosts. Keep in lockstep with install_mitm_ca in
// deploy/images/common/agent-run-lib.sh. No-op when the run did not opt into
// TLS-MITM (WARDYN_MITM_CA_PEM unset).
const agentIdleScript = `d=/tmp/wardyn
if [ -n "${WARDYN_MITM_CA_PEM:-}" ]; then
  mkdir -p "$d" 2>/dev/null; chmod 1777 "$d" 2>/dev/null || true
  { printf '%s\n' "$WARDYN_MITM_CA_PEM" > "$d/mitm-ca.pem" && chmod 0644 "$d/mitm-ca.pem"; } 2>/dev/null || true
  sys=""
  for c in /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem /etc/pki/tls/certs/ca-bundle.crt; do
    [ -f "$c" ] && sys="$c" && break
  done
  if [ -n "$sys" ]; then
    { cat "$sys" "$d/mitm-ca.pem" > "$d/ca-bundle.pem" && chmod 0644 "$d/ca-bundle.pem"; } 2>/dev/null || true
  else
    { cp "$d/mitm-ca.pem" "$d/ca-bundle.pem" && chmod 0644 "$d/ca-bundle.pem"; } 2>/dev/null || true
    echo "wardyn: no system CA bundle found; ca-bundle.pem is proxy-CA-only (non-MITM TLS hosts will not verify)" >&2
  fi
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

// New constructs a Driver against the host Docker daemon. API-version negotiation
// with the server is on by default in the moby v29 client (forward/backward compat).
func New(cfg Config) (*Driver, error) {
	cli, err := client.New(
		client.FromEnv,
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
		creating:   make(map[string]bool),
	}
}

func (d *Driver) Name() string { return driverName }

// Classes probes the daemon for available runtimes and reports the Confinement
// Classes this host can actually enforce, with the per-class substrate label.
func (d *Driver) Classes(ctx context.Context) (substrate.ClassSupport, error) {
	infoRes, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return substrate.ClassSupport{}, fmt.Errorf("docker: info: %w", err)
	}
	c := capabilitiesForWith(infoRes.Info, d.cfg.ConfinementRuntimes)
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
	proxyResp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     proxyCfg,
		HostConfig: proxyHost,
		Name:       proxyContainerName(spec.RunID),
	})
	if err != nil {
		return fail(fmt.Errorf("docker: create proxy: %w", err))
	}
	rollback = append(rollback, func() {
		_, _ = d.cli.ContainerRemove(context.Background(), proxyResp.ID, client.ContainerRemoveOptions{Force: true})
	})

	// Connect the proxy to the control-plane-facing network so it can reach
	// the control plane. This network is the ONLY route off the per-run
	// segment, and only the proxy is on it.
	if _, err := d.cli.NetworkConnect(ctx, d.cfg.InternalNetwork, client.NetworkConnectOptions{
		Container:      proxyResp.ID,
		EndpointConfig: &network.EndpointSettings{},
	}); err != nil {
		return fail(fmt.Errorf("docker: connect proxy to %s: %w", d.cfg.InternalNetwork, err))
	}

	if _, err := d.cli.ContainerStart(ctx, proxyResp.ID, client.ContainerStartOptions{}); err != nil {
		return fail(fmt.Errorf("docker: start proxy: %w", err))
	}

	// Resolve the proxy's IP on the per-run internal network so the agent can
	// reach it via a static /etc/hosts entry instead of Docker's embedded DNS.
	// gVisor's netstack (CC2/runsc) does NOT traverse the embedded resolver at
	// 127.0.0.11, so resolving the "wardyn-proxy" container alias fails under
	// runsc (the agent then cannot reach its only egress path). A static hosts
	// entry works under every runtime and weakens nothing: the agent still has no
	// default route — the proxy remains its sole path off the gatewayless segment.
	proxyInspectRes, err := d.cli.ContainerInspect(ctx, proxyResp.ID, client.ContainerInspectOptions{})
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

	if _, err := d.cli.ContainerStart(ctx, agentResp.ID, client.ContainerStartOptions{}); err != nil {
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
	dirs := []string{defaultCastDir}
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
	created, err := d.cli.ExecCreate(ctx, ref, client.ExecCreateOptions{
		User: "0:0", // root, regardless of the image's default USER
		Cmd:  append([]string{"mkdir"}, args...),
	})
	if err != nil {
		return
	}
	if _, err := d.cli.ExecStart(ctx, created.ID, client.ExecStartOptions{}); err != nil {
		return
	}
	d.waitExec(ctx, created.ID)

	chmodCreated, err := d.cli.ExecCreate(ctx, ref, client.ExecCreateOptions{
		User: "0:0",
		Cmd:  append([]string{"chmod", "0777"}, dirs...),
	})
	if err != nil {
		return
	}
	if _, err := d.cli.ExecStart(ctx, chmodCreated.ID, client.ExecStartOptions{}); err != nil {
		return
	}
	d.waitExec(ctx, chmodCreated.ID)
}

// waitExec briefly polls a one-shot exec to completion so a subsequent Exec
// (which races right after) observes the prepared directories. Bounded so a
// stuck exec cannot stall sandbox bring-up.
func (d *Driver) waitExec(ctx context.Context, execID string) {
	for i := 0; i < 50; i++ {
		insp, ierr := d.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
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
	return recorderArgv(castDir, outDir, uploadURL, runID, argv)
}

// Exec launches the agent process inside the sandbox with a TTY attached.
// When recording, the argv is wrapped by wardyn-rec (which execs asciinema or
// falls back to a .log). Returns once the process is started, not finished.
func (d *Driver) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("docker: exec: empty argv")
	}
	// EXEC-LESS path: a deferred (krun) agent is created NOW with the workload as
	// its main process — there is no exec to attach, and the container IS the agent
	// (empty exec id => the reconciler uses container Status for liveness).
	// CLAIM the pending entry (delete + mark creating) in ONE critical section so a
	// concurrent teardown cannot observe a ref that is neither pending nor created.
	d.mu.Lock()
	p, isPending := d.pending[ref]
	if isPending {
		delete(d.pending, ref)
		d.creating[ref] = true
	}
	d.mu.Unlock()
	if isPending {
		return "", d.runAsMainProcess(ctx, ref, p, argv)
	}
	cmd := argv
	if d.cfg.Record {
		// Recover the run id from the container label so wardyn-rec names the
		// recording deterministically. If we cannot, record under a nil id
		// rather than failing the exec.
		runID := uuid.Nil
		if insp, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{}); err == nil && insp.Container.Config != nil {
			if id, perr := parseRunID(insp.Container.Config.Labels[labelRun]); perr == nil {
				runID = id
			}
		}
		cmd = d.recordCmd(runID, defaultCastDir, argv)
	}
	execCfg := client.ExecCreateOptions{
		TTY:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	}
	created, err := d.cli.ExecCreate(ctx, ref, execCfg)
	if err != nil {
		return "", fmt.Errorf("docker: exec create: %w", err)
	}
	// Track this exec id as the agent process for ref so Wait can observe its
	// completion + exit code. The latest Exec for a ref wins (re-exec replaces).
	d.mu.Lock()
	d.agentExecs[ref] = created.ID
	d.mu.Unlock()
	// Attach (TTY hijack) so output can be wired to wardyn-rec / a sink. The
	// caller (control plane) owns the lifecycle of the returned stream; for v0
	// we start detached after establishing the attach to confirm liveness.
	attachRes, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		return "", fmt.Errorf("docker: exec attach: %w", err)
	}
	resp := attachRes.HijackedResponse
	// We do not block on the process; drain in the background so the PTY does
	// not stall. A real recorder pipeline replaces this drain.
	go func() {
		defer resp.Close()
		_, _ = io.Copy(io.Discard, resp.Reader)
	}()
	return created.ID, nil
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
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           p.cfg,
		HostConfig:       p.host,
		NetworkingConfig: p.netcfg,
		Name:             ref,
	})
	if err != nil {
		d.mu.Lock()
		delete(d.creating, ref)
		d.mu.Unlock()
		return fmt.Errorf("docker: create main-process agent: %w", err)
	}
	_, startErr := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	// The container now exists on the daemon, so re-check the claim: a teardown
	// that ran during the create found NO container to remove and reported
	// idempotent success, so removing this one is our job — a killed sandbox must
	// never end up with a live agent.
	d.mu.Lock()
	tornDown := !d.creating[ref]
	delete(d.creating, ref)
	if !tornDown && startErr == nil {
		d.mainProc[ref] = true
	}
	d.mu.Unlock()
	if tornDown {
		// Not ctx: the kill cascade that tore this ref down may already have
		// cancelled it, and the removal must still happen (fail closed).
		rmCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		if _, rerr := d.cli.ContainerRemove(rmCtx, created.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil && !isNotFound(rerr) {
			return fmt.Errorf("docker: sandbox %s torn down during agent create, and removing the created agent failed: %w", ref, rerr)
		}
		return fmt.Errorf("docker: sandbox %s torn down during agent create", ref)
	}
	if startErr != nil {
		return fmt.Errorf("docker: start main-process agent: %w", startErr)
	}
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
	errs := 0
	for {
		insp, err := d.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		switch {
		case err == nil:
			errs = 0
			if !insp.Running {
				return insp.ExitCode, nil
			}
		case isNotFound(err):
			return 0, fmt.Errorf("docker: wait: exec inspect: %w", err)
		default:
			if errs++; errs >= waitMaxProbeErrors {
				return 0, fmt.Errorf("docker: wait: exec inspect (%d consecutive errors): %w", errs, err)
			}
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
	errs := 0
	for {
		// v29: ContainerWait returns a single result carrying both the status and
		// error channels (was a bare two-channel return); the select is otherwise
		// unchanged.
		wait := d.cli.ContainerWait(ctx, ref, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
		var err error
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("docker: wait (main process): %w", ctx.Err())
		case res := <-wait.Result:
			if res.Error != nil {
				return int(res.StatusCode), fmt.Errorf("docker: wait (main process): %s", res.Error.Message)
			}
			return int(res.StatusCode), nil
		case err = <-wait.Error:
		}
		// Same budget as the exec path: a transient daemon error is not an exit.
		if isNotFound(err) {
			return 0, fmt.Errorf("docker: wait (main process): %w", err)
		}
		if errs++; errs >= waitMaxProbeErrors {
			return 0, fmt.Errorf("docker: wait (main process) (%d consecutive errors): %w", errs, err)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("docker: wait (main process): %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func (d *Driver) Status(ctx context.Context, ref string) (runner.Status, error) {
	res, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		if isNotFound(err) {
			return runner.Status{State: types.RunStopped, Message: "container not found"}, nil
		}
		return runner.Status{}, fmt.Errorf("docker: inspect: %w", err)
	}
	return statusFromInspect(res.Container), nil
}

// AgentStatus reports the agent's liveness in a restart-safe way. For an
// exec-based ref (agentExecID != "") it inspects that exec: Running => alive
// (RUNNING); exited => terminal with the real exit code, EVEN while the idle
// sandbox container is still up — the case container Status cannot detect after a
// restart dropped the in-memory exec map. A vanished exec (its
// container already gone) reads as stopped => the reconciler finalizes + tears
// down. When agentExecID is "" (exec-less/main-process, or Exec never ran) the
// container IS the agent, so fall back to Status.
func (d *Driver) AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	if agentExecID == "" {
		return d.Status(ctx, ref)
	}
	insp, err := d.cli.ExecInspect(ctx, agentExecID, client.ExecInspectOptions{})
	if err != nil {
		if isNotFound(err) {
			return runner.Status{State: types.RunStopped, Message: "agent exec not found"}, nil
		}
		return runner.Status{}, fmt.Errorf("docker: agent exec inspect: %w", err)
	}
	if insp.Running {
		return runner.Status{State: types.RunRunning}, nil
	}
	code := insp.ExitCode
	return runner.Status{State: types.RunStopped, ExitCode: &code}, nil
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

	if _, err := d.cli.ContainerRemove(ctx, proxyContainerName(id), client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove proxy: %w", err)
	}
	if _, err := d.cli.NetworkRemove(ctx, internalNetName(id), client.NetworkRemoveOptions{}); err != nil && !isNotFound(err) {
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
	// absent locally (not a stale tag). The demo agent tags (wardyn/agent-*:local)
	// live in no registry, so name the fix rather than leaking a bare
	// "registry: denied".
	if err := dockerutil.PullImage(ctx, d.cli, ref, "docker"); err != nil {
		return fmt.Errorf("%w (image %q not present locally and pull failed — for the demo images run: make agent-images)", err, ref)
	}
	return nil
}

func (d *Driver) imagePresent(ctx context.Context, ref string) (bool, error) {
	// A digest-pinned ref (repo@sha256:...) is not a tag, so the "reference" list
	// filter (which is tag-shaped) never matches it — check by inspect instead, so
	// a pre-pulled digest-pinned BYOI/private image reads present and short-circuits
	// the pull (which would otherwise re-hit a registry we may have no auth for).
	if strings.Contains(ref, "@sha256:") {
		if _, err := d.cli.ImageInspect(ctx, ref); err != nil {
			if isNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("docker: image inspect %q: %w", ref, err)
		}
		return true, nil
	}
	// v29: the filter set is client.Filters (was filters.Args); .Add returns the
	// populated map. Same "reference"=<ref> tag-shaped filter as before.
	res, err := d.cli.ImageList(ctx, client.ImageListOptions{Filters: client.Filters{}.Add("reference", ref)})
	if err != nil {
		return false, fmt.Errorf("docker: image list: %w", err)
	}
	return len(res.Items) > 0, nil
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
		GitGrants:        pc.GitGrants,
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
