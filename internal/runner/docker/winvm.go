// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// A Windows VM sandbox (machine_kind windows_vm) is a Linux VM's QEMU container booting UEFI firmware
// from an organisation's registered disk image, beside a software TPM sidecar, the run's network relay
// and proxy. It has no guest agent: its desktop is the only way in, so every verb that would reach the
// guest answers errWinVMNoGuestAgent. It needs Linux VM sandboxes on (it shares their QEMU image, state
// directory and probe); EnableWindowsVM turns it on after ProbeWindowsVM.

// componentVMTPM labels a Windows VM's swtpm sidecar, which the orphan sweep lists.
const componentVMTPM = "vmtpm"

// vmtpmStep names the swtpm sidecar as a run's prep containers are named, so removeVMSidecars' list
// of them reaches it.
const vmtpmStep = "tpm"

// winVMMinMemoryMiB is the least guest memory a Windows VM boots with: Windows 11's 4 GB minimum.
// It is the guest's share after qemu.Memory's split, not the run's cap.
const winVMMinMemoryMiB = 4096

// winGuestMiB is the guest memory a run cap gives a VM; a cap qemu.Memory refuses gives none.
func winGuestMiB(capMiB int64) int64 {
	guestMiB, _, err := qemu.Memory(capMiB)
	if err != nil {
		return 0
	}
	return guestMiB
}

// winVMMinCapMiB is the smallest run memory cap whose guest share reaches winVMMinMemoryMiB.
func winVMMinCapMiB() int64 {
	c := int64(winVMMinMemoryMiB)
	for winGuestMiB(c) < winVMMinMemoryMiB {
		c++
	}
	return c
}

// WinVMConfig turns Windows VM sandboxes on; ProbeWindowsVM fills it.
type WinVMConfig struct {
	// SWTPMVersion is what the QEMU image's swtpm said it is.
	SWTPMVersion string
}

// winVMConfig is the Windows VM config, or nil while Windows VMs (or Linux VMs, which they need) are off.
func (d *Driver) winVMConfig() *WinVMConfig {
	if vc := d.vmConfig(); vc != nil {
		return vc.Windows
	}
	return nil
}

// EnableWindowsVM turns Windows VM sandboxes on beside Linux ones; a nil config turns them off.
func (d *Driver) EnableWindowsVM(c *WinVMConfig) error {
	vc := d.vmConfig()
	if vc == nil {
		if c == nil {
			return nil
		}
		return errors.New("docker: Windows VM sandboxes need Linux VM sandboxes on this runner")
	}
	nc := *vc
	nc.Windows = c
	d.vmCfg.Store(&nc)
	return nil
}

// isWinVM says whether a container's config is a Windows VM's QEMU container, by the label createWinVM sets.
func isWinVM(c *container.Config) bool {
	return c != nil && c.Labels[labelMachineKind] == string(types.MachineWindowsVM)
}

// errWinVMNoGuestAgent is every guest agent call's answer on a Windows VM.
var errWinVMNoGuestAgent = errors.New("docker: a Windows VM run has no guest agent: it is desktop-only")

// newWinVMRun is a Windows VM as the driver knows it: its agent's dial refuses, so every agent call
// fails with errWinVMNoGuestAgent and holds no connection.
func (d *Driver) newWinVMRun(runID uuid.UUID, dir string) *vmRun {
	ctx, stop := context.WithCancel(context.Background())
	return &vmRun{runID: runID, dir: dir, ctx: ctx, stop: stop, win: &winVM{},
		agent: guestagent.NewClient(func(context.Context) (net.Conn, error) { return nil, errWinVMNoGuestAgent })}
}

// vmRunOf is the vmRun of a QEMU container found by its machine-kind label.
func (d *Driver) vmRunOf(kind types.MachineKind, runID uuid.UUID, dir string) *vmRun {
	if kind == types.MachineWindowsVM {
		return d.newWinVMRun(runID, dir)
	}
	return d.newVMRun(runID, dir)
}

// refuseWinVMSpec refuses, before anything is created, what a Windows VM cannot honour: it has no
// guest agent to mount a folder or a drive, take secret env or record, and it is desktop-only.
func (d *Driver) refuseWinVMSpec(spec runner.SandboxSpec) error {
	if d.winVMConfig() == nil {
		return errors.New("docker: this runner does not offer Windows VM sandboxes")
	}
	switch {
	case spec.MachineKind != types.MachineWindowsVM:
		return fmt.Errorf("docker: unknown machine kind %q", spec.MachineKind)
	case len(spec.Mounts) > 0:
		return errors.New("docker: a Windows VM sandbox takes no folder: it has no guest agent to mount one")
	case len(spec.Drives) > 0:
		return errors.New("docker: a Windows VM sandbox takes no drive: it has no guest agent to mount one")
	case spec.RecordWithin(d.cfg.Record):
		return errors.New("docker: a Windows VM sandbox is never recorded; turn recording off for this run")
	case len(spec.SecretEnv) > 0:
		return errors.New("docker: a Windows VM sandbox takes no secret env: it has no guest agent to hand it to")
	case !spec.Interactive:
		return errors.New("docker: a Windows VM sandbox is desktop-only: it takes no task or background run")
	case winGuestMiB(spec.Resources.MemoryMiB) < winVMMinMemoryMiB:
		return fmt.Errorf("docker: a Windows VM sandbox needs %d MiB of guest memory, which takes a run memory cap of at least %d MiB; this run has %d", winVMMinMemoryMiB, winVMMinCapMiB(), spec.Resources.MemoryMiB)
	case spec.Resources.DiskMiB > 0 && !spec.Resources.DiskMiBSoft:
		return errors.New("docker: a Windows VM run can't enforce a disk limit; remove disk_mib")
	}
	return d.preflightSpec(spec)
}

// createWinVM is CreateSandbox for a Windows VM: createVM's network, proxy, relay and hardened QEMU
// container, with the registered image's disk, UEFI firmware and a swtpm sidecar in place of a
// kernel, shares and a guest agent. Everything created before an error is rolled back.
//
//nolint:funlen // One linear assembly sequence whose rollback must stay in one scope, as createVM's.
func (d *Driver) createWinVM(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	if err := d.refuseWinVMSpec(spec); err != nil {
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
	imageID, err := d.vmImages(ctx, spec, vc.QEMUImage)
	if err != nil {
		return runner.Sandbox{}, err
	}
	var rollback []func()
	fail := func(err error) (runner.Sandbox, error) {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
		return runner.Sandbox{}, err
	}
	_, proxyIP, proxySHA, undoNet, err := d.runNetworkAndProxy(ctx, spec, false)
	if err != nil {
		return runner.Sandbox{}, err
	}
	rollback = append(rollback, undoNet)
	base, err := d.winVMImage(ctx, spec, imageID)
	if err != nil {
		return fail(err)
	}
	dir := vc.runDir(spec.RunID)
	rollback = append(rollback, func() { _ = os.RemoveAll(dir) })
	if err := d.makeRunDir(dir, ""); err != nil {
		return fail(err)
	}
	part := func(p string) string { return filepath.Join(dir, p) }
	tpmState := filepath.Join(part("ctl"), qemu.TPMStateDir)
	if err := os.Mkdir(tpmState, 0o700); err != nil {
		return fail(fmt.Errorf("docker: VM run directory: %w", err))
	}
	if err := vc.chown(tpmState); err != nil {
		return fail(err)
	}
	// The run's overlay on the read-only base, and its copy of the firmware variables.
	if err := d.runOneShot(ctx, spec.RunID, "overlay", qemu.WinOverlayArgs(), []mount.Mount{
		{Type: mount.TypeBind, Source: base, Target: qemu.BaseDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: part("disk"), Target: qemu.DiskDir},
	}); err != nil {
		return fail(err)
	}
	rollback = append(rollback, func() { d.warnVMSidecars(context.Background(), spec.RunID) })
	if err := d.startWinTPM(ctx, spec, part("ctl")); err != nil {
		return fail(err)
	}
	if err := waitForSocket(ctx, filepath.Join(part("ctl"), qemu.TPMSocket), 30*time.Second); err != nil {
		return fail(fmt.Errorf("docker: the VM's TPM: %w", err))
	}
	if err := d.startVMNet(ctx, spec, part("net"), proxyIP); err != nil {
		return fail(err)
	}
	if err := waitForSocket(ctx, filepath.Join(part("net"), qemu.NetSocket), 30*time.Second); err != nil {
		return fail(fmt.Errorf("docker: the VM's network relay: %w", err))
	}
	argv, err := qemu.WinArgs(qemu.WinConfig{RunID: spec.RunID, MachineType: vc.MachineType, CPUModel: vc.CPUModel,
		VCPUs: qemu.VCPUs(spec.Resources.CPUMillis), MemoryMiB: guestMiB})
	if err != nil {
		return fail(err)
	}
	host := d.vmHostConfig(runner.Resources{CPUMillis: spec.Resources.CPUMillis, MemoryMiB: containerMiB, PidsLimit: vmPidsLimit}, infoRes.Info)
	host.Devices = append(host.Devices, container.DeviceMapping{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"})
	if vc.KVMGID != "" {
		host.GroupAdd = append(host.GroupAdd, vc.KVMGID)
	}
	host.Mounts = []mount.Mount{
		{Type: mount.TypeBind, Source: base, Target: qemu.BaseDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: part("ctl"), Target: qemu.CtlDir},
		{Type: mount.TypeBind, Source: part("net"), Target: qemu.NetDir},
		{Type: mount.TypeBind, Source: part("disk"), Target: qemu.DiskDir},
	}
	labels := d.labels(spec.RunID, componentAgent, spec.Labels)
	labels[labelMachineKind] = string(types.MachineWindowsVM)
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
	if capErr := verifyCapsEnforced(created.Warnings); capErr != nil && !d.cfg.AllowUnenforceableCaps {
		return fail(capErr)
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fail(fmt.Errorf("docker: start the VM: %w", err))
	}
	v := d.newWinVMRun(spec.RunID, dir)
	rollback = append(rollback, func() { v.stop(); v.agent.Close() })
	// No guest agent to wait for: the VM is up once QEMU answers QMP and runs under KVM.
	if err := waitForSocket(ctx, filepath.Join(part("ctl"), qemu.QMPSocket), 30*time.Second); err != nil {
		return fail(fmt.Errorf("docker: the VM did not start: %w%s", err, d.vmLogTail(name)))
	}
	if err := d.vmUnderKVM(ctx, part("ctl")); err != nil {
		return fail(err)
	}
	d.vms.Store(name, v)
	machine := &types.VMMachine{MachineType: vc.MachineType, CPUModel: vc.CPUModel, Accelerator: "kvm", QEMUVersion: vc.QEMUVersion}
	return runner.Sandbox{Ref: name, Driver: driverName, EnforcedClass: types.CC3, Machine: machine,
		ProxyConfigSHA256: proxySHA, ProxyAddr: proxyIP}, nil
}

func vmtpmName(runID uuid.UUID) string { return vmPrepName(runID, vmtpmStep) }

// startWinTPM starts the run's swtpm sidecar from the QEMU image, hardened as every VM container is
// (the runner's user, no capability, no network, a read-only root), with the run's ctl/ part as its
// only mount: its state and its control socket live there, and go with the run directory.
func (d *Driver) startWinTPM(ctx context.Context, spec runner.SandboxSpec, ctlDir string) error {
	vc := d.vmConfig()
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return fmt.Errorf("docker: info: %w", err)
	}
	host := d.vmHostConfig(runner.Resources{MemoryMiB: 128, PidsLimit: vmPidsLimit}, info.Info)
	host.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: ctlDir, Target: qemu.CtlDir}}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       vmtpmName(spec.RunID),
		Config:     &container.Config{Image: vc.QEMUImage, User: vc.user(), Cmd: qemu.SWTPMArgs(), Labels: d.labels(spec.RunID, componentVMTPM, spec.Labels)},
		HostConfig: host,
	})
	if err != nil {
		return fmt.Errorf("docker: create the VM's TPM: %w", err)
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker: start the VM's TPM: %w", err)
	}
	return nil
}

// ProbeWindowsVM checks that the QEMU image can run Windows VM sandboxes: in one container hardened
// as a VM's and given /dev/kvm, the UEFI firmware files are there, swtpm runs, and /dev/kvm opens.
// Linux VM sandboxes must be on (ProbeVM and EnableVM first).
func (d *Driver) ProbeWindowsVM(ctx context.Context) (WinVMConfig, error) {
	vc := d.vmConfig()
	if vc == nil {
		return WinVMConfig{}, errors.New("docker: Windows VM sandboxes need Linux VM sandboxes on this runner: set WARDYN_RUNNER_LINUX_VM=1")
	}
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return WinVMConfig{}, fmt.Errorf("docker: info: %w", err)
	}
	host := d.vmHostConfig(runner.Resources{MemoryMiB: 256, PidsLimit: vmPidsLimit}, info.Info)
	host.Devices = []container.DeviceMapping{{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"}}
	if vc.KVMGID != "" {
		host.GroupAdd = []string{vc.KVMGID}
	}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "wardyn-vmprobe-" + uuid.NewString()[:8],
		Config: &container.Config{Image: vc.QEMUImage, User: vc.user(), Cmd: []string{"wardyn-winvm-disk", "probe", qemu.OVMFCode, qemu.OVMFVars},
			Labels: map[string]string{labelManaged: "true", labelComponent: "vmprobe"}},
		HostConfig: host,
	})
	if err != nil {
		return WinVMConfig{}, fmt.Errorf("docker: create the Windows VM probe: %w", err)
	}
	defer func() {
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wait := d.cli.ContainerWait(wctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return WinVMConfig{}, fmt.Errorf("docker: start the Windows VM probe: %w", err)
	}
	select {
	case r := <-wait.Result:
		log := d.proxyExitLogTail(ctx, created.ID)
		if r.StatusCode != 0 {
			return WinVMConfig{}, fmt.Errorf("the QEMU image cannot run Windows VMs (exit %d): %s; use an image built from deploy/runner/vm-qemu", r.StatusCode, log)
		}
		return WinVMConfig{SWTPMVersion: strings.TrimSpace(log)}, nil
	case err := <-wait.Error:
		return WinVMConfig{}, fmt.Errorf("the Windows VM probe: %w", err)
	case <-wctx.Done():
		return WinVMConfig{}, wctx.Err()
	}
}
