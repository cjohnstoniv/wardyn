// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// A Linux VM sandbox (machine_kind linux_vm) is Docker containers on the runner's daemon: QEMU/KVM
// in an unprivileged container that IS the run's agent container (same name, same ref), one
// virtiofsd container per mount, and one-shot prep containers, next to the run's usual network and
// proxy. So the driver's names, labels, teardown, proxy stop and the runner's offline containment
// cover a VM with no second copy. Only wardyn-runnerd turns it on (EnableVM, after ProbeVM).

// labelMachineKind marks the QEMU container: every per-ref method looks for it.
const labelMachineKind = "wardyn.machine-kind"

// envInteractiveStart is the sandbox env the control plane sets for interactive_start: agent.
const envInteractiveStart = "WARDYN_INTERACTIVE_START"

const (
	componentVMFS   = "vmfs"   // a virtiofsd sidecar
	componentVMPrep = "vmprep" // a one-shot prep container (image conversion, the overlay)
)

// vmPidsLimit bounds a VM's containers: QEMU's threads are a few dozen.
const vmPidsLimit = 1024

// VMConfig turns Linux VM sandboxes on. ProbeVM fills MachineType and QEMUVersion.
type VMConfig struct {
	// StateDir holds the image cache (images/) and the run directories (runs/). The Docker daemon
	// must see it at the same path: a local engine, which ProbeVM proves.
	StateDir string
	// QEMUImage is the wardyn/vm-qemu image every VM container starts from.
	QEMUImage string
	// CPUModel is the named QEMU CPU model every VM gets, started with enforce.
	CPUModel string
	// MachineType (pc-q35-X.Y) and QEMUVersion are what the probe found; every VM uses and records them.
	MachineType string
	QEMUVersion string
	// UID and GID are who every VM container runs as and who owns the run directories; never 0.
	UID, GID int
	// KVMGID is the group of /dev/kvm, added to the QEMU container; empty when /dev/kvm is open to all.
	KVMGID string
	// BootTimeout bounds a VM's boot to its guest agent's first answer; zero is three minutes.
	BootTimeout time.Duration
}

// maxSocketPath is the longest unix socket path a connect accepts (sun_path is 108 bytes).
const maxSocketPath = 107

func (c VMConfig) validate() error {
	switch {
	case c.StateDir == "" || !filepath.IsAbs(c.StateDir):
		return fmt.Errorf("docker: the VM state directory %q is not an absolute path", c.StateDir)
	case len(c.runDir(uuid.Nil))+len("/ctl/"+qemu.AgentSocket) > maxSocketPath:
		return fmt.Errorf("docker: the VM state directory %q is too long for its sockets' paths", c.StateDir)
	case c.QEMUImage == "":
		return errors.New("docker: no QEMU image for VMs")
	case c.UID <= 0 || c.GID <= 0:
		return errors.New("docker: VM containers never run as root")
	case c.KVMGID == "0":
		return errors.New("docker: a VM container never gets the root group")
	}
	return qemu.Config{RunID: uuid.New(), MachineType: c.MachineType, CPUModel: c.CPUModel, VCPUs: 1, MemoryMiB: 256}.Validate()
}

func (c VMConfig) runDir(runID uuid.UUID) string {
	return filepath.Join(c.StateDir, "runs", runID.String())
}

func (c VMConfig) imageDir(id string) string { return filepath.Join(c.StateDir, "images", id) }

// runParts are the run directory's parts (DESIGN 3.3): each mounted only where it is needed, and
// all of them 0700 and owned by the VM containers' user, so nobody else on the host can open a
// socket in them. ctl/ holds VNC (no authentication) and QMP (full control of the VM).
var runParts = []string{"ctl", "net", "fs", "disk", "fwcfg"}

// EnableVM turns Linux VM sandboxes on; a nil config turns them off.
func (d *Driver) EnableVM(c *VMConfig) error {
	if c != nil {
		if err := c.validate(); err != nil {
			return err
		}
		if c.BootTimeout == 0 {
			c.BootTimeout = 3 * time.Minute
		}
	}
	d.vmCfg.Store(c)
	return nil
}

func (d *Driver) vmConfig() *VMConfig { return d.vmCfg.Load() }

// vmRun is a VM the driver knows: found at create, or again by its container's label.
type vmRun struct {
	runID uuid.UUID
	dir   string
	agent *guestagent.Client
	// stop ends the output followers of the VM's detached execs at teardown.
	ctx  context.Context
	stop context.CancelFunc
}

func (d *Driver) dialUnix(ctx context.Context, path string) (net.Conn, error) {
	if d.vmDial != nil {
		return d.vmDial(ctx, path)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "unix", path)
}

func (d *Driver) newVMRun(runID uuid.UUID, dir string) *vmRun {
	ctx, stop := context.WithCancel(context.Background())
	sock := filepath.Join(dir, "ctl", qemu.AgentSocket)
	return &vmRun{runID: runID, dir: dir, ctx: ctx, stop: stop,
		agent: guestagent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.dialUnix(ctx, sock) })}
}

// vmFor is the VM behind ref, or nil for a container (or when VMs are off, which costs nothing).
// After a runner restart the VM is found again by its container's label. A ref found to be a
// container is remembered as one until its teardown, so it is inspected once, not on every call.
func (d *Driver) vmFor(ctx context.Context, ref string) *vmRun {
	vc := d.vmConfig()
	if vc == nil {
		return nil
	}
	if v, ok := d.vms.Load(ref); ok {
		return v.(*vmRun)
	}
	if _, ok := d.notVMs.Load(ref); ok {
		return nil
	}
	res, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return nil
	}
	if res.Container.Config == nil || res.Container.Config.Labels[labelMachineKind] != string(types.MachineLinuxVM) {
		d.notVMs.Store(ref, struct{}{})
		return nil
	}
	id, err := parseRunID(res.Container.Config.Labels[labelRun])
	if err != nil {
		return nil
	}
	v, _ := d.vms.LoadOrStore(ref, d.newVMRun(id, vc.runDir(id)))
	return v.(*vmRun)
}

// errVMUnsupported answers the verbs a VM does not serve in this release.
var errVMUnsupported = errors.New("docker: a Linux VM sandbox does not support this")

// refuseVMSpec refuses, before anything is created, what a VM cannot honour (DESIGN 3.6).
func (d *Driver) refuseVMSpec(spec runner.SandboxSpec) error {
	if d.vmConfig() == nil {
		return errors.New("docker: this runner does not offer Linux VM sandboxes")
	}
	if spec.MachineKind != types.MachineLinuxVM {
		return fmt.Errorf("docker: unknown machine kind %q", spec.MachineKind)
	}
	if len(spec.Drives) > 0 {
		return errors.New("docker: a Linux VM sandbox has no drives in this release")
	}
	if spec.RecordWithin(d.cfg.Record) {
		return errors.New("docker: a Linux VM sandbox is not recorded in this release; turn recording off for this run")
	}
	if spec.Interactive && spec.Env[envInteractiveStart] == "agent" {
		return errors.New("docker: a Linux VM sandbox's terminal opens a shell, not the agent CLI, in this release; set interactive_start to shell")
	}
	return d.preflightSpec(spec)
}

// createVM is CreateSandbox for a Linux VM. Like the container path, everything created before an
// error is rolled back; the network and proxy come up exactly as for a container.
//
//nolint:funlen // One linear assembly sequence whose rollback must stay in one scope, as CreateSandbox's.
func (d *Driver) createVM(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	if err := d.refuseVMSpec(spec); err != nil {
		return runner.Sandbox{}, err
	}
	vc := d.vmConfig()
	guestMiB, containerMiB, err := qemu.Memory(spec.Resources.MemoryMiB)
	if err != nil {
		return runner.Sandbox{}, err
	}
	infoRes, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return runner.Sandbox{}, fmt.Errorf("docker: info: %w", err)
	}
	for _, img := range []string{vc.QEMUImage, spec.Image, d.proxyImage()} {
		if err := d.ensureImage(ctx, img, "", func() { spec.NotifyWaiting("image: Pulling: " + img) }); err != nil {
			return runner.Sandbox{}, err
		}
	}
	mounts, err := d.agentMounts(ctx, spec, false)
	if err != nil {
		return runner.Sandbox{}, err
	}
	shares := mounts[:0]
	for _, m := range mounts {
		if m.Target != RecordingMountTarget { // recording is refused above; its mount means nothing here
			shares = append(shares, m)
		}
	}

	var rollback []func()
	fail := func(err error) (runner.Sandbox, error) {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
		return runner.Sandbox{}, err
	}
	_, _, undoNet, err := d.runNetworkAndProxy(ctx, spec)
	if err != nil {
		return runner.Sandbox{}, err
	}
	rollback = append(rollback, undoNet)

	base, err := d.vmImage(ctx, spec)
	if err != nil {
		return fail(err)
	}
	dir := vc.runDir(spec.RunID)
	rollback = append(rollback, func() { _ = os.RemoveAll(dir) })
	if err := d.makeRunDir(dir, vmCA(spec, d.cfg.TrustedCAPEM)); err != nil {
		return fail(err)
	}
	part := func(p string) string { return filepath.Join(dir, p) }

	// The run's overlay on the read-only base.
	if err := d.runOneShot(ctx, spec.RunID, "overlay", qemu.OverlayArgs(), []mount.Mount{
		{Type: mount.TypeBind, Source: base, Target: qemu.BaseDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: part("disk"), Target: qemu.DiskDir},
	}); err != nil {
		return fail(err)
	}

	// One virtiofsd per mount, started before QEMU, which connects to its socket at start.
	rollback = append(rollback, func() { d.warnVMSidecars(context.Background(), spec.RunID) })
	guestMounts := make([]guestagent.Mount, 0, len(shares))
	for n, m := range shares {
		if err := d.startVirtiofsd(ctx, spec, n, m, part("fs")); err != nil {
			return fail(err)
		}
		guestMounts = append(guestMounts, guestagent.Mount{Tag: qemu.ShareTag(n), Target: m.Target, ReadOnly: m.ReadOnly})
	}
	for n := range shares {
		if err := waitForSocket(ctx, filepath.Join(part("fs"), qemu.ShareSocket(n)), 30*time.Second); err != nil {
			return fail(fmt.Errorf("docker: virtiofsd for mount %d: %w", n, err))
		}
	}

	qc := qemu.Config{RunID: spec.RunID, MachineType: vc.MachineType, CPUModel: vc.CPUModel,
		VCPUs: qemu.VCPUs(spec.Resources.CPUMillis), MemoryMiB: guestMiB, Shares: len(shares)}
	_, err = os.Stat(filepath.Join(part("fwcfg"), qemu.CAFile))
	qc.CA = err == nil
	argv, err := qemu.Args(qc)
	if err != nil {
		return fail(err)
	}
	res := runner.Resources{CPUMillis: spec.Resources.CPUMillis, MemoryMiB: containerMiB, PidsLimit: vmPidsLimit}
	host := d.vmHostConfig(res, infoRes.Info)
	host.Devices = append(host.Devices, container.DeviceMapping{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"})
	if vc.KVMGID != "" {
		host.GroupAdd = append(host.GroupAdd, vc.KVMGID)
	}
	host.Mounts = []mount.Mount{
		{Type: mount.TypeBind, Source: base, Target: qemu.BaseDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: part("ctl"), Target: qemu.CtlDir},
		{Type: mount.TypeBind, Source: part("net"), Target: qemu.NetDir},
		{Type: mount.TypeBind, Source: part("fs"), Target: qemu.FSDir},
		{Type: mount.TypeBind, Source: part("disk"), Target: qemu.DiskDir},
		{Type: mount.TypeBind, Source: part("fwcfg"), Target: qemu.FWCfgDir, ReadOnly: true},
	}
	labels := d.labels(spec.RunID, componentAgent, spec.Labels)
	labels[labelMachineKind] = string(types.MachineLinuxVM)
	name := agentContainerName(spec.RunID)
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       name,
		Config:     &container.Config{Image: vc.QEMUImage, User: vc.user(), Cmd: argv, Labels: labels, Hostname: "agent"},
		HostConfig: host,
	})
	if err != nil {
		return fail(fmt.Errorf("docker: create the VM: %w", err))
	}
	rollback = append(rollback, func() {
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	// As for a container: a discarded resource limit refuses the run before anything runs uncapped.
	if capErr := verifyCapsEnforced(created.Warnings); capErr != nil && !d.cfg.AllowUnenforceableCaps {
		return fail(capErr)
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fail(fmt.Errorf("docker: start the VM: %w", err))
	}
	v := d.newVMRun(spec.RunID, dir)
	rollback = append(rollback, func() { v.stop(); v.agent.Close() })
	if err := d.bootVM(ctx, v, name, spec, guestMounts); err != nil {
		return fail(err)
	}
	machine := &types.VMMachine{MachineType: vc.MachineType, CPUModel: vc.CPUModel, Accelerator: "kvm", QEMUVersion: vc.QEMUVersion}
	d.vms.Store(name, v)
	d.keepExecOutput(name, spec.ExecOutput)
	// A hardware VM boundary: the CC3 family, as Kata and krun microVMs are.
	return runner.Sandbox{Ref: name, Driver: driverName, EnforcedClass: types.CC3, Machine: machine}, nil
}

func (c VMConfig) user() string { return strconv.Itoa(c.UID) + ":" + strconv.Itoa(c.GID) }

// vmHostConfig is the hardening every VM container gets: the container one, with no network at
// all and a read-only root. Never privileged; no capability.
func (d *Driver) vmHostConfig(res runner.Resources, info system.Info) *container.HostConfig {
	host := hardenedHostConfig("none", "", res, info)
	host.ReadonlyRootfs = true
	return host
}

// vmCA is what the guest trusts, handed over fw_cfg: the run's proxy CA certificate (never its
// key) and the runner's own trusted CA bundle. Never a secret.
func vmCA(spec runner.SandboxSpec, trusted string) string {
	var parts []string
	for _, p := range []string{spec.ProxyConfig.MITMCACertPEM, trusted} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p+"\n")
		}
	}
	return strings.Join(parts, "")
}

// makeRunDir creates the run directory and its parts, 0700 and owned by the containers' user.
func (d *Driver) makeRunDir(dir, ca string) error {
	vc := d.vmConfig()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("docker: VM run directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("docker: VM run directory: %w", err)
	}
	paths := []string{dir}
	for _, p := range runParts {
		paths = append(paths, filepath.Join(dir, p))
		if err := os.Mkdir(paths[len(paths)-1], 0o700); err != nil {
			return fmt.Errorf("docker: VM run directory: %w", err)
		}
	}
	if ca != "" {
		f := filepath.Join(dir, "fwcfg", qemu.CAFile)
		if err := os.WriteFile(f, []byte(ca), 0o400); err != nil {
			return fmt.Errorf("docker: VM CA: %w", err)
		}
		paths = append(paths, f)
	}
	return vc.chown(paths...)
}

// chown hands paths to the containers' user when the runner runs as root; otherwise they are
// already the runner's own, which the containers run as.
func (c VMConfig) chown(paths ...string) error {
	if os.Getuid() != 0 {
		return nil
	}
	for _, p := range paths {
		if err := os.Lchown(p, c.UID, c.GID); err != nil {
			return err
		}
	}
	return nil
}

// vmImage is the image's converted base (rootfs.raw, vmlinuz, initrd.img), made on first use and
// cached by image id: export the image's filesystem to a tar, then mke2fs -d in a prep container,
// unprivileged, so file owners and setuid bits come from the tar and nothing runs as root.
func (d *Driver) vmImage(ctx context.Context, spec runner.SandboxSpec) (string, error) {
	vc := d.vmConfig()
	insp, err := d.cli.ImageInspect(ctx, spec.Image)
	if err != nil {
		return "", fmt.Errorf("docker: inspect the VM image: %w", err)
	}
	id := strings.TrimPrefix(insp.ID, "sha256:")
	if id == "" || strings.ContainsAny(id, "/.") {
		return "", fmt.Errorf("docker: the VM image has no usable id %q", insp.ID)
	}
	dir := vc.imageDir(id)
	if _, err := os.Stat(filepath.Join(dir, qemu.RootFS)); err == nil {
		return dir, nil
	}
	spec.NotifyWaiting("image: converting for a VM: " + spec.Image)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", fmt.Errorf("docker: VM image cache: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), id+".tmp-")
	if err != nil {
		return "", fmt.Errorf("docker: VM image cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := d.exportImage(ctx, spec, filepath.Join(tmp, "rootfs.tar")); err != nil {
		return "", err
	}
	if err := vc.chown(tmp, filepath.Join(tmp, "rootfs.tar")); err != nil {
		return "", err
	}
	if err := d.runOneShot(ctx, spec.RunID, "image", []string{"wardyn-vm-image"}, []mount.Mount{
		{Type: mount.TypeBind, Source: tmp, Target: "/out"},
	}); err != nil {
		return "", err
	}
	for _, f := range []string{qemu.RootFS, qemu.Kernel, qemu.Initrd} {
		if _, err := os.Stat(filepath.Join(tmp, f)); err != nil {
			return "", fmt.Errorf("docker: the VM image conversion made no %s", f)
		}
	}
	// Atomic: a concurrent conversion of the same image that finished first wins, and ours is dropped.
	if err := os.Rename(tmp, dir); err != nil {
		if _, serr := os.Stat(filepath.Join(dir, qemu.RootFS)); serr != nil {
			return "", fmt.Errorf("docker: VM image cache: %w", err)
		}
	}
	return dir, nil
}

// exportImage writes the image's filesystem to path: a container created from it (never started)
// and exported.
func (d *Driver) exportImage(ctx context.Context, spec runner.SandboxSpec, path string) error {
	name := vmPrepName(spec.RunID, "export")
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{Image: spec.Image, Entrypoint: []string{"/nonexistent"},
			Labels: d.labels(spec.RunID, componentVMPrep, nil)},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		return fmt.Errorf("docker: create the VM image export: %w", err)
	}
	defer func() {
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	rc, err := d.cli.ContainerExport(ctx, created.ID, client.ContainerExportOptions{})
	if err != nil {
		return fmt.Errorf("docker: export the VM image: %w", err)
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("docker: export the VM image: %w", err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		return fmt.Errorf("docker: export the VM image: %w", err)
	}
	return f.Close()
}

func vmPrepName(runID uuid.UUID, what string) string {
	return "wardyn-vmprep-" + runID.String() + "-" + what
}

func vmfsName(runID uuid.UUID, n int) string {
	return "wardyn-vmfs-" + runID.String() + "-" + strconv.Itoa(n)
}

// runOneShot runs argv in a prep container from the QEMU image, hardened as a VM, and waits for it.
func (d *Driver) runOneShot(ctx context.Context, runID uuid.UUID, what string, argv []string, mounts []mount.Mount) error {
	vc := d.vmConfig()
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return fmt.Errorf("docker: info: %w", err)
	}
	host := d.vmHostConfig(runner.Resources{MemoryMiB: 1024, PidsLimit: vmPidsLimit}, info.Info)
	host.Mounts = mounts
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       vmPrepName(runID, what),
		Config:     &container.Config{Image: vc.QEMUImage, User: vc.user(), Cmd: argv, Labels: d.labels(runID, componentVMPrep, nil)},
		HostConfig: host,
	})
	if err != nil {
		return fmt.Errorf("docker: create the VM %s step: %w", what, err)
	}
	defer func() {
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	wait := d.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker: start the VM %s step: %w", what, err)
	}
	select {
	case r := <-wait.Result:
		if r.StatusCode != 0 {
			return fmt.Errorf("docker: the VM %s step exited %d: %s", what, r.StatusCode, d.proxyExitLogTail(ctx, created.ID))
		}
		return nil
	case err := <-wait.Error:
		return fmt.Errorf("docker: the VM %s step: %w", what, err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startVirtiofsd serves mount m as share n: its source at /share, the run's fs/ part for its socket.
func (d *Driver) startVirtiofsd(ctx context.Context, spec runner.SandboxSpec, n int, m mount.Mount, fsDir string) error {
	vc := d.vmConfig()
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return fmt.Errorf("docker: info: %w", err)
	}
	host := d.vmHostConfig(runner.Resources{MemoryMiB: 512, PidsLimit: vmPidsLimit}, info.Info)
	share := m
	share.Target = "/share"
	host.Mounts = []mount.Mount{share, {Type: mount.TypeBind, Source: fsDir, Target: qemu.FSDir}}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: vmfsName(spec.RunID, n),
		Config: &container.Config{Image: vc.QEMUImage, User: vc.user(), Cmd: qemu.VirtiofsdArgs(n, m.ReadOnly),
			Labels: d.labels(spec.RunID, componentVMFS, spec.Labels)},
		HostConfig: host,
	})
	if err != nil {
		return fmt.Errorf("docker: create virtiofsd for %s: %w", m.Target, err)
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker: start virtiofsd for %s: %w", m.Target, err)
	}
	return nil
}

// maxVMShares bounds the virtiofsd names removeVMSidecars tries when removals keep failing.
const maxVMShares = 64

// removeVMSidecars removes a run's virtiofsd and prep containers. Their names are deterministic
// and their count small, so no list is needed: the shares end at the first absent one. A failed
// removal does not stop the others; every failure is returned.
func (d *Driver) removeVMSidecars(ctx context.Context, runID uuid.UUID) error {
	var errs []error
	remove := func(name string) bool {
		_, err := d.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
		if err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("docker: remove %s: %w", name, err))
		}
		return err == nil || !isNotFound(err)
	}
	for n := 0; n < maxVMShares && remove(vmfsName(runID, n)); n++ {
	}
	for _, what := range []string{"export", "image", "overlay"} {
		remove(vmPrepName(runID, what))
	}
	return errors.Join(errs...)
}

// warnVMSidecars is removeVMSidecars where nothing can act on a failure but the log and a later sweep.
func (d *Driver) warnVMSidecars(ctx context.Context, runID uuid.UUID) {
	if err := d.removeVMSidecars(ctx, runID); err != nil {
		slog.Warn("docker: remove a VM's sidecars", slog.String("run_id", runID.String()), slog.String("err", err.Error()))
	}
}

// waitForSocket waits for a unix socket a container creates.
func waitForSocket(ctx context.Context, path string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no socket at %s after %s", path, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// bootVM waits for the guest agent, sets the guest clock through qemu-ga, checks that the running
// VM is under KVM, then hands the guest agent the sandbox's env, managed files and mounts.
func (d *Driver) bootVM(ctx context.Context, v *vmRun, ref string, spec runner.SandboxSpec, mounts []guestagent.Mount) error {
	vc := d.vmConfig()
	ctx, cancel := context.WithTimeout(ctx, vc.BootTimeout)
	defer cancel()
	ctl := filepath.Join(v.dir, "ctl")
	if err := waitForSocket(ctx, filepath.Join(ctl, qemu.AgentSocket), 30*time.Second); err != nil {
		return fmt.Errorf("docker: the VM did not start: %w%s", err, d.vmLogTail(ref))
	}
	spec.NotifyWaiting("vm: booting")
	for {
		attempt, stop := context.WithTimeout(ctx, 20*time.Second)
		_, err := v.agent.Ready(attempt)
		stop()
		if err == nil {
			break
		}
		if st, serr := d.Status(ctx, ref); serr == nil && st.State != types.RunRunning {
			return fmt.Errorf("docker: the VM stopped while booting%s", d.vmLogTail(ref))
		}
		if ctx.Err() != nil {
			return fmt.Errorf("docker: the VM's guest agent did not answer within %s: %w%s", vc.BootTimeout, err, d.vmLogTail(ref))
		}
	}
	qgaConn, err := d.dialUnix(ctx, filepath.Join(ctl, qemu.QGASocket))
	if err != nil {
		return fmt.Errorf("docker: the VM's qemu-ga: %w", err)
	}
	qga, err := qemu.NewQGA(ctx, qgaConn)
	if err != nil {
		_ = qgaConn.Close()
		return fmt.Errorf("docker: the VM's qemu-ga: %w", err)
	}
	err = qga.SetTime(ctx, time.Now())
	_ = qga.Close()
	if err != nil {
		return fmt.Errorf("docker: set the VM's clock: %w", err)
	}
	if err := d.vmUnderKVM(ctx, ctl); err != nil {
		return err
	}
	in := guestagent.Init{Env: spec.Env, SecretEnv: spec.SecretEnv, Mounts: mounts}
	for _, f := range spec.ManagedFiles {
		in.Files = append(in.Files, guestagent.File{Path: f.Path, Mode: f.Mode, AgentOwned: f.AgentOwned, Content: f.Content})
	}
	if err := v.agent.Init(ctx, in); err != nil {
		return fmt.Errorf("docker: hand the VM its sandbox: %w", err)
	}
	return nil
}

// vmUnderKVM asks the running VM's QMP whether KVM backs it: a VM never runs under emulation.
func (d *Driver) vmUnderKVM(ctx context.Context, ctl string) error {
	conn, err := d.dialUnix(ctx, filepath.Join(ctl, qemu.QMPSocket))
	if err != nil {
		return fmt.Errorf("docker: the VM's QMP: %w", err)
	}
	q, err := qemu.NewQMP(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("docker: the VM's QMP: %w", err)
	}
	defer q.Close()
	var kvm struct {
		Enabled bool `json:"enabled"`
		Present bool `json:"present"`
	}
	if err := q.Execute(ctx, "query-kvm", nil, &kvm); err != nil {
		return err
	}
	if !kvm.Enabled || !kvm.Present {
		return errors.New("docker: the VM is not running under KVM")
	}
	return nil
}

// vmPowerdown asks the guest to shut down (ACPI) and waits up to stopTimeout for QEMU to exit.
func (d *Driver) vmPowerdown(ctx context.Context, v *vmRun, ref string) {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	conn, err := d.dialUnix(ctx, filepath.Join(v.dir, "ctl", qemu.QMPSocket))
	if err != nil {
		return
	}
	q, err := qemu.NewQMP(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	err = q.Execute(ctx, "system_powerdown", nil, nil)
	_ = q.Close()
	if err != nil {
		return
	}
	wait := d.cli.ContainerWait(ctx, ref, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-wait.Result:
	case <-wait.Error:
	case <-ctx.Done():
	}
}

// vmLogTail is the end of the QEMU container's log (the guest console and QEMU's own errors).
func (d *Driver) vmLogTail(ref string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if t := d.proxyExitLogTail(ctx, ref); t != "" {
		return ": " + t
	}
	return ""
}

// teardownVM removes what a VM run adds to a container run's teardown: its sidecars and its run
// directory. Safe on any run id; a container run has neither.
func (d *Driver) teardownVM(ctx context.Context, agentRef string, runID uuid.UUID) {
	vc := d.vmConfig()
	if vc == nil {
		return
	}
	if v, ok := d.vms.LoadAndDelete(agentRef); ok {
		v.(*vmRun).stop()
		v.(*vmRun).agent.Close()
	}
	d.warnVMSidecars(ctx, runID)
	if err := os.RemoveAll(vc.runDir(runID)); err != nil {
		slog.Warn("docker: remove a VM run directory", slog.String("run_id", runID.String()), slog.String("err", err.Error()))
	}
}

// sweepVMOrphans removes the VM residue a teardown cannot reach: virtiofsd and prep containers,
// and run directories, older than cutoff whose run has no agent (QEMU) container. They are made
// before the QEMU container, so the age gate keeps a create in flight safe. It answers how many
// runs it cleaned. The containers are swept even with VMs off; the directories need the config.
func (d *Driver) sweepVMOrphans(ctx context.Context, lister containerListerAPI, cutoff time.Time) (int, error) {
	gone := map[uuid.UUID]bool{}
	agentGone := func(runID uuid.UUID) bool {
		if g, ok := gone[runID]; ok {
			return g
		}
		_, err := d.cli.ContainerInspect(ctx, agentContainerName(runID), client.ContainerInspectOptions{})
		gone[runID] = isNotFound(err)
		return gone[runID]
	}
	cleaned := map[uuid.UUID]bool{}
	var errs []error
	for _, comp := range []string{componentVMFS, componentVMPrep} {
		res, err := lister.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label", labelComponent+"="+comp)})
		if err != nil {
			errs = append(errs, fmt.Errorf("docker: list VM sidecars for orphan sweep: %w", err))
			continue
		}
		for _, c := range res.Items {
			runID, perr := parseRunID(c.Labels[labelRun])
			if perr != nil || c.Labels[labelComponent] != comp {
				continue
			}
			if c.Labels[runner.LabelRunnerID] != d.cfg.RunnerID || d.otherDeployment(c.Labels) {
				continue
			}
			if time.Unix(c.Created, 0).After(cutoff) || !agentGone(runID) {
				continue
			}
			if _, err := d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
				errs = append(errs, fmt.Errorf("docker: remove orphaned VM sidecar %s: %w", c.ID, err))
				continue
			}
			cleaned[runID] = true
		}
	}
	if vc := d.vmConfig(); vc != nil {
		entries, err := os.ReadDir(filepath.Join(vc.StateDir, "runs"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("docker: read VM run directories: %w", err))
		}
		for _, e := range entries {
			runID, perr := uuid.Parse(e.Name())
			if perr != nil || !e.IsDir() {
				continue
			}
			fi, ierr := e.Info()
			if ierr != nil || fi.ModTime().After(cutoff) || !agentGone(runID) {
				continue
			}
			if err := os.RemoveAll(vc.runDir(runID)); err != nil {
				errs = append(errs, fmt.Errorf("docker: remove orphaned VM run directory %s: %w", runID, err))
				continue
			}
			cleaned[runID] = true
		}
	}
	return len(cleaned), errors.Join(errs...)
}
