// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"strconv"

	"github.com/google/uuid"
)

// A Windows VM sandbox boots UEFI firmware from an organisation's own disk image, with a software TPM
// and only devices Windows has in-box drivers for. It has no guest agent, no kernel of Wardyn's, no
// shared folder and no fw_cfg file: its desktop is the only way in.

// Firmware in the QEMU image (Debian's ovmf package): the Secure Boot code and the variable store
// with Microsoft's keys enrolled, which each run copies (WinVars) so its own variables persist.
const (
	OVMFCode = "/usr/share/OVMF/OVMF_CODE_4M.ms.fd"
	OVMFVars = "/usr/share/OVMF/OVMF_VARS_4M.ms.fd"
)

// Windows VM files within the run directory's parts.
const (
	WinDisk     = "disk.qcow2" // the converted image under BaseDir; read-only, shared by every run of the image
	WinVars     = "efivars.fd" // the run's copy of OVMFVars under DiskDir
	TPMSocket   = "tpm.sock"   // the swtpm sidecar's control socket under CtlDir
	TPMStateDir = "tpm"        // the swtpm sidecar's state under CtlDir
)

// WinConfig is one Windows VM's command line.
type WinConfig struct {
	RunID       uuid.UUID
	MachineType string // versioned, e.g. pc-q35-10.0
	CPUModel    string // named, e.g. Haswell-v4; started with enforce
	VCPUs       int
	MemoryMiB   int64
}

// winCPUFlags are the Hyper-V enlightenments Windows uses for its timers, interrupts and spinlocks.
const winCPUFlags = ",hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time"

// WinArgs is the full argv of a Windows VM. As Args, every endpoint is a unix socket under CtlDir or
// NetDir: QEMU opens no TCP listener and has no network of its own.
func WinArgs(c WinConfig) ([]string, error) {
	if err := (Config{RunID: c.RunID, MachineType: c.MachineType, CPUModel: c.CPUModel, VCPUs: c.VCPUs, MemoryMiB: c.MemoryMiB}).Validate(); err != nil {
		return nil, err
	}
	run := c.RunID.String()
	return []string{
		"qemu-system-x86_64", "-name", "wardyn-" + run,
		"-nodefaults", "-no-user-config",
		// smm: Secure Boot firmware keeps its variables out of the guest's reach only in SMM.
		"-machine", c.MachineType + ",accel=kvm,smm=on",
		"-global", "driver=cfi.pflash01,property=secure,value=on",
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=" + OVMFCode,
		"-drive", "if=pflash,format=raw,unit=1,file=" + DiskDir + "/" + WinVars,
		"-cpu", c.CPUModel + ",enforce" + winCPUFlags,
		"-smp", strconv.Itoa(c.VCPUs), "-m", strconv.FormatInt(c.MemoryMiB, 10),
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		// TPM 2.0 from the run's swtpm sidecar, through its control socket.
		"-chardev", "socket,id=tpm0,path=" + CtlDir + "/" + TPMSocket,
		"-tpmdev", "emulator,id=tpm0,chardev=tpm0", "-device", "tpm-crb,tpmdev=tpm0",
		"-device", "ahci,id=ahci0",
		"-drive", "file=" + DiskDir + "/" + Overlay + ",if=none,id=disk0,format=qcow2,discard=unmap",
		"-device", "ide-hd,drive=disk0,bus=ahci0.0",
		"-device", "VGA", "-vnc", "unix:" + CtlDir + "/" + VNCSocket, "-display", "none",
		"-device", "qemu-xhci,id=xhci0", "-device", "usb-tablet,bus=xhci0.0",
		"-monitor", "none",
		"-qmp", "unix:" + CtlDir + "/" + QMPSocket + ",server=on,wait=off",
		// The guest's only NIC, as a Linux VM's: a stream to the network relay. No option ROM: the
		// firmware never network-boots.
		"-netdev", "stream,id=n0,server=off,reconnect-ms=1000,addr.type=unix,addr.path=" + NetDir + "/" + NetSocket,
		"-device", "e1000e,netdev=n0,mac=" + GuestMAC + ",romfile=",
	}, nil
}

// WinOverlayArgs creates the run's overlay on the converted image and the run's copy of the firmware
// variables, in one prep step from the QEMU image. Nothing in it comes from the run.
func WinOverlayArgs() []string {
	return []string{"sh", "-ec", "qemu-img create -q -f qcow2 -F qcow2 -b " + BaseDir + "/" + WinDisk + " " + DiskDir + "/" + Overlay +
		"; cp " + OVMFVars + " " + DiskDir + "/" + WinVars}
}

// SWTPMArgs is the run's software TPM 2.0: its state under CtlDir/TPMStateDir, its control socket
// at CtlDir/TPMSocket, and it exits when QEMU drops the connection.
func SWTPMArgs() []string {
	return []string{"swtpm", "socket", "--tpm2", "--tpmstate", "dir=" + CtlDir + "/" + TPMStateDir,
		"--ctrl", "type=unixio,path=" + CtlDir + "/" + TPMSocket, "--terminate"}
}
