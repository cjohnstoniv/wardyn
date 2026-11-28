// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDriveHolder_K8s_ReadOnly: the holder read names the pod that mounts the claim, answers "" for a claim no pod
// mounts, and touches nothing: the fake's action list holds only a pod list.
func TestDriveHolder_K8s_ReadOnly(t *testing.T) {
	drive := testDriveMount()
	cs, d := reclaimDriver(existingDriveClaim(drive), podMountingClaim("sandbox-1", drive.ObjectName))
	cs.ClearActions()

	got, err := d.DriveHolder(context.Background(), *drive)
	if err != nil || got != "sandbox-1" {
		t.Fatalf("DriveHolder = %q, %v; want sandbox-1", got, err)
	}
	_, d2 := reclaimDriver(existingDriveClaim(drive), podMountingClaim("other", "some-other-claim"))
	if got, err := d2.DriveHolder(context.Background(), *drive); err != nil || got != "" {
		t.Fatalf("DriveHolder with no mounting pod = %q, %v; want empty", got, err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() != "list" || a.GetResource().Resource != "pods" {
			t.Errorf("unexpected action %s %s: the holder read must only list pods", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if len(cs.Actions()) != 1 {
		t.Errorf("actions = %d, want exactly one pod list", len(cs.Actions()))
	}
}

// TestDriveHolder_K8s_WrongBackendUnknown: only a managed claim can be judged.
func TestDriveHolder_K8s_WrongBackendUnknown(t *testing.T) {
	_, d := reclaimDriver()
	for name, m := range map[string]types.DriveMount{
		"docker volume": {Backend: types.DriveBackendDockerVolume, ObjectName: "v"},
		"static claim":  {Backend: types.DriveBackendK8sPVCStatic, ObjectName: "c"},
		"no object":     {Backend: types.DriveBackendK8sPVC},
	} {
		if got, err := d.DriveHolder(context.Background(), m); got != "" || !errors.Is(err, runner.ErrDriveHolderUnknown) {
			t.Errorf("%s: DriveHolder = %q, %v; want ErrDriveHolderUnknown", name, got, err)
		}
	}
}
