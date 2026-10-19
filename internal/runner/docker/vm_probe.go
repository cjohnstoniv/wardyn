// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// fallbackVMUID is who VM containers run as when the runner itself runs as root.
const fallbackVMUID = 65532

// ProbeVM checks that this host can run Linux VM sandboxes and returns c completed with what it
// found: /dev/kvm's group, the containers' user, and the QEMU version and versioned q35 machine. It
// starts one paused, diskless VM in a container hardened as every VM is, whose QMP socket runnerd
// reaches through a bind-mounted directory, so one probe proves KVM inside an unprivileged
// container, the CPU model (with enforce), and a Docker daemon that shares this runner's
// filesystem. Every refusal is one sentence naming its fix.
func (d *Driver) ProbeVM(ctx context.Context, c VMConfig) (VMConfig, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return c, fmt.Errorf("Linux VM sandboxes need an x86-64 Linux runner host (this one is %s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	fi, err := os.Stat("/dev/kvm")
	st, ok := statT(fi)
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 || !ok {
		return c, errors.New("this host has no /dev/kvm: turn on hardware virtualisation and load the kvm module")
	}
	// The QEMU container gets /dev/kvm's group, never root's: a root-group device must be open to all.
	c.KVMGID = ""
	switch {
	case st.Gid != 0:
		c.KVMGID = strconv.FormatUint(uint64(st.Gid), 10)
	case fi.Mode().Perm()&0o006 != 0o006:
		return c, errors.New("/dev/kvm belongs to group root: install the kvm group's udev rule (see the Linux VM sandboxes guide) and restart the runner")
	}
	c.UID, c.GID = os.Getuid(), os.Getgid()
	if c.UID == 0 {
		c.UID, c.GID = fallbackVMUID, fallbackVMUID
	}
	if c.QEMUImage == "" {
		return c, errors.New("no QEMU image: set WARDYN_RUNNER_VM_QEMU_IMAGE to an image built from deploy/runner/vm-qemu")
	}
	if c.CPUModel == "" || strings.ContainsAny(c.CPUModel, ", =") {
		return c, fmt.Errorf("the CPU model %q is not one named QEMU model: fix WARDYN_RUNNER_VM_CPU_MODEL", c.CPUModel)
	}
	if err := d.ensureImage(ctx, c.QEMUImage, "", func() {}); err != nil {
		return c, fmt.Errorf("the QEMU image %s: %w", c.QEMUImage, err)
	}
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return c, err
	}
	dir, err := os.MkdirTemp(c.StateDir, "probe-")
	if err != nil {
		return c, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := c.chown(c.StateDir, dir); err != nil {
		return c, err
	}
	p, err := d.probeQEMU(ctx, c, dir)
	if err != nil {
		return c, err
	}
	c.MachineType, c.QEMUVersion = p.MachineType, p.QEMUVersion
	return c, c.validate()
}

func statT(fi os.FileInfo) (*syscall.Stat_t, bool) {
	if fi == nil {
		return nil, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return st, ok
}

func (d *Driver) probeQEMU(ctx context.Context, c VMConfig, dir string) (qemu.Probed, error) {
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return qemu.Probed{}, fmt.Errorf("docker: info: %w", err)
	}
	host := hardenedHostConfig("none", "", runner.Resources{MemoryMiB: 512, PidsLimit: vmPidsLimit}, info.Info)
	host.ReadonlyRootfs = true
	host.Devices = []container.DeviceMapping{{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"}}
	if c.KVMGID != "" {
		host.GroupAdd = []string{c.KVMGID}
	}
	host.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: qemu.CtlDir}}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "wardyn-vmprobe-" + uuid.NewString()[:8],
		Config: &container.Config{Image: c.QEMUImage, User: c.user(), Cmd: qemu.ProbeArgs(c.CPUModel),
			Labels: map[string]string{labelManaged: "true", labelComponent: "vmprobe"}},
		HostConfig: host,
	})
	if err != nil {
		return qemu.Probed{}, fmt.Errorf("docker: create the VM probe: %w", err)
	}
	defer func() {
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return qemu.Probed{}, fmt.Errorf("docker: start the VM probe: %w", err)
	}
	sock := filepath.Join(dir, qemu.QMPSocket)
	if err := waitForSocket(ctx, sock, 30*time.Second); err != nil {
		return qemu.Probed{}, probeFailure(d.proxyExitLogTail(ctx, created.ID))
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := d.dialUnix(pctx, sock)
	if err != nil {
		return qemu.Probed{}, fmt.Errorf("the VM probe's QMP socket: %w", err)
	}
	q, err := qemu.NewQMP(pctx, conn)
	if err != nil {
		_ = conn.Close()
		return qemu.Probed{}, d.probeExited(ctx, created.ID, fmt.Errorf("the VM probe's QMP: %w", err))
	}
	defer q.Close()
	p, err := q.Probe(pctx)
	if err != nil {
		return qemu.Probed{}, d.probeExited(ctx, created.ID, err)
	}
	_ = q.Execute(pctx, "quit", nil, nil)
	return p, nil
}

// probeExited is a QMP failure of a probe VM that opened its socket and then exited (QEMU creates
// its sockets before it checks the CPU model and KVM): its log names the cause, err otherwise.
func (d *Driver) probeExited(ctx context.Context, id string, err error) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	wait := d.cli.ContainerWait(wctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-wait.Result:
	case <-wait.Error:
		return err
	case <-wctx.Done():
		return err
	}
	if log := d.proxyExitLogTail(ctx, id); log != "" {
		return probeFailure(log)
	}
	return err
}

// probeFailure names the fix for a probe VM that never opened its QMP socket, from its log.
func probeFailure(log string) error {
	switch {
	case strings.Contains(log, "Permission denied") && strings.Contains(log, "KVM"):
		return fmt.Errorf("QEMU cannot open /dev/kvm in its container: add the runner's user to the kvm group (or install the kvm udev rule) and restart the runner: %s", log)
	case strings.Contains(log, "host doesn't support requested feature") || strings.Contains(log, "unable to find CPU model"):
		return fmt.Errorf("this host's CPU cannot run the VM CPU model: set WARDYN_RUNNER_VM_CPU_MODEL to a model it supports: %s", log)
	case log == "":
		return errors.New("the probe VM is running but its QMP socket never appeared here: the Docker engine does not share this runner's filesystem; use a local engine, not Docker Desktop or a remote DOCKER_HOST")
	}
	return fmt.Errorf("the probe VM did not start: %s", log)
}
