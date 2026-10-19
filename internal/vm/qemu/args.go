// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package qemu builds the QEMU command line of a Linux VM sandbox and speaks QMP and the QEMU guest
// agent protocol. It starts nothing: the runner's Docker driver runs QEMU in its own unprivileged
// container, with the run directory's parts mounted at the paths below.
package qemu

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Paths inside the QEMU container. Each is one part of the run directory (or the image cache),
// mounted into the containers that need it and no others.
const (
	BaseDir  = "/vm/base"  // the image's rootfs.raw, vmlinuz, initrd.img; read-only, shared by every run of the image
	CtlDir   = "/vm/ctl"   // QMP, QGA, guest agent and VNC sockets: QEMU and the runner only
	NetDir   = "/vm/net"   // net.sock, served by the network relay
	FSDir    = "/vm/fs"    // virtiofsd sockets
	DiskDir  = "/vm/disk"  // the run's overlay
	FWCfgDir = "/vm/fwcfg" // files QEMU hands the guest over fw_cfg; read-only
)

// Socket and file names within the parts.
const (
	QMPSocket   = "qmp.sock"
	QGASocket   = "qga.sock"
	AgentSocket = "agent.sock"
	VNCSocket   = "vnc.sock"
	NetSocket   = "net.sock"
	Overlay     = "overlay.qcow2"
	RootFS      = "rootfs.raw"
	Kernel      = "vmlinuz"
	Initrd      = "initrd.img"
	CAFile      = "ca.pem"
)

// GuestMAC is the guest NIC's address, fixed so the network relay (and a restored VM) sees one peer.
const GuestMAC = "52:54:00:12:34:56"

// FWCfg names: QEMU reserves "opt/" for users and asks for a reverse-domain prefix.
const (
	FWCfgRunID = "opt/org.wardyn/run-id"
	FWCfgCA    = "opt/org.wardyn/ca.pem"
)

// Config is one VM's command line.
type Config struct {
	RunID       uuid.UUID
	MachineType string // versioned, e.g. pc-q35-10.0
	CPUModel    string // named, e.g. Haswell-v4; started with enforce
	VCPUs       int
	MemoryMiB   int64
	// CA says the run has a CA bundle for the guest at FWCfgDir/CAFile.
	CA bool
	// Shares is the number of virtiofs shares; share n is FSDir/<n>.sock, tag ShareTag(n).
	Shares int
}

// ShareTag is the virtiofs tag of share n, which the guest agent mounts.
func ShareTag(n int) string { return "wardyn" + strconv.Itoa(n) }

// ShareSocket is the vhost-user socket name of share n under FSDir.
func ShareSocket(n int) string { return strconv.Itoa(n) + ".sock" }

// Validate refuses a config that would start QEMU on a machine the runner cannot record.
func (c Config) Validate() error {
	switch {
	case c.RunID == uuid.Nil:
		return fmt.Errorf("qemu: a VM needs a run id")
	case !strings.HasPrefix(c.MachineType, "pc-q35-"):
		return fmt.Errorf("qemu: machine type %q is not a versioned q35 machine", c.MachineType)
	case c.CPUModel == "" || strings.ContainsAny(c.CPUModel, ", ="):
		return fmt.Errorf("qemu: CPU model %q is not one named model", c.CPUModel)
	case c.VCPUs < 1 || c.MemoryMiB < 256:
		return fmt.Errorf("qemu: %d vCPUs and %d MiB is too small a VM", c.VCPUs, c.MemoryMiB)
	case c.Shares < 0:
		return fmt.Errorf("qemu: negative share count")
	}
	return nil
}

// Args is the full argv. Every endpoint is a unix socket under CtlDir, NetDir or FSDir: QEMU opens
// no TCP listener and has no network of its own (its container runs with none).
func Args(c Config) ([]string, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	run := c.RunID.String()
	mem := strconv.FormatInt(c.MemoryMiB, 10)
	a := []string{
		"qemu-system-x86_64", "-name", "wardyn-" + run,
		"-nodefaults", "-no-user-config",
		"-machine", c.MachineType + ",accel=kvm",
		// enforce: a host that lacks a feature of the model refuses to start, so the recorded
		// model is the CPU the guest actually has.
		"-cpu", c.CPUModel + ",enforce",
		"-smp", strconv.Itoa(c.VCPUs), "-m", mem,
		// Shared guest memory, which vhost-user (virtiofs) needs; one layout for every VM.
		"-object", "memory-backend-memfd,id=mem,size=" + mem + "M,share=on", "-numa", "node,memdev=mem",
		// QEMU's own seccomp filter; spawn=deny also closes QMP's migrate exec:.
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-kernel", BaseDir + "/" + Kernel, "-initrd", BaseDir + "/" + Initrd,
		"-append", "root=/dev/vda rw console=ttyS0 noresume wardyn.run=" + run,
		"-drive", "file=" + DiskDir + "/" + Overlay + ",if=virtio,format=qcow2,discard=unmap",
		"-device", "virtio-vga", "-vnc", "unix:" + CtlDir + "/" + VNCSocket, "-display", "none",
		"-serial", "stdio", "-monitor", "none",
		"-qmp", "unix:" + CtlDir + "/" + QMPSocket + ",server=on,wait=off",
		"-device", "virtio-serial-pci",
		"-chardev", "socket,id=qga,path=" + CtlDir + "/" + QGASocket + ",server=on,wait=off",
		"-device", "virtserialport,chardev=qga,name=org.qemu.guest_agent.0",
		"-chardev", "socket,id=wagent,path=" + CtlDir + "/" + AgentSocket + ",server=on,wait=off",
		"-device", "virtserialport,chardev=wagent,name=org.wardyn.agent.0",
		// The guest's only NIC: a stream to the network relay, which may start before or after QEMU
		// and restart; nothing answering means nothing is reachable.
		"-netdev", "stream,id=n0,server=off,reconnect-ms=1000,addr.type=unix,addr.path=" + NetDir + "/" + NetSocket,
		"-device", "virtio-net-pci,netdev=n0,mac=" + GuestMAC,
		"-device", "virtio-rng-pci",
		"-fw_cfg", "name=" + FWCfgRunID + ",string=" + run,
	}
	if c.CA {
		a = append(a, "-fw_cfg", "name="+FWCfgCA+",file="+FWCfgDir+"/"+CAFile)
	}
	for n := range c.Shares {
		id := "fs" + strconv.Itoa(n)
		a = append(a, "-chardev", "socket,id="+id+",path="+FSDir+"/"+ShareSocket(n),
			"-device", "vhost-user-fs-pci,chardev="+id+",tag="+ShareTag(n))
	}
	return a, nil
}

// OverlayArgs creates the run's overlay on the read-only base.
func OverlayArgs() []string {
	return []string{"qemu-img", "create", "-q", "-f", "qcow2", "-F", "raw", "-b", BaseDir + "/" + RootFS, DiskDir + "/" + Overlay}
}

// VirtiofsdArgs serves share n of the source mounted at /share.
func VirtiofsdArgs(n int, readOnly bool) []string {
	a := []string{"/usr/libexec/virtiofsd", "--socket-path=" + FSDir + "/" + ShareSocket(n), "--shared-dir=/share",
		// The container is the confinement: virtiofsd's own sandbox needs privileges it is not given.
		"--sandbox=none", "--cache=auto"}
	if readOnly {
		a = append(a, "--readonly")
	}
	return a
}

// ProbeArgs starts a paused, diskless VM whose QMP socket waits for the runner: the start-up probe
// of KVM, the CPU model and the machine type in a container hardened as every VM is.
func ProbeArgs(cpuModel string) []string {
	return []string{"qemu-system-x86_64", "-nodefaults", "-no-user-config", "-display", "none", "-S",
		"-machine", "q35,accel=kvm", "-cpu", cpuModel + ",enforce", "-m", "64",
		"-qmp", "unix:" + CtlDir + "/" + QMPSocket + ",server=on,wait=on"}
}

// VCPUs is a run's vCPU count from its CPU cap: one per started thousand millicores, two by default.
func VCPUs(cpuMillis int64) int {
	if cpuMillis <= 0 {
		return 2
	}
	return int((cpuMillis + 999) / 1000)
}

// Memory splits a run's memory cap between the guest and QEMU itself, so the QEMU container's limit
// is the cap the run was admitted with. QEMU's share is max(512 MiB, a fifth); S1 measured 1.44 GiB
// accounted for a 2 GiB guest with virtiofs. No cap gives a 2 GiB guest in a 2.5 GiB container. A cap
// too small for a 256 MiB guest is refused.
func Memory(capMiB int64) (guestMiB, containerMiB int64, err error) {
	if capMiB <= 0 {
		return 2048, 2560, nil
	}
	guestMiB = capMiB - max(512, capMiB/5)
	if guestMiB < 256 {
		return 0, 0, fmt.Errorf("qemu: a %d MiB memory cap leaves under 256 MiB for the guest; a VM needs at least 768 MiB", capMiB)
	}
	return guestMiB, capMiB, nil
}
