// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
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
	// Docker runtime family that must back it — the operator knob that makes
	// CC3 substrate-pluggable across OCI runtimes. An empty/absent entry uses
	// the built-in default mapping (CC2->runsc, CC3->kata*); a pinned runtime
	// is still probed against `docker info` and FAILS CLOSED when absent
	// (never downgrades). Non-OCI VMM substrates are a future Runner driver.
	ConfinementRuntimes map[types.ConfinementClass]string
	// AllowUnenforceableCaps downgrades the fail-closed resource-cap probe to
	// a warning when the daemon can't enforce CPU/memory/pids limits (cgroup
	// controller missing/not delegated). OFF by default — an untrusted
	// sandbox must not run uncapped. Set only on a trusted host.
	AllowUnenforceableCaps bool
	// UserDriveHostRoots is the deployment's WARDYN_USER_DRIVE_HOST_ROOTS
	// ceiling over host_path USER DRIVES, parsed once at boot and handed to
	// the substrate constructor, so the driver's bind-time re-check and the
	// API's authoring-time check are the same operator-set list.
	//
	// DRIVER CONFIG rather than a SandboxSpec field, deliberately the opposite
	// of UserMountRoots: a drive's ceiling is per DEPLOYMENT (where this
	// daemon's operator mounted shares), not per-run like member roots. Empty
	// (the default) refuses every host_path drive — the whole posture.
	UserDriveHostRoots []string
	// DriveProbeImage is the OCI image ProbeDrive runs its short-lived
	// readability check in. Empty uses defaultDriveProbeImage (busybox-class).
	DriveProbeImage string
}

// RecordingMountTarget is where RecordingMount appears inside the agent
// container; wardyn-rec's -out-dir points here.
const RecordingMountTarget = "/wardyn/recordings"

// defaultCastDir is where wardyn-rec writes session recordings inside the
// agent container. stopTimeout is the graceful StopSandbox timeout. Nobody in
// the repo overrides either of these; re-add a Config knob if that changes.
const (
	defaultCastDir = "/var/log/wardyn"
	stopTimeout    = 10 * time.Second
)

// pollInterval is Wait's probe cadence, and waitMaxProbeErrors is how many
// CONSECUTIVE transient probe errors it tolerates before giving up (~1 min,
// matching the control plane's reconcileMaxProbeErrors budget).
//
// A transient daemon/API blip is NOT "the agent exited": Wait's error is
// terminal for its caller, stranding the run RUNNING with a live sandbox, so
// a blip must not surface as an error at all. A not-found is authoritative
// (the exec/container really is gone) and is never retried, so a
// kill/teardown still unblocks Wait immediately.
const (
	pollInterval       = 200 * time.Millisecond
	waitMaxProbeErrors = 300
)

func (c *Config) withDefaults() {
	if c.InternalNetwork == "" {
		c.InternalNetwork = "wardyn-internal"
	}
}

// Driver implements runner.Runner against the Docker Engine API.
type Driver struct {
	cli dockerAPI
	cfg Config

	// proxySettle is how long startProxy's exit-watch requires a proxy's
	// Running state to have HELD before trusting it. New() sets this to
	// proxyStartSettle for a real daemon; newWithClient's zero value (0) is
	// the deliberate default for fake-backed test drivers, so the unit suite
	// doesn't pay a real wall-clock tax on every CreateSandbox.
	proxySettle time.Duration

	// mu guards agentExecs, pending, mainProc, and creating — the driver's
	// only mutable state.
	mu sync.Mutex
	// agentExecs maps a sandbox ref to the exec id of the agent process Exec
	// started; Wait inspects it to observe completion + exit code. One entry
	// per ref (latest Exec wins).
	agentExecs map[string]string
	// pending holds the deferred agent-container config for EXEC-LESS runtimes
	// (krun microVMs): CreateSandbox can't pre-create a keep-alive container
	// to exec into, so it stashes the built config here and Exec creates the
	// container with the workload as its MAIN process.
	pending map[string]*pendingAgent
	// mainProc marks refs whose workload runs as the container main process
	// (exec-less path), so Wait blocks on container exit instead of an exec.
	mainProc map[string]bool
	// creating marks exec-less refs whose agent container is being created
	// RIGHT NOW by runAsMainProcess. The tombstone making the
	// pending->created transition atomic w.r.t. teardown: teardown finds no
	// container to remove in that window and deletes the mark instead, and
	// runAsMainProcess — seeing its mark gone — removes the container it just
	// made rather than leaving a killed run's agent alive.
	creating map[string]bool
	// execOutputs maps a sandbox ref to its SandboxSpec.ExecOutput (an
	// io.Writer); every agent Exec on the ref tees its output there, the BYOI
	// selftest included. Dropped at teardown.
	execOutputs sync.Map
}

// pendingAgent is the fully-built agent-container config CreateSandbox defers
// for an exec-less runtime; Exec sets its Cmd to the (recorder-wrapped)
// workload and creates the container.
type pendingAgent struct {
	cfg     *container.Config
	host    *container.HostConfig
	netcfg  *network.NetworkingConfig
	managed []runner.ManagedFile // delivered by runAsMainProcess, between ITS create and start
}

// agentImageHome is the Wardyn agent-image user's home directory, set as HOME
// on the exec-less (krun) path because libkrun runs the guest as root with
// HOME=/, so ~-relative paths (~/work, ~/.claude) would otherwise resolve
// under / and fail for a non-writable root.
const agentImageHome = "/home/agent"

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
// exec-less path: no root exec exists to pre-create the root-owned default
// cast dir, so wardyn-rec stages here and delivers via the proxy upload route.
const mainProcCastDir = "/tmp/wardyn-rec"

// Driver is the OCI/Docker confinement substrate; the orchestrator wraps it to
// present the runner.Runner surface to the control plane.
var _ substrate.Substrate = (*Driver)(nil)
var _ runner.SandboxEnder = (*Driver)(nil)
var _ runner.ProxyStopper = (*Driver)(nil)
var _ runner.Freezer = (*Driver)(nil)
var _ runner.ActivitySampler = (*Driver)(nil)

// New constructs a Driver against the host Docker daemon. API-version
// negotiation with the server is on by default in the moby v29 client.
func New(cfg Config) (*Driver, error) {
	cli, err := client.New(
		client.FromEnv,
	)
	if err != nil {
		return nil, fmt.Errorf("docker: new client: %w", err)
	}
	d := newWithClient(cli, cfg)
	d.proxySettle = proxyStartSettle
	return d, nil
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

// PrewarmImages is defined in prewarm.go.

func (d *Driver) Name() string { return driverName }

// Classes probes the daemon for available runtimes and reports the Confinement
// Classes this host can actually enforce, with the per-class substrate label.
func (d *Driver) Classes(ctx context.Context) (substrate.ClassSupport, error) {
	infoRes, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return substrate.ClassSupport{}, fmt.Errorf("docker: info: %w", err)
	}
	c := capabilitiesForWith(infoRes.Info, d.cfg.ConfinementRuntimes, d.cfg.Record)
	return substrate.ClassSupport{
		Classes:          c.ConfinementClasses,
		Resolved:         c.Resolved,
		StructuralEgress: c.StructuralEgress,
		NetworkPolicy:    c.NetworkPolicy,
		SessionRecording: c.SessionRecording,
		// This substrate binds a member's drive — driveMount resolves it and
		// ensureDriveVolume creates or adopts the named volume. True only
		// while that path exists: declaring it without the mount previews
		// green and fails at dispatch, and TestCreateSandbox_MountsAUserDrive
		// pins the two together.
		UserDrives:   true,
		ManagedFiles: true, // deliverManagedFiles, between create and start (managed_files.go)
		// What a run's disk_mib actually binds on this daemon: `filesystem`
		// when the storage driver can enforce a per-container quota, `none`
		// when it can't (warn-and-run-uncapped, or create-refused); see capabilitiesForWith.
		EphemeralDiskEnforcement: c.EphemeralDiskEnforcement,
		// Per-class Freeze/Thaw support — see capabilitiesForWith.
		Freeze: c.Freeze,
	}, nil
}

// ensureImage pulls ref if it is not already present locally. Pull output is
// drained and discarded; failures surface as errors (fail closed — never run
// a sandbox we couldn't provision).
//
// onPulling (nil-safe) fires immediately BEFORE the pull that blocks, and
// only there: imagePresent has just said the image is absent, so this is the
// one place a first download can be ASSERTED rather than hedged.
func (d *Driver) ensureImage(ctx context.Context, ref string, onPulling func()) error {
	present, err := d.imagePresent(ctx, ref)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if onPulling != nil {
		onPulling()
	}
	// imagePresent said false, so a pull failure means the image is genuinely
	// absent (not a stale tag) — see pullFailure for what that error says.
	if err := dockerutil.PullImage(ctx, d.cli, ref, "docker"); err != nil {
		return pullFailure(ref, err)
	}
	return nil
}

// ImagePresent implements runner.ImageChecker: exported imagePresent, so a
// caller holding only a runner.Runner can verify a cached image ref before trusting it.
func (d *Driver) ImagePresent(ctx context.Context, ref string) (bool, error) {
	return d.imagePresent(ctx, ref)
}

func (d *Driver) imagePresent(ctx context.Context, ref string) (bool, error) {
	// A digest-pinned ref (repo@sha256:...) isn't a tag, so the tag-shaped
	// "reference" filter never matches it — check by inspect instead, so a
	// pre-pulled digest-pinned private image short-circuits the pull.
	if strings.Contains(ref, "@sha256:") {
		if _, err := d.cli.ImageInspect(ctx, ref); err != nil {
			if isNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("docker: image inspect %q: %w", ref, err)
		}
		return true, nil
	}
	res, err := d.cli.ImageList(ctx, client.ImageListOptions{Filters: client.Filters{}.Add("reference", ref)})
	if err != nil {
		return false, fmt.Errorf("docker: image list: %w", err)
	}
	return len(res.Items) > 0, nil
}

// ImageRemove implements runner.ImageRemover: reclaims a workspace-built
// image tag superseded by a rescan/edit/delete. A ref already absent is not
// an error — idempotent, same contract as StopSandbox — and a ref still in
// USE (e.g. a concurrently-running sandbox) fails soft rather than yanking an
// image out from under a live run: the caller logs and moves on.
func (d *Driver) ImageRemove(ctx context.Context, ref string) error {
	if _, err := d.cli.ImageRemove(ctx, ref, client.ImageRemoveOptions{}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("docker: image remove %q: %w", ref, err)
	}
	return nil
}
