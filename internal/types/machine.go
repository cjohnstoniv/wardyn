// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"time"
)

// MachineKind is what a run's sandbox is: a container (the default, and every run before 0.10) or a
// virtual machine. It is its own knob, not a confinement class: CC1..CC3 keep describing containers.
type MachineKind string

const (
	// MachineContainer is today's sandbox. The empty string means the same, and is what a run row
	// stores, so a container run reads and serialises exactly as it did before the knob existed.
	MachineContainer MachineKind = "container"
	// MachineLinuxVM is a Linux guest under QEMU/KVM, started by a registered runner that offers it.
	MachineLinuxVM MachineKind = "linux_vm"
	// MachineMacOSVM is a macOS guest under Apple's Virtualization.framework, started by a registered
	// runner on a managed Mac that offers it. Off unless the organisation turns it on (WARDYN_MACOS_VMS).
	MachineMacOSVM MachineKind = "macos_vm"
)

// Normalize maps "container" to "", the stored form; any other value is returned unchanged.
func (k MachineKind) Normalize() MachineKind {
	if k == MachineContainer {
		return ""
	}
	return k
}

// Validate refuses a kind outside the closed set.
func (k MachineKind) Validate() error {
	switch k {
	case "", MachineContainer, MachineLinuxVM, MachineMacOSVM:
		return nil
	}
	return fmt.Errorf("machine_kind %q is not %q, %q or %q", string(k), MachineContainer, MachineLinuxVM, MachineMacOSVM)
}

// IsVM reports whether the kind is any virtual machine; a site that depends on which kind says so with
// IsMacOSVM or a comparison with MachineLinuxVM.
func (k MachineKind) IsVM() bool { return k == MachineLinuxVM || k == MachineMacOSVM }

// IsMacOSVM reports whether the kind is a macOS virtual machine.
func (k MachineKind) IsMacOSVM() bool { return k == MachineMacOSVM }

// VMMachine is the virtual hardware a VM sandbox was started with, recorded on its run so a later
// move or restore can check the target can give the same machine. The runner reports what it
// actually started; the CPU model is started with QEMU's enforce flag, so a host that lacks one of
// its features refuses to start rather than give the guest a different CPU.
type VMMachine struct {
	MachineType string `json:"machine_type"` // versioned, e.g. "pc-q35-10.0"
	CPUModel    string `json:"cpu_model"`    // named and versioned, e.g. "Haswell-v4"
	Accelerator string `json:"accelerator"`  // "kvm"
	QEMUVersion string `json:"qemu_version"` // e.g. "10.0.13"
	// GuestOS and HostOS are a macOS VM's guest build and the Mac's, e.g. "macOS 27.0.1 (26A434)". Absent
	// on a Linux VM, whose machine they do not describe.
	GuestOS string `json:"guest_os,omitempty"`
	HostOS  string `json:"host_os,omitempty"`
	// BaseEditedAt is when a macOS VM's base was last edited (`macos-vm edit-base`) before the run cloned it;
	// absent when it never was, and on a Linux VM.
	BaseEditedAt *time.Time `json:"base_edited_at,omitempty"`
}

// A macOS VM's machine: Virtualization.framework gives the guest no CPU model or QEMU, so the machine
// is named by these two words and the guest build.
const (
	MacVMAccelerator = "vz"
	MacVMMachineType = "vz-mac"
)

// Complete reports whether every field of a Linux VM's machine is set: a VM whose machine cannot be
// recorded never runs.
func (m *VMMachine) Complete() bool {
	return m != nil && m.MachineType != "" && m.CPUModel != "" && m.Accelerator != "" && m.QEMUVersion != ""
}

// CompleteFor is Complete for the kind of the run the machine was recorded on: a macOS VM's machine
// needs the framework's accelerator and machine type and the guest build; every other kind keeps
// Complete's rule.
func (m *VMMachine) CompleteFor(k MachineKind) bool {
	if k != MachineMacOSVM {
		return m.Complete()
	}
	return m != nil && m.Accelerator == MacVMAccelerator && m.MachineType == MacVMMachineType && m.GuestOS != ""
}
