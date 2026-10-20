// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"cmp"
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// defaultVMCPUModel is the named CPU every VM gets unless the operator names another: the x86-64-v3
// class (Intel Haswell and later). QEMU starts it with enforce, so a host that lacks a feature
// refuses instead of giving the guest a different CPU.
const defaultVMCPUModel = "Haswell-v4"

// enableLinuxVM turns Linux VM sandboxes on when the operator asked for them and the host passes
// the probe, and returns what the runner's caps offer. Off by default; a failed probe logs its fix
// and offers none, and container runs are untouched either way.
func enableLinuxVM(ctx context.Context, sub *docker.Driver, stateDir string, getenv func(string) string) *runnerwire.VMCaps {
	if getenv("WARDYN_RUNNER_LINUX_VM") != "1" {
		return nil
	}
	c := docker.VMConfig{
		StateDir:  filepath.Join(stateDir, "vm"),
		QEMUImage: getenv("WARDYN_RUNNER_VM_QEMU_IMAGE"),
		CPUModel:  cmp.Or(getenv("WARDYN_RUNNER_VM_CPU_MODEL"), defaultVMCPUModel),
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute) // a first pull of the QEMU image included
	defer cancel()
	c, err := sub.ProbeVM(pctx, c)
	if err == nil {
		err = sub.EnableVM(&c)
	}
	if err != nil {
		slog.WarnContext(ctx, "Linux VM sandboxes are off on this runner", "reason", err.Error())
		return nil
	}
	slog.InfoContext(ctx, "Linux VM sandboxes are on", "machine_type", c.MachineType, "cpu_model", c.CPUModel, "qemu", c.QEMUVersion)
	// Desktop: the driver opens every VM's display (docker.Driver.DialDesktop).
	return &runnerwire.VMCaps{MachineType: c.MachineType, CPUModel: c.CPUModel, QEMUVersion: c.QEMUVersion, Desktop: true}
}
