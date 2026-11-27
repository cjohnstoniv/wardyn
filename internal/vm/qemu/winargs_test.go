// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testWinConfig() WinConfig {
	return WinConfig{RunID: uuid.MustParse("6f0c0d8e-4b1a-4c9e-9a51-0d1f2e3a4b5c"), MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", VCPUs: 4, MemoryMiB: 6554}
}

// The whole argv, so any change to what a Windows guest is given is a reviewed change to this list.
func TestWinArgsGolden(t *testing.T) {
	a, err := WinArgs(testWinConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"qemu-system-x86_64", "-name", "wardyn-6f0c0d8e-4b1a-4c9e-9a51-0d1f2e3a4b5c",
		"-nodefaults", "-no-user-config",
		"-machine", "pc-q35-10.0,accel=kvm,smm=on",
		"-global", "driver=cfi.pflash01,property=secure,value=on",
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.ms.fd",
		"-drive", "if=pflash,format=raw,unit=1,file=/vm/disk/efivars.fd",
		"-cpu", "Haswell-v4,enforce,hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time",
		"-smp", "4", "-m", "6554",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-chardev", "socket,id=tpm0,path=/vm/ctl/tpm.sock",
		"-tpmdev", "emulator,id=tpm0,chardev=tpm0", "-device", "tpm-crb,tpmdev=tpm0",
		"-device", "ahci,id=ahci0",
		"-drive", "file=/vm/disk/overlay.qcow2,if=none,id=disk0,format=qcow2,discard=unmap",
		"-device", "ide-hd,drive=disk0,bus=ahci0.0",
		"-device", "VGA", "-vnc", "unix:/vm/ctl/vnc.sock", "-display", "none",
		"-device", "qemu-xhci,id=xhci0", "-device", "usb-tablet,bus=xhci0.0",
		"-monitor", "none",
		"-qmp", "unix:/vm/ctl/qmp.sock,server=on,wait=off",
		"-netdev", "stream,id=n0,server=off,reconnect-ms=1000,addr.type=unix,addr.path=/vm/net/net.sock",
		"-device", "e1000e,netdev=n0,mac=52:54:00:12:34:56,romfile=",
	}
	if !slices.Equal(a, want) {
		t.Fatalf("WinArgs =\n%q\nwant\n%q", a, want)
	}
	if _, err := WinArgs(WinConfig{RunID: uuid.New(), MachineType: "pc", CPUModel: "Haswell-v4", VCPUs: 2, MemoryMiB: 4096}); err == nil {
		t.Fatal("an unversioned machine type was accepted")
	}
}

// Nothing a Linux VM's guest agent, shares or kernel boot use, and nothing that would give the guest
// or QEMU a network of its own or a host path.
func TestWinArgsBannedTokens(t *testing.T) {
	a, err := WinArgs(testWinConfig())
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(a, " ")
	for _, banned := range []string{"-kernel", "-initrd", "-append", "virtserialport", "virtio-serial", "vhost-user-fs", "-fw_cfg",
		"tcp:", "-net user", "user,", "hostfwd", "smb=", "virtio", "-device vfio", "passthrough", "-spice", "guest_agent"} {
		if strings.Contains(line, banned) {
			t.Errorf("WinArgs carries %q: %s", banned, line)
		}
	}
	if argAfter(t, a, "-netdev") != "stream,id=n0,server=off,reconnect-ms=1000,addr.type=unix,addr.path="+NetDir+"/"+NetSocket {
		t.Fatal("the only NIC is not the relay's unix stream")
	}
	if !strings.Contains(argAfter(t, a, "-sandbox"), "spawn=deny") {
		t.Fatal("QEMU's seccomp filter does not deny spawn")
	}
}

func TestWinPrepArgsNameOnlyFixedPaths(t *testing.T) {
	ov := []string{"sh", "-ec", "qemu-img create -q -f qcow2 -F qcow2 -b /vm/base/disk.qcow2 /vm/disk/overlay.qcow2; " +
		"cp /usr/share/OVMF/OVMF_VARS_4M.ms.fd /vm/disk/efivars.fd"}
	if got := WinOverlayArgs(); !slices.Equal(got, ov) {
		t.Errorf("overlay step %q, want %q", got, ov)
	}
	tpm := []string{"swtpm", "socket", "--tpm2", "--tpmstate", "dir=/vm/ctl/tpm", "--ctrl", "type=unixio,path=/vm/ctl/tpm.sock", "--terminate"}
	if got := SWTPMArgs(); !slices.Equal(got, tpm) {
		t.Errorf("swtpm %q, want %q", got, tpm)
	}
}
