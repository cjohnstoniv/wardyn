// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func secondK8sDrive() types.DriveMount {
	d := *testDriveMount()
	d.DriveID, d.ObjectName, d.Target, d.ReadOnly = uuid.New(), "wardyn-drive-team-d-9f3a1c", runner.DrivesRoot+"/team", true
	return d
}

// TestCreateSandbox_MountsEveryDrive: two managed drives become two claims and
// two volumes on the agent pod, each at its own path and posture; the first
// keeps the one-drive volume name.
func TestCreateSandbox_MountsEveryDrive(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	spec.Drives = []types.DriveMount{*testDriveMount(), secondK8sDrive()}
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if n := len(createdPVCs(t, cs)); n != 2 {
		t.Errorf("created %d claims, want one per drive (2)", n)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range spec.Drives {
		v := volumeFor(pod.Spec.Volumes, driveVolumeNameAt(i))
		if v == nil || v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != want.ObjectName || v.PersistentVolumeClaim.ReadOnly != want.ReadOnly {
			t.Errorf("volume %s = %+v, want claim %s ro=%v", driveVolumeNameAt(i), v, want.ObjectName, want.ReadOnly)
		}
		found := false
		for _, c := range pod.Spec.Containers {
			for _, m := range c.VolumeMounts {
				found = found || m.Name == driveVolumeNameAt(i) && m.MountPath == want.Target && m.ReadOnly == want.ReadOnly
			}
		}
		if !found {
			t.Errorf("no container mounts %s at %s", driveVolumeNameAt(i), want.Target)
		}
	}
	if driveVolumeNameAt(0) != driveVolumeName {
		t.Errorf("the first drive's volume is %q, want %q", driveVolumeNameAt(0), driveVolumeName)
	}
}

// TestCreateSandbox_DriveSetRefusals: two drives at one path, a drive off its
// derived path, and a share beside a managed drive (whose fsGroup would re-own
// the share) are refused before anything is created.
func TestCreateSandbox_DriveSetRefusals(t *testing.T) {
	twin := secondK8sDrive()
	twin.Target = runner.DriveTarget
	share := secondK8sDrive()
	share.Backend, share.Target = types.DriveBackendK8sPVCStatic, runner.SharedRoot+"/team"
	offPath := secondK8sDrive()
	offPath.Target = runner.SharedRoot + "/team"
	for name, c := range map[string]struct {
		drives []types.DriveMount
		want   error
	}{
		"two at one target":    {[]types.DriveMount{*testDriveMount(), twin}, errDriveTargetInvalid},
		"managed under shared": {[]types.DriveMount{offPath}, errDriveTargetInvalid},
		"share beside managed": {[]types.DriveMount{*testDriveMount(), share}, errDriveShareBesideManaged},
	} {
		t.Run(name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			spec := testSandboxSpec()
			spec.Drives = c.drives
			if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, c.want) {
				t.Fatalf("CreateSandbox = %v, want %v", err, c.want)
			}
			if n := len(createdPVCs(t, cs)); n != 0 {
				t.Errorf("a refused drive set created %d claims", n)
			}
		})
	}
}
