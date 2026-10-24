// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// The closed set of kinds, and which of them are VMs.
func TestMachineKind(t *testing.T) {
	for _, tc := range []struct {
		kind         MachineKind
		valid, isVM  bool
		isMacOS      bool
		wantNormal   MachineKind
		wantErrNames []string
	}{
		{kind: "", valid: true, wantNormal: ""},
		{kind: MachineContainer, valid: true, wantNormal: ""},
		{kind: MachineLinuxVM, valid: true, isVM: true, wantNormal: MachineLinuxVM},
		{kind: MachineMacOSVM, valid: true, isVM: true, isMacOS: true, wantNormal: MachineMacOSVM},
		{kind: "windows_vm", wantNormal: "windows_vm", wantErrNames: []string{`"windows_vm"`, `"container"`, `"linux_vm"`, `"macos_vm"`}},
	} {
		err := tc.kind.Validate()
		if (err == nil) != tc.valid {
			t.Errorf("%q: Validate = %v, want valid %v", tc.kind, err, tc.valid)
		}
		for _, name := range tc.wantErrNames {
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%q: Validate error %v does not name %s", tc.kind, err, name)
			}
		}
		if tc.kind.IsVM() != tc.isVM || tc.kind.IsMacOSVM() != tc.isMacOS || tc.kind.Normalize() != tc.wantNormal {
			t.Errorf("%q: IsVM %v IsMacOSVM %v Normalize %q", tc.kind, tc.kind.IsVM(), tc.kind.IsMacOSVM(), tc.kind.Normalize())
		}
	}
}

// A machine is complete for its run's kind: a Linux VM's needs the QEMU fields, a macOS VM's the
// framework's accelerator and machine type and the guest build, and neither passes for the other.
func TestVMMachineCompleteFor(t *testing.T) {
	linux := &VMMachine{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", Accelerator: "kvm", QEMUVersion: "10.0.13"}
	mac := &VMMachine{MachineType: MacVMMachineType, Accelerator: MacVMAccelerator, GuestOS: "macOS 27.0.1 (26A434)", HostOS: "macOS 27.0 (26A400)"}
	macNoGuest := &VMMachine{MachineType: MacVMMachineType, Accelerator: MacVMAccelerator}
	macWrongType := &VMMachine{MachineType: "pc-q35-10.0", Accelerator: MacVMAccelerator, GuestOS: "macOS 27.0.1 (26A434)"}
	for _, tc := range []struct {
		name string
		m    *VMMachine
		kind MachineKind
		want bool
	}{
		{"linux machine, linux run", linux, MachineLinuxVM, true},
		{"linux machine, empty kind keeps Complete", linux, "", true},
		{"mac machine, mac run", mac, MachineMacOSVM, true},
		{"mac machine, linux run", mac, MachineLinuxVM, false},
		{"linux machine, mac run", linux, MachineMacOSVM, false},
		{"mac machine without a guest build", macNoGuest, MachineMacOSVM, false},
		{"mac machine of another machine type", macWrongType, MachineMacOSVM, false},
		{"nil, linux run", nil, MachineLinuxVM, false},
		{"nil, mac run", nil, MachineMacOSVM, false},
	} {
		if got := tc.m.CompleteFor(tc.kind); got != tc.want {
			t.Errorf("%s: CompleteFor = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !linux.Complete() || mac.Complete() {
		t.Errorf("Complete stays the Linux rule: linux %v, mac %v", linux.Complete(), mac.Complete())
	}
}

// A Linux VM's recorded machine reads and writes exactly as before the macOS fields existed.
func TestVMMachineJSONIsAdditive(t *testing.T) {
	linux := VMMachine{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", Accelerator: "kvm", QEMUVersion: "10.0.13"}
	raw, err := json.Marshal(linux)
	if err != nil || string(raw) != `{"machine_type":"pc-q35-10.0","cpu_model":"Haswell-v4","accelerator":"kvm","qemu_version":"10.0.13"}` {
		t.Fatalf("linux machine json = %s (%v)", raw, err)
	}
	var back VMMachine
	if err := json.Unmarshal([]byte(`{"machine_type":"vz-mac","accelerator":"vz","guest_os":"macOS 27.0.1 (26A434)"}`), &back); err != nil ||
		back.GuestOS != "macOS 27.0.1 (26A434)" || !back.CompleteFor(MachineMacOSVM) {
		t.Fatalf("mac machine read back = %+v (%v)", back, err)
	}
}
