// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A macOS VM sandbox (machine_kind macos_vm) runs on the Mac itself, not in Docker: one wardyn-macvm
// helper per VM, started in its own session so the VM outlives a runnerd restart or upgrade, with the
// run's usual network and proxy in Docker and the run's wardyn-vmnet relay as the guest's only network.
// The relay's attach stream is handed to the helper as a file descriptor, so the helper owns the bridge.
// Every VM holds an entry in the host-wide admission ledger from before its helper starts until its
// destruction is verified. A macOS VM has no agent container: its ref is the agent container name all
// the same, and it is found again after a restart by its run directory.

// macHelperExitWait bounds the wait for a helper to exit after its VM stopped, before it is killed.
const macHelperExitWait = 15 * time.Second

// macDestroyTimeout bounds a macOS VM destroy, which runs apart from the call that asked for it: the stop
// (its timeout, and one helper wait for the dial), then a helper wait before the kill and one after it, plus
// slack for the ledger reads and poll overshoot so the wait after the kill keeps its whole bound.
const macDestroyTimeout = macvm.DefaultStopTimeout + 3*macHelperExitWait + 5*time.Second

// MacVMConfig turns macOS VM sandboxes on. wardyn-runnerd fills it on a Mac whose operator turned them on.
type MacVMConfig struct {
	// Layout is the user's macos-vm.noindex tree: the base in bases/0, run directories in runs/.
	Layout macvm.Layout
	// Helper is the wardyn-macvm binary.
	Helper string
	// Identity is the machine-identifier strategy every run uses.
	Identity macvm.IdentityStrategy
	// SockRoot is the socket root the helper makes each run's socket directory under ("" is
	// admission.DefaultSockDir), written into each run's run.json (macvm_sock.go).
	SockRoot string
	// ToolsDialFile, when set, is where the last run boot's tools-port dial class is kept for
	// `macos-vm status` (recordToolsDial).
	ToolsDialFile string
	// Ledger is this runner's handle on the host admission ledger; LedgerDir is its directory, written
	// into each run's run.json for the helper ("" is the package default).
	Ledger    *admission.Ledger
	LedgerDir string
	// Host reads the Mac's process table, so a helper is known gone by its pid and start time.
	Host     admission.Host
	RunnerID string
	// HostOS is the Mac's own macOS build, recorded on every run's machine.
	HostOS string
	// BootTimeout bounds a VM's boot to its guest agent's first answer; zero is ten minutes.
	BootTimeout time.Duration
	// Changed is called after each ledger transaction the driver makes, so the runner recomputes the
	// max it offers and resends its caps.
	Changed func()

	// spawn starts the helper for a run dir and signal signals a helper; tests replace both.
	spawn  func(runDir string) error
	signal func(pid int, sig syscall.Signal) error
	// sockFstat replaces fstat(2) in the socket directory checks and sweepSock the socket residue sweep
	// (macvm.SweepSockResidue); tests replace both.
	sockFstat func(fd int, name string) (unix.Stat_t, error)
	sweepSock func(l macvm.Layout, sockRoot string) ([]string, error)
	// removeAll deletes a run directory (os.RemoveAll), and helperWait bounds the wait for a helper's exit
	// (zero is macHelperExitWait); tests replace both.
	removeAll  func(path string) error
	helperWait time.Duration
}

// runDir is the run's directory, named by its run key (the run id's first 12 hex digits).
func (c *MacVMConfig) runDir(runID uuid.UUID) string { return c.Layout.RunDir(runID.String()) }

func (c *MacVMConfig) changed() {
	if c.Changed != nil {
		c.Changed()
	}
}

// EnableMacVM turns macOS VM sandboxes on; a nil config turns them off.
func (d *Driver) EnableMacVM(c *MacVMConfig) error {
	if c != nil {
		if c.Ledger == nil || c.Host == nil || c.Helper == "" || c.Layout.Root == "" || c.RunnerID == "" {
			return errors.New("docker: macOS VMs need a ledger, a host, the helper, a VM directory and the runner id")
		}
		// The sockets live in the socket directory, not the run directory: the guard is its length.
		if n := len(filepath.Join(c.sockDir(uuid.Nil), macvm.DesktopSock)); n > macvm.MaxSocketPath {
			return fmt.Errorf("docker: the macOS VM socket directory %q is too long for its sockets' paths (%d bytes, at most %d)", c.sockDir(uuid.Nil), n, macvm.MaxSocketPath)
		}
		if c.BootTimeout == 0 {
			c.BootTimeout = 10 * time.Minute
		}
		if c.spawn == nil {
			c.spawn = func(dir string) error { return startHelper(c.Helper, dir) }
		}
		if c.signal == nil {
			c.signal = syscall.Kill
		}
		if c.sweepSock == nil {
			c.sweepSock = macvm.SweepSockResidue
		}
		if c.removeAll == nil {
			c.removeAll = os.RemoveAll
		}
	}
	d.macCfg.Store(c)
	return nil
}

func (d *Driver) macVMConfig() *MacVMConfig { return d.macCfg.Load() }

var _ runner.KindCoverer = (*Driver)(nil)

// CoversKind is runner.KindCoverer: containers and Linux VMs are inspected as containers, and macOS
// VMs only while they are on (macVMConfig); with them off, Status would read every one as absent.
func (d *Driver) CoversKind(kind types.MachineKind) bool {
	switch kind.Normalize() {
	case "", types.MachineLinuxVM:
		return true
	case types.MachineMacOSVM:
		return d.macVMConfig() != nil
	}
	return false
}

// ReclaimMacVMs reconciles this runner's macOS VM ledger entries whose VM and run directory are gone
// (reclaimGone); wardyn-runnerd runs it with every recompute of its offer.
func (d *Driver) ReclaimMacVMs() error {
	c := d.macVMConfig()
	if c == nil {
		return nil
	}
	return c.reclaimGone()
}

// startHelper starts `wardyn-macvm run --dir <runDir>` in a new session: launchd ends the job's process
// group when runnerd's job ends, and the VM must outlive that.
func startHelper(helper, runDir string) error {
	cmd := exec.Command(helper, "run", "--dir", runDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker: start the macOS VM helper: %w", err)
	}
	go func() { _ = cmd.Wait() }() // reaped here while this runnerd lives; launchd adopts it after
	return nil
}

// newMacVMRun is a macOS VM run's handle: its guest agent is dialled on agent.sock in the run's socket
// directory, checked on every dial.
func newMacVMRun(c *MacVMConfig, runID uuid.UUID, dir string) *vmRun {
	ctx, stop := context.WithCancel(context.Background())
	var nd net.Dialer
	return &vmRun{runID: runID, dir: dir, mac: true, ctx: ctx, stop: stop,
		agent: guestagent.NewClient(func(ctx context.Context) (net.Conn, error) {
			sock, err := c.sockPath(runID, macvm.AgentSock)
			if err != nil {
				return nil, err
			}
			return nd.DialContext(ctx, "unix", sock)
		})}
}

// macVMLookup finds a macOS VM by its run directory, as a runner restart leaves it: the ref names the run,
// and the directory's run.json names the same run. Nil when macOS VMs are off or the ref is no such run.
func (d *Driver) macVMLookup(ref string) *vmRun {
	c := d.macVMConfig()
	if c == nil {
		return nil
	}
	id, err := runIDFromAgentName(ref)
	if err != nil {
		return nil
	}
	dir := c.runDir(id)
	if _, err := readMacRun(dir, id); err != nil {
		return nil
	}
	if v, ok := d.vms.Load(ref); ok {
		return v.(*vmRun)
	}
	nv := newMacVMRun(c, id, dir)
	v, loaded := d.vms.LoadOrStore(ref, nv)
	if loaded { // another caller stored it first: this one's context and client go
		nv.stop()
		nv.agent.Close()
	}
	return v.(*vmRun)
}

// readMacRun reads a run directory's run.json and checks it names runID in full: the directory's name is
// only the run id's first 12 hex digits, so every open checks the whole id.
func readMacRun(dir string, runID uuid.UUID) (macvm.RunConfig, error) {
	cfg, err := macvm.ReadRunConfig(dir)
	if err == nil && cfg.RunID != runID.String() {
		err = fmt.Errorf("docker: the macOS VM run directory %s belongs to run %s, not %s", dir, cfg.RunID, runID)
	}
	return cfg, err
}

// The texts a macOS VM run ends with: a helper gone for no reason the runner knows is macVMHelperStopped,
// and a VM that stopped with no error of its own is macVMStopped.
const (
	macVMLostNetwork   = "The macOS VM lost its network."
	macVMRestarted     = "The Mac restarted, so the macOS VM was lost."
	macVMHelperStopped = "The macOS VM's helper stopped."
	macVMStopped       = "The macOS VM stopped."
	macVMUpgraded      = "This Mac's runner was upgraded while the macOS VM ran, so the run was ended."
)

// macVMLowDisk is the text of a run the low-disk check ended; freeGB is what the check read (the run's
// macLowDiskFile). A file that does not hold a number leaves the free space out rather than guess it.
func macVMLowDisk(freeGB string) string {
	if n, err := strconv.Atoi(strings.TrimSpace(freeGB)); err == nil {
		return fmt.Sprintf("This Mac ran low on disk (%d GB free), so the macOS VM was stopped.", n)
	}
	return "This Mac ran low on disk, so the macOS VM was stopped."
}

// refuseMacVMSpec refuses, before anything is created, what a macOS VM cannot honour, and a run that a
// stale cap let through: the base must be ready, serve every harness the run starts, and fit the run's
// size, checked again here against the base's own manifest.
func (d *Driver) refuseMacVMSpec(spec runner.SandboxSpec) (macvm.Manifest, error) {
	c := d.macVMConfig()
	switch {
	case c == nil:
		return macvm.Manifest{}, errors.New("docker: this runner does not offer macOS VM sandboxes")
	case len(spec.Drives) > 0:
		//lint:ignore ST1005 the sentence the person reads, as written in the approved copy
		return macvm.Manifest{}, errors.New("A macOS VM sandbox has no drives in this release.")
	case spec.RecordWithin(d.cfg.Record):
		//lint:ignore ST1005 the sentence the person reads, as written in the approved copy
		return macvm.Manifest{}, errors.New("A macOS VM sandbox's terminal isn't recorded in this release, and the organisation asked to record this run.")
	case spec.Interactive && spec.Env[envInteractiveStart] == "agent":
		//lint:ignore ST1005 the sentence the person reads, as written in the approved copy
		return macvm.Manifest{}, errors.New("A macOS VM sandbox's terminal opens a shell, not the agent CLI, in this release. Set the interactive start to shell.")
	}
	if err := d.preflightSpec(spec); err != nil {
		return macvm.Manifest{}, err
	}
	// The base the run names: Standard, or one of the Mac's others (macvm.NamedBaseDir).
	base, err := c.Layout.NamedBaseDir(spec.MacOSBase)
	if err != nil {
		return macvm.Manifest{}, fmt.Errorf("docker: %w", err)
	}
	man, err := macvm.ReadyBase(base)
	if err != nil {
		return man, fmt.Errorf("docker: %w", err)
	}
	return man, macVMFits(spec, man)
}

// macVMFits is the start's own check of the run against the base (the org checked the caps).
func macVMFits(spec runner.SandboxSpec, man macvm.Manifest) error {
	harnesses := strings.Fields(spec.Env["WARDYN_HARNESSES"])
	for _, h := range harnesses {
		if h != "none" && !slices.Contains(man.Harnesses, h) {
			//lint:ignore ST1005 the sentence the run's owner reads, as the organisation's own refusal words it
			return fmt.Errorf("This Mac's macOS base can't run %s right now.", h)
		}
	}
	// The size refusals say the shortfall only: how to raise a limit is the organisation's to word.
	if cpus := int(spec.Resources.CPUMillis / 1000); spec.Resources.CPUMillis > 0 && cpus < man.MinCPUs {
		//lint:ignore ST1005 as above
		return fmt.Errorf("Per-run CPUs on this runner are %d; a macOS VM needs at least %d.", cpus, man.MinCPUs)
	}
	if mem := uint64(max(spec.Resources.MemoryMiB, 0)); spec.Resources.MemoryMiB > 0 && mem < man.MinMemoryMiB {
		need := (man.MinMemoryMiB + 1023) / 1024
		//lint:ignore ST1005 as above
		return fmt.Errorf("Per-run memory on this runner is %d GB; a macOS VM needs at least %d GB.", mem/1024, need)
	}
	return nil
}

// macGuestInit is what a macOS guest's agent is handed: the sandbox's env and files, and the run's CA as a
// file at the guest's own CA path, whose variables the agent sets. The Linux paths the dispatcher put in
// the CA variables (/tmp/wardyn) are not forwarded: no such file exists in a macOS guest, and a tool
// pointed at one fails every TLS handshake. Every other path, in the env and the managed files, goes
// through the guest path table (macvm_guestpaths.go); one it cannot place refuses the run.
func macGuestInit(spec runner.SandboxSpec, trustedPEM string) (guestagent.Init, error) {
	linux := map[string]string{}
	runner.SetSandboxCATrustVars(linux, false)
	env := make(map[string]string, len(spec.Env))
	for k, v := range spec.Env {
		if lv, ok := linux[k]; ok && v == lv {
			continue
		}
		env[k] = v
	}
	// The pinned harness CLIs are root's under /usr/local/bin, so the run's agent user cannot update them:
	// an update comes only from a new pin (a tools update), as on the Linux images.
	env["DISABLE_AUTOUPDATER"] = "1"
	if err := macGuestEnv(env); err != nil {
		return guestagent.Init{}, err
	}
	files, err := macGuestFiles(spec.ManagedFiles)
	if err != nil {
		return guestagent.Init{}, err
	}
	if v := macGuestFilesEnv(files); v != "" {
		env[guestagent.EnvManagedFiles] = v
	}
	// RunCA is the run proxy's own CA alone, which the guest trusts in its System keychain; the
	// operator's CAs reach the guest only in the bundle file below.
	in := guestagent.Init{Env: env, SecretEnv: spec.SecretEnv, SignInsOff: spec.DesktopSignInsOff, Files: files,
		RunCA: strings.TrimSpace(spec.ProxyConfig.MITMCACertPEM)}
	if ca := vmCA(spec, trustedPEM); ca != "" {
		in.Files = append(in.Files, guestagent.File{Path: guestagent.MacCABundlePath, Mode: 0o644, Content: []byte(ca)})
	}
	return in, nil
}

// admitMacVM takes the run's ledger entry: the tombstone pass first, so another login's dead VM no longer
// holds the slot, then the admission, which is the authority on host capacity.
func (c *MacVMConfig) admitMacVM(req admission.Request) (admission.Entry, error) {
	if _, err := c.Ledger.Sweep(); err != nil {
		return admission.Entry{}, fmt.Errorf("docker: the macOS VM ledger: %w", err)
	}
	e, err := c.Ledger.Admit(req)
	c.changed()
	if err != nil {
		return e, fmt.Errorf("docker: %w", err)
	}
	return e, nil
}

// createMacVM is CreateSandbox for a macOS VM. Everything created before an error is rolled back; the
// ledger entry goes last, and only by the rule every teardown follows (destroyMacVM).
//
//nolint:funlen // One linear assembly sequence whose rollback must stay in one scope, as createVM's.
func (d *Driver) createMacVM(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	man, err := d.refuseMacVMSpec(spec)
	if err != nil {
		return runner.Sandbox{}, err
	}
	// Folders are checked before anything exists, as createVM checks its mounts.
	shares, guestMounts, err := d.macShares(ctx, spec)
	if err != nil {
		return runner.Sandbox{}, err
	}
	c := d.macVMConfig()
	cpus := cmp.Or(int(spec.Resources.CPUMillis/1000), man.MinCPUs)
	memMiB := cmp.Or(uint64(max(spec.Resources.MemoryMiB, 0)), man.MinMemoryMiB)
	self := os.Getpid()
	start, err := admission.Self(c.Host, self)
	if err != nil {
		return runner.Sandbox{}, fmt.Errorf("docker: the macOS VM ledger: %w", err)
	}

	var rollback []func()
	// failed is the failure token of the start phase the create is in (runner.MacOS*Failed): an error
	// says the start stopped there, as the run's last status line, before anything is rolled back.
	failed := ""
	phase := func(reason, failure string) {
		spec.NotifyWaiting(runner.MacOSStartDetail(reason))
		failed = failure
	}
	fail := func(err error) (runner.Sandbox, error) {
		if failed != "" {
			spec.NotifyWaiting(runner.MacOSStartDetail(failed))
		}
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
		return runner.Sandbox{}, err
	}
	dir, err := c.makeRunDir(spec.RunID)
	if err != nil {
		return runner.Sandbox{}, err
	}
	// Until the VM's destruction joins the rollback, the run directory is this create's to delete. From then on
	// destroyMacVM alone decides: it keeps the directory, and the ledger entry, while the helper may be alive.
	destroys := false
	rollback = append(rollback, func() {
		if !destroys {
			_ = os.RemoveAll(dir)
		}
	})
	// The run's network and proxy are what the start hands the VM (MacOSStarting), so a failure here is
	// the start's, said before any clone began.
	failed = runner.MacOSStartFailed
	_, proxyIP, undoNet, err := d.runNetworkAndProxy(ctx, spec)
	if err != nil {
		return fail(err)
	}
	rollback = append(rollback, undoNet)
	phase(runner.MacOSCloning, runner.MacOSCloneFailed)
	entry, err := c.admitMacVM(admission.Request{Kind: admission.KindRun, RunID: spec.RunID.String(), GuestCPUs: cpus,
		GuestMemory: memMiB << 20, DiskReserve: admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes)), OwnerPID: self, OwnerStart: start})
	if err != nil {
		if short := (*admission.Refusal)(nil); errors.As(err, &short) && short.DiskNeeded > 0 {
			spec.NotifyWaiting(runner.MacOSDiskShortMessage(short.DiskNeeded, short.DiskFreeLeft))
			failed = ""
		}
		return fail(err)
	}
	v := newMacVMRun(c, spec.RunID, dir)
	ref := agentContainerName(spec.RunID)
	// From here the VM (or its helper) may exist: the rollback is the teardown's own destruction.
	destroys = true
	rollback = append(rollback, func() {
		v.stop()
		v.agent.Close()
		d.destroyMacVM(context.Background(), v, entry.ID, false)
	})
	base, _ := c.Layout.NamedBaseDir(spec.MacOSBase) // refuseMacVMSpec checked the name
	if err := macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: spec.RunID.String(), BaseDir: base, CPUs: cpus, MemoryMiB: memMiB,
		Identity: c.Identity, RunnerID: c.RunnerID, LedgerEntry: entry.ID, LedgerDir: c.LedgerDir, SockRoot: c.SockRoot,
		Shares: shares}); err != nil {
		return fail(fmt.Errorf("docker: macOS VM run directory: %w", err))
	}
	if err := c.spawn(dir); err != nil {
		return fail(err)
	}
	// The clone comes first. The socket is waited for where the helper makes it, and its directory is
	// checked before the dial (dialControl).
	if err := waitForSocket(ctx, filepath.Join(c.sockDir(spec.RunID), macvm.ControlSock), 2*time.Minute); err != nil {
		return fail(fmt.Errorf("docker: the macOS VM helper did not start: %w", err))
	}
	cc, err := c.dialControl(ctx, spec.RunID)
	if err != nil {
		return fail(fmt.Errorf("docker: the macOS VM helper: %w", err))
	}
	defer cc.Close()
	phase(runner.MacOSStarting, runner.MacOSStartFailed)
	// The guest's only network: the run's relay, whose attach stream the helper bridges. The VM starts
	// only once the helper holds it.
	attach, err := d.startMacVMNet(ctx, spec, proxyIP)
	if err != nil {
		return fail(err)
	}
	err = cc.SendNetFD(ctx, attach)
	_ = attach.Close()
	if err != nil {
		return fail(fmt.Errorf("docker: hand the macOS VM its network: %w", err))
	}
	bctx, cancel := context.WithTimeout(ctx, c.BootTimeout)
	defer cancel()
	if err := waitMacVMRunning(bctx, cc); err != nil {
		return fail(err)
	}
	if _, err := c.Ledger.Update(entry.ID, func(e *admission.Entry) { e.State = admission.StateRunning }); err != nil {
		slog.Warn("docker: mark a macOS VM running in the ledger", slog.String("run_id", spec.RunID.String()), slog.String("err", err.Error()))
	}
	c.changed()
	phase(runner.MacOSConnecting, runner.MacOSConnectFailed)
	// The desktop's per-run password: from the helper to the guest agent, in memory only (never logged,
	// never written). A base with a desktop publishes the run ready only once the guest set it (ERRATA E5):
	// a helper that gives none, or a guest that did not set it, is the run's named failure
	// (macvm.ErrDesktopPassword), never a ready run without its password. A run that wants no sign-ins
	// kept has no desktop at all (the guest disables it and sets no password), so it runs agent-only.
	var password string
	desktop := man.Desktop != "" && man.Desktop != "none" && !spec.DesktopSignInsOff
	if desktop {
		if password, err = cc.DesktopPassword(bctx); err != nil {
			slog.Warn("docker: the macOS VM's helper gave no desktop password", slog.String("run_id", spec.RunID.String()), slog.String("err", err.Error()))
			return fail(fmt.Errorf("docker: %w", macvm.ErrDesktopPassword))
		}
	}
	res, err := d.bootMacVM(bctx, v, cc, spec, guestMounts, password)
	password = ""
	if err != nil {
		return fail(err)
	}
	set := res.DesktopPasswordSet != nil && *res.DesktopPasswordSet
	if desktop && !set {
		return fail(fmt.Errorf("docker: %w", macvm.ErrDesktopPassword))
	}
	go c.recordToolsDial(v.ctx, v.agent)
	if !set {
		if err := os.WriteFile(filepath.Join(dir, macDesktopOffFile), nil, 0o600); err != nil {
			return fail(fmt.Errorf("docker: macOS VM run directory: %w", err))
		}
	}
	signIns := res.SignIns
	machine := &types.VMMachine{Accelerator: types.MacVMAccelerator, MachineType: types.MacVMMachineType,
		GuestOS: cmp.Or(man.GuestOS, "macOS "+man.BuildVersion), HostOS: c.HostOS, BaseEditedAt: macvm.BaseEditedAt(c.Layout.BaseDir(0), man)}
	machine.MacOSBase = cmp.Or(spec.MacOSBase, macvm.StandardBase)
	d.notVMs.Delete(ref)
	d.vms.Store(ref, v)
	d.keepExecOutput(ref, spec.ExecOutput)
	return runner.Sandbox{Ref: ref, Driver: driverName, EnforcedClass: types.CC3, Machine: machine, SignIns: signIns,
		KeychainTrust: macKeychainTrust(res)}, nil
}

// makeRunDir creates a run's directory, new: a directory already there (another run's whose id shares
// the first 12 hex digits, or one a crash left) is refused, never reused.
func (c *MacVMConfig) makeRunDir(runID uuid.UUID) (string, error) {
	if err := os.MkdirAll(c.Layout.RunsDir(), 0o700); err != nil {
		return "", fmt.Errorf("docker: macOS VM run directory: %w", err)
	}
	dir := c.runDir(runID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("docker: a macOS VM run directory is already at %s", dir)
		}
		return "", fmt.Errorf("docker: macOS VM run directory: %w", err)
	}
	return dir, nil
}

// waitMacVMRunning waits for the helper to report the VM running.
func waitMacVMRunning(ctx context.Context, cc *macvm.ControlClient) error {
	for {
		st, err := cc.Status(ctx)
		if err != nil {
			return fmt.Errorf("docker: the macOS VM helper: %w", err)
		}
		// Until the helper has the network and starts the VM, it reports stopped with no error and no
		// start time; stopped after that, or with an error, is a VM that ended.
		ended := st.State == macvm.StateError ||
			st.State == macvm.StateStopped && (st.Error != "" || !st.StartedAt.IsZero() || st.Network == macvm.NetLost)
		switch {
		case st.State == macvm.StateRunning:
			return nil
		case ended:
			return fmt.Errorf("docker: the macOS VM did not start: %s", cmp.Or(st.Error, string(st.State)))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("docker: the macOS VM did not start: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// bootMacVM waits for the guest agent and hands it its sandbox; it answers what the guest made of its
// sign-in store.
func (d *Driver) bootMacVM(ctx context.Context, v *vmRun, cc *macvm.ControlClient, spec runner.SandboxSpec, mounts []guestagent.Mount,
	desktopPassword string) (guestagent.InitResult, error) {
	var none guestagent.InitResult
	if err := waitForSocket(ctx, filepath.Join(d.macVMConfig().sockDir(v.runID), macvm.AgentSock), 30*time.Second); err != nil {
		return none, fmt.Errorf("docker: the macOS VM's guest agent: %w", err)
	}
	for {
		attempt, stop := context.WithTimeout(ctx, 20*time.Second)
		_, err := v.agent.Ready(attempt)
		stop()
		if err == nil {
			break
		}
		if st, serr := cc.Status(ctx); serr == nil && st.State != macvm.StateRunning {
			return none, fmt.Errorf("docker: the macOS VM stopped while booting: %s", cmp.Or(st.Error, string(st.State)))
		}
		if ctx.Err() != nil {
			return none, fmt.Errorf("docker: the macOS VM's guest agent did not answer: %w", err)
		}
	}
	in, err := macGuestInit(spec, d.cfg.TrustedCAPEM)
	if err != nil {
		return none, err
	}
	in.Mounts = mounts
	in.DesktopPassword = desktopPassword
	res, err := v.agent.Init(ctx, in)
	in.DesktopPassword = "" // best effort: this copy is dropped once Init returns
	if err != nil {
		return none, fmt.Errorf("docker: hand the macOS VM its sandbox: %w", err)
	}
	return res, nil
}

// macDesktopOffFile, in a run directory, marks a run whose guest did not set its desktop password: its
// desktop stays off for the run, across a runner restart too. It holds nothing.
const macDesktopOffFile = "desktop-off"

// errMacDesktopOff is DialDesktop's refusal for such a run: the runner's own words for a sandbox with no
// desktop.
var errMacDesktopOff = runner.ErrDesktopUnsupported

// macDesktopOff reports a run whose desktop is off.
func macDesktopOff(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, macDesktopOffFile))
	return err == nil
}

// startMacVMNet starts the guest's relay as startVMNet does, but serving its frames on stdin and stdout
// (-stdio) instead of a socket, and answers the attach connection's descriptor. It attaches before the
// start, so nothing is buffered on the connection that the helper would miss.
func (d *Driver) startMacVMNet(ctx context.Context, spec runner.SandboxSpec, proxyIP string) (*os.File, error) {
	labels := d.labels(spec.RunID, componentVMNet, spec.Labels)
	labels[labelMachineKind] = string(types.MachineMacOSVM)
	return d.startMacRelay(ctx, spec.RunID, labels, proxyIP)
}

// startMacRelay is startMacVMNet's relay on id's internal network, -proxy at proxyIP: a run's, or a
// base job's stage-2 one (StartMacPrepNet).
func (d *Driver) startMacRelay(ctx context.Context, id uuid.UUID, labels map[string]string, proxyIP string) (*os.File, error) {
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker: info: %w", err)
	}
	host := hardenedHostConfig(internalNetName(id), "", runner.Resources{MemoryMiB: 256, PidsLimit: vmPidsLimit}, info.Info)
	host.ReadonlyRootfs = true
	host.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyDisabled}
	host.ExtraHosts = []string{"wardyn-proxy:" + proxyIP}
	uid, gid := vmUser(os.Getuid(), os.Getgid())
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: vmnetName(id),
		Config: &container.Config{Image: d.proxyImage(), User: strconv.Itoa(uid) + ":" + strconv.Itoa(gid), Entrypoint: []string{vmnetEntrypoint},
			Cmd:       []string{"-stdio", "-proxy", "wardyn-proxy:" + strconv.Itoa(runner.ProxyListenPort)},
			OpenStdin: true, AttachStdin: true, AttachStdout: true, AttachStderr: true, Labels: labels},
		HostConfig: host,
	})
	if err != nil {
		return nil, fmt.Errorf("docker: create the macOS VM's network relay: %w", err)
	}
	att, err := d.cli.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("docker: attach to the macOS VM's network relay: %w", err)
	}
	f, err := attachFile(att.HijackedResponse)
	att.Close()
	if err != nil {
		return nil, err
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("docker: start the macOS VM's network relay: %w", err)
	}
	return f, nil
}

// attachFile is a duplicate of the attach connection's descriptor. Bytes the client already buffered
// would be lost to the helper, so a connection with any is refused; so is one with no descriptor (an
// engine reached through something other than a socket).
func attachFile(h client.HijackedResponse) (*os.File, error) {
	if h.Reader != nil && h.Reader.Buffered() > 0 {
		return nil, errors.New("docker: the macOS VM's network relay spoke before it started")
	}
	fc, ok := h.Conn.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, errors.New("docker: this Docker engine's attach connection cannot be handed to the macOS VM helper; it needs a unix socket or TCP engine")
	}
	f, err := fc.File()
	if err != nil {
		return nil, fmt.Errorf("docker: the macOS VM's network relay: %w", err)
	}
	return f, nil
}

// macVMStatus is Status for a macOS VM: what its helper says. A helper nobody answers for is gone (the
// Mac restarted, or the helper died), and one that speaks another control protocol is ended cleanly.
func (d *Driver) macVMStatus(ctx context.Context, v *vmRun) (runner.Status, error) {
	if _, err := os.Lstat(filepath.Join(v.dir, macLowDiskFile)); err == nil {
		free, _ := os.ReadFile(filepath.Join(v.dir, macLowDiskFile))
		return runner.Status{State: types.RunStopped, Message: macVMLowDisk(string(free))}, nil
	}
	c := d.macVMConfig()
	if c == nil {
		return runner.Status{State: types.RunStopped}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cc, err := c.dialControl(ctx, v.runID)
	var proto *macvm.ErrProto
	switch {
	case errors.As(err, &proto):
		d.endMacVMHelper(v, syscall.SIGTERM)
		return runner.Status{State: types.RunStopped, Message: macVMUpgraded}, nil
	case errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED):
		return runner.Status{State: types.RunStopped, Message: d.macVMGoneReason(v)}, nil
	case err != nil:
		return runner.Status{}, fmt.Errorf("docker: the macOS VM helper: %w", err)
	}
	defer cc.Close()
	st, err := cc.Status(ctx)
	if err != nil {
		return runner.Status{}, fmt.Errorf("docker: the macOS VM helper: %w", err)
	}
	switch {
	case st.Network == macvm.NetLost:
		return runner.Status{State: types.RunStopped, Message: macVMLostNetwork}, nil
	case st.State == macvm.StateStopped || st.State == macvm.StateError:
		return runner.Status{State: types.RunStopped, Message: cmp.Or(st.Error, macVMStopped)}, nil
	}
	return runner.Status{State: types.RunRunning}, nil
}

// macLowDiskFile, in a run directory, marks a run the runner's low-disk check ended. It holds the free
// GB the check read.
const macLowDiskFile = "ended-low-disk"

// EndNewestMacVM ends this runner's newest running macOS VM, for wardyn-runnerd's free-space check: the
// helper stops the VM hard, the run reports macVMLowDisk, and its teardown deletes it as any other. It
// answers the run's id, "" when no macOS VM of this runner runs. freeBytes is the free space the check
// read, which the run's end reports.
func (d *Driver) EndNewestMacVM(ctx context.Context, freeBytes int64) (string, error) {
	c := d.macVMConfig()
	if c == nil {
		return "", nil
	}
	counted, err := c.Ledger.Counted()
	if err != nil {
		return "", fmt.Errorf("docker: the macOS VM ledger: %w", err)
	}
	var newest *admission.Entry
	var runID uuid.UUID
	for i, e := range counted {
		if e.RunnerID != c.RunnerID || e.Kind != admission.KindRun || c.vmGone(e) || newest != nil && !e.Created.After(newest.Created) {
			continue
		}
		id, err := uuid.Parse(e.RunID)
		if err != nil {
			continue
		}
		// A reserved entry whose create has not written its run directory yet has no VM to end.
		if _, err := readMacRun(c.runDir(id), id); err != nil {
			continue
		}
		newest, runID = &counted[i], id
	}
	if newest == nil {
		return "", nil
	}
	dir := c.runDir(runID)
	if err := os.WriteFile(filepath.Join(dir, macLowDiskFile), []byte(strconv.FormatInt(max(freeBytes, 0)>>30, 10)), 0o600); err != nil {
		return runID.String(), fmt.Errorf("docker: macOS VM run directory: %w", err)
	}
	cc, err := c.dialControl(ctx, runID)
	if err != nil {
		return runID.String(), fmt.Errorf("docker: the macOS VM helper: %w", err)
	}
	defer cc.Close()
	if err := cc.Stop(ctx, false, 0); err != nil {
		return runID.String(), fmt.Errorf("docker: stop the macOS VM: %w", err)
	}
	c.changed()
	return runID.String(), nil
}

// macVMGoneReason says why a helper is gone: a ledger entry from an earlier boot (by its boot session, as
// the ledger tells boots apart) means the Mac restarted; any other gone helper simply stopped.
func (d *Driver) macVMGoneReason(v *vmRun) string {
	c := d.macVMConfig()
	cfg, err := readMacRun(v.dir, v.runID)
	if c == nil || err != nil {
		return macVMHelperStopped
	}
	if e, ok := c.entry(cfg.LedgerEntry); ok && admission.EarlierBoot(c.Host, e) {
		return macVMRestarted
	}
	return macVMHelperStopped
}

// entry is one of this runner's ledger entries, counted or held as a disk debt.
func (c *MacVMConfig) entry(id string) (admission.Entry, bool) {
	if id == "" {
		return admission.Entry{}, false
	}
	counted, err := c.Ledger.Counted()
	if err != nil {
		return admission.Entry{}, false
	}
	debts, _ := c.Ledger.Debts()
	for _, e := range append(counted, debts...) {
		if e.ID == id && e.RunnerID == c.RunnerID {
			return e, true
		}
	}
	return admission.Entry{}, false
}

// helperAlive reports whether e's helper is still the process the ledger recorded.
func (c *MacVMConfig) helperAlive(e admission.Entry) bool { return c.alive(e.HelperPID, e.HelperStart) }

// vmGone reports whether nothing can still start or run e's VM: its helper, or for an entry no helper has
// claimed yet, the runnerd that took it, is no longer the process it was.
func (c *MacVMConfig) vmGone(e admission.Entry) bool {
	if e.HelperPID != 0 {
		return !c.helperAlive(e)
	}
	return !c.alive(e.OwnerPID, e.OwnerStart)
}

func (c *MacVMConfig) alive(pid int, start int64) bool {
	if pid == 0 {
		return false
	}
	got, err := admission.Self(c.Host, pid)
	return err == nil && got == start
}

// endMacVMHelper signals the run's helper, found by the pid and start time its ledger entry holds.
func (d *Driver) endMacVMHelper(v *vmRun, sig syscall.Signal) {
	c := d.macVMConfig()
	cfg, err := readMacRun(v.dir, v.runID)
	if c == nil || err != nil {
		return
	}
	if e, ok := c.entry(cfg.LedgerEntry); ok && c.helperAlive(e) {
		if err := c.signal(e.HelperPID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			slog.Warn("docker: signal a macOS VM helper", slog.String("run_id", v.runID.String()), slog.String("err", err.Error()))
		}
	}
}

// macVMStop is the macOS VM's part of a stop or a lease end: the VM is stopped (gracefully first when
// asked) and destroyed, since a macOS VM keeps no disk past its run.
func (d *Driver) macVMStop(ctx context.Context, v *vmRun, graceful bool) {
	cfg, err := readMacRun(v.dir, v.runID)
	if err != nil {
		return // already destroyed
	}
	d.destroyMacVM(ctx, v, cfg.LedgerEntry, graceful)
}

// destroyMacVM ends a macOS VM in the design's teardown order: the helper stops the VM (asking the guest
// first when graceful, then hard), the helper exits (or is killed), the run directory is deleted, and only
// then is the ledger entry released, by the helper's stop report, or with no helper ever having claimed
// it (the helper claims before it builds a VM, and a claim after the release finds no entry), or else by
// the ledger's own check that no VM process of this user can still be it. A step that fails leaves the
// entry held, and the next orphan sweep retries it. It runs on a context cut loose from the call's, which
// the organisation cancels when it gives up on a slow call, and bounded by macDestroyTimeout instead.
func (d *Driver) destroyMacVM(ctx context.Context, v *vmRun, entryID string, graceful bool) {
	c := d.macVMConfig()
	if c == nil {
		return
	}
	ctx, done := context.WithTimeout(context.WithoutCancel(ctx), macDestroyTimeout)
	defer done()
	log := slog.With(slog.String("run_id", v.runID.String()))
	if _, err := c.Ledger.Update(entryID, func(e *admission.Entry) { e.State = admission.StateStopping }); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("docker: mark a macOS VM stopping in the ledger", slog.String("err", err.Error()))
	}
	stopped := false
	sctx, cancel := context.WithTimeout(ctx, macvm.DefaultStopTimeout+macHelperExitWait)
	if cc, err := c.dialControl(sctx, v.runID); err == nil {
		stopped = cc.Stop(sctx, graceful, macvm.DefaultStopTimeout) == nil
		_ = cc.Close()
	}
	cancel()
	if e, ok := c.entry(entryID); ok && !d.waitMacVMHelperExit(ctx, c, e) {
		log.Warn("docker: the macOS VM helper did not exit; killing it")
		d.endMacVMHelper(v, syscall.SIGKILL)
		if !d.waitMacVMHelperExit(ctx, c, e) {
			d.recordMacOSVMDisk(v.runID, runner.MacOSVMDiskKept, runner.MacOSVMDiskHelperAlive)
			if ctx.Err() != nil {
				log.Error("docker: the macOS VM destroy ran out of time before the helper exited; its ledger entry stays held")
			} else {
				log.Error("docker: the macOS VM helper outlived SIGKILL; its ledger entry stays held")
			}
			return
		}
	}
	if err := c.removeAll(v.dir); err != nil {
		log.Warn("docker: delete a macOS VM run directory (pending; the orphan sweep retries)", slog.String("err", err.Error()))
		d.recordMacOSVMDisk(v.runID, runner.MacOSVMDiskKept, runner.MacOSVMDiskRemoveFailed)
		return
	}
	d.recordMacOSVMDisk(v.runID, runner.MacOSVMDiskRemoved, "")
	c.sweepSockResidue()
	var err error
	if e, ok := c.entry(entryID); stopped || ok && e.HelperPID == 0 {
		err = c.Ledger.Release(entryID, admission.Destroyed{Stopped: true, RunDir: v.dir})
	} else {
		err = c.Ledger.ReclaimOwn(entryID, v.dir)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("docker: release a macOS VM's ledger entry (the orphan sweep retries)", slog.String("err", err.Error()))
	}
	c.changed()
}

// waitMacVMHelperExit waits for e's helper to leave the process table.
func (d *Driver) waitMacVMHelperExit(ctx context.Context, c *MacVMConfig, e admission.Entry) bool {
	deadline := time.Now().Add(cmp.Or(c.helperWait, macHelperExitWait))
	for c.helperAlive(e) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// teardownMacVM is teardownVM's macOS VM part: destroy the run's VM, if it is one, whatever is left of it.
// It reports whether the run was a macOS VM.
func (d *Driver) teardownMacVM(ctx context.Context, runID uuid.UUID) bool {
	c := d.macVMConfig()
	if c == nil {
		return false
	}
	dir := c.runDir(runID)
	cfg, err := readMacRun(dir, runID)
	if err != nil {
		return false
	}
	v := newMacVMRun(c, runID, dir)
	defer func() { v.stop(); v.agent.Close() }()
	d.destroyMacVM(ctx, v, cfg.LedgerEntry, false)
	return true
}

// sweepMacVMOrphans is the macOS VM part of the orphan sweep, and the owner's return after another login
// tombstoned this runner's entries: a run directory with no live helper, older than cutoff, whose run
// isOrphan says owns no sandbox, is deleted and its entry released by the ledger's own check. An in-flight
// create has a live helper or a young directory, so neither the age gate nor a missing helper alone
// sweeps it. Then every entry of this runner whose VM is gone and whose run directory is gone too is
// reconciled, which also clears a tombstoned entry's disk debt.
func (d *Driver) sweepMacVMOrphans(ctx context.Context, cutoff time.Time, isOrphan func(uuid.UUID) bool) (int, error) {
	c := d.macVMConfig()
	if c == nil {
		return 0, nil
	}
	var errs []error
	cleaned := 0
	des, err := os.ReadDir(c.Layout.RunsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("docker: read macOS VM run directories: %w", err))
	}
	for _, de := range des {
		dir := filepath.Join(c.Layout.RunsDir(), de.Name())
		cfg, cerr := macvm.ReadRunConfig(dir)
		runID, perr := uuid.Parse(cfg.RunID)
		fi, ierr := de.Info()
		if cerr != nil || perr != nil || ierr != nil || dir != c.runDir(runID) || fi.ModTime().After(cutoff) || !isOrphan(runID) {
			continue
		}
		if e, ok := c.entry(cfg.LedgerEntry); ok && !c.vmGone(e) {
			continue // a live VM, or a create in flight; its run's own teardown ends it
		}
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		cc, derr := c.dialControl(dctx, runID)
		cancel()
		if derr == nil {
			_ = cc.Close()
			continue
		}
		if err := c.removeAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("docker: remove orphaned macOS VM run directory %s: %w", runID, err))
			continue
		}
		cleaned++
		d.recordMacOSVMDisk(runID, runner.MacOSVMDiskRemoved, "") // a kept disk the sweep now removed
		c.sweepSockResidue()
		if err := c.Ledger.ReclaimOwn(cfg.LedgerEntry, dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("docker: release orphaned macOS VM %s: %w", runID, err))
		}
	}
	errs = append(errs, c.reclaimGone())
	c.changed()
	return cleaned, errors.Join(errs...)
}

// reclaimGone reconciles this runner's run entries whose helper is gone and whose run directory is gone:
// a teardown that could not release, or a login's return after another login tombstoned its entries.
// The ledger itself refuses while any VM process of this user could still be the entry's VM.
func (c *MacVMConfig) reclaimGone() error {
	counted, err := c.Ledger.Counted()
	if err != nil {
		return fmt.Errorf("docker: the macOS VM ledger: %w", err)
	}
	debts, err := c.Ledger.Debts()
	if err != nil {
		return fmt.Errorf("docker: the macOS VM ledger: %w", err)
	}
	var errs []error
	for _, e := range append(counted, debts...) {
		if e.RunnerID != c.RunnerID || e.Kind != admission.KindRun || !c.vmGone(e) {
			continue
		}
		runID, err := uuid.Parse(e.RunID)
		if err != nil {
			continue
		}
		dir := c.runDir(runID)
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			continue // its run's teardown, or the orphan sweep above, deletes it first
		}
		if err := c.Ledger.ReclaimOwn(e.ID, dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("docker: reconcile macOS VM ledger entry for run %s: %w", runID, err))
		}
	}
	return errors.Join(errs...)
}
