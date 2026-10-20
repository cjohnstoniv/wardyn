// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
)

// Linux VM sandboxes are off unless the operator turns them on: no probe runs and the caps offer none.
func TestLinuxVMOffByDefault(t *testing.T) {
	for _, v := range []string{"", "0", "true", "yes"} {
		env := func(k string) string {
			if k == "WARDYN_RUNNER_LINUX_VM" {
				return v
			}
			return ""
		}
		// A nil driver: turning VMs on would probe it and panic, so reaching nil proves no probe ran.
		if caps := enableLinuxVM(context.Background(), nil, t.TempDir(), env); caps != nil {
			t.Fatalf("WARDYN_RUNNER_LINUX_VM=%q offered %+v", v, caps)
		}
	}
}

// The caps say whether the probe found virtiofsd: the organisation sends a VM run that carries a
// folder only to a runner that says so.
func TestVMCapsCarryTheFolderShareProbe(t *testing.T) {
	for _, found := range []bool{true, false} {
		c := docker.VMConfig{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: found}
		if caps := vmCaps(c); caps.Virtiofs != found || caps.MachineType != c.MachineType {
			t.Fatalf("probe found virtiofsd %v, caps %+v", found, caps)
		}
	}
}
