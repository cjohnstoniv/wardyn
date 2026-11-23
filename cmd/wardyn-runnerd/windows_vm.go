// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// windowsVMDriver is the part of the Docker driver that turns Windows VM sandboxes on.
type windowsVMDriver interface {
	ProbeWindowsVM(ctx context.Context) (docker.WinVMConfig, error)
	EnableWindowsVM(c *docker.WinVMConfig) error
}

// enableWindowsVM turns Windows VM sandboxes on when the operator asked for them
// (WARDYN_RUNNER_WINDOWS_VM=1), Linux VM sandboxes are on (linux, their caps: Windows VMs share their
// QEMU image, state directory and probe), and the QEMU image passes the Windows probe (UEFI firmware,
// swtpm, /dev/kvm). It returns what the runner's caps offer: the Linux VMs' machine, and the desktop,
// which is all a Windows VM has. Off by default; a failed probe logs its fix and offers none.
func enableWindowsVM(ctx context.Context, sub windowsVMDriver, linux *runnerwire.VMCaps, getenv func(string) string) *runnerwire.VMCaps {
	if getenv("WARDYN_RUNNER_WINDOWS_VM") != "1" {
		return nil
	}
	if linux == nil {
		slog.WarnContext(ctx, "Windows VM sandboxes are off on this runner", "reason",
			"they need Linux VM sandboxes on: set WARDYN_RUNNER_LINUX_VM=1 and pass its start-up check")
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c, err := sub.ProbeWindowsVM(pctx)
	if err == nil {
		err = sub.EnableWindowsVM(&c)
	}
	if err != nil {
		slog.WarnContext(ctx, "Windows VM sandboxes are off on this runner", "reason", err.Error())
		return nil
	}
	slog.InfoContext(ctx, "Windows VM sandboxes are on", "machine_type", linux.MachineType, "cpu_model", linux.CPUModel, "swtpm", c.SWTPMVersion)
	return &runnerwire.VMCaps{MachineType: linux.MachineType, CPUModel: linux.CPUModel, QEMUVersion: linux.QEMUVersion, Desktop: true}
}
