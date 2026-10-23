// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// EnableLinuxVM turns Linux VM sandboxes on. A VM here is the run's pod under the RuntimeClass the
// operator pinned for CC3 (WARDYN_CONFINEMENT_MAP), so a microVM runtime such as Kata boots it in a
// guest kernel of its own. Its network, proxy and Secret are a container run's, unchanged: the guest's
// only interface is the pod's, held by the same NetworkPolicies, and the desktop's VNC is reached
// through the same exec relay. Only runnerd calls this, after its operator turned VMs on; it returns
// the machine the runner's caps offer, or why it offers none.
func (d *Driver) EnableLinuxVM(ctx context.Context) (types.VMMachine, error) {
	_, handler, err := d.resolveRuntimeClassName(ctx, types.CC3)
	if err != nil {
		return types.VMMachine{}, fmt.Errorf("k8s: a Linux VM sandbox runs under the CC3 RuntimeClass: %w", err)
	}
	d.linuxVM.Store(true)
	return vmMachine(handler), nil
}

// vmMachine is the machine recorded for a VM pod. The node's runtime picks the virtual hardware and
// the runner cannot see it, so every field names the RuntimeClass handler that did: two runs' machines
// compare equal only when the same handler started both.
func vmMachine(handler string) types.VMMachine {
	id := "runtimeclass:" + handler
	return types.VMMachine{MachineType: id, CPUModel: id, Accelerator: id, QEMUVersion: id}
}

// refuseVMSpec refuses, before anything is created, a VM this runner does not offer and what a VM
// does not honour in this release (the Docker runner's table: no drives, no recording).
func (d *Driver) refuseVMSpec(spec runner.SandboxSpec) error {
	switch {
	case !d.linuxVM.Load():
		return fmt.Errorf("k8s: a %q sandbox runs only on a registered runner that offers it", spec.MachineKind)
	case spec.MachineKind != types.MachineLinuxVM:
		return fmt.Errorf("k8s: unknown machine kind %q", spec.MachineKind)
	case len(spec.Drives) > 0:
		return errors.New("k8s: a Linux VM sandbox has no drives in this release")
	case spec.RecordWithin(d.cfg.Record):
		return errors.New("k8s: a Linux VM sandbox is not recorded in this release; turn recording off for this run")
	}
	return nil
}
