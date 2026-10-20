// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "fmt"

// MachineKind is what a run's sandbox is: a container (the default, and every run before 0.10) or a
// virtual machine. It is its own knob, not a confinement class: CC1..CC3 keep describing containers.
type MachineKind string

const (
	// MachineContainer is today's sandbox. The empty string means the same, and is what a run row
	// stores, so a container run reads and serialises exactly as it did before the knob existed.
	MachineContainer MachineKind = "container"
	// MachineLinuxVM is a Linux guest under QEMU/KVM, started by a registered runner that offers it.
	MachineLinuxVM MachineKind = "linux_vm"
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
	case "", MachineContainer, MachineLinuxVM:
		return nil
	}
	return fmt.Errorf("machine_kind %q is neither %q nor %q", string(k), MachineContainer, MachineLinuxVM)
}

// IsVM reports whether the kind is a virtual machine.
func (k MachineKind) IsVM() bool { return k == MachineLinuxVM }

// VMMachine is the virtual hardware a VM sandbox was started with, recorded on its run so a later
// move or restore can check the target can give the same machine. The runner reports what it
// actually started; the CPU model is started with QEMU's enforce flag, so a host that lacks one of
// its features refuses to start rather than give the guest a different CPU.
type VMMachine struct {
	MachineType string `json:"machine_type"` // versioned, e.g. "pc-q35-10.0"
	CPUModel    string `json:"cpu_model"`    // named and versioned, e.g. "Haswell-v4"
	Accelerator string `json:"accelerator"`  // "kvm"
	QEMUVersion string `json:"qemu_version"` // e.g. "10.0.13"
}

// Complete reports whether every field is set: a VM whose machine cannot be recorded never runs.
func (m *VMMachine) Complete() bool {
	return m != nil && m.MachineType != "" && m.CPUModel != "" && m.Accelerator != "" && m.QEMUVersion != ""
}
