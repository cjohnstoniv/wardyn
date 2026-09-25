// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestProbeDrive_BoundClaimIsUnknown pins the interface contract
// (runner.go: DriveProbeReadable means the probe ran AS THE AGENT'S OWN
// UID): a Bound claim proves the volume is provisioned and attachable, not
// that the agent's own uid can read it once mounted, and this driver has no
// pod-less way to ask that question — so even its best case stays Unknown,
// never a guessed pass.
func TestProbeDrive_BoundClaimIsUnknown(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	drive := testDriveMount()
	if _, err := cs.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: drive.ObjectName, Namespace: testNamespace},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	probe, err := d.ProbeDrive(context.Background(), *drive)
	if err != nil {
		t.Fatalf("ProbeDrive: %v", err)
	}
	if probe.Result != runner.DriveProbeUnknown {
		t.Errorf("result = %q, want %q for a merely Bound claim", probe.Result, runner.DriveProbeUnknown)
	}
}

// TestProbeDrive_MissingClaimIsUnknown is the #165 property for Kubernetes: a
// claim this driver cannot even Get (not found here; Forbidden and a
// transport error take the identical path through the same err != nil arm) is
// NOT a refusal and NOT a pass — it is unknown, because a probe that cannot
// see the storage has proved nothing either way.
func TestProbeDrive_MissingClaimIsUnknown(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	drive := testDriveMount()

	probe, err := d.ProbeDrive(context.Background(), *drive)
	if err != nil {
		t.Fatalf("ProbeDrive: %v", err)
	}
	if probe.Result != runner.DriveProbeUnknown {
		t.Errorf("result = %q, want %q for a claim this driver cannot Get", probe.Result, runner.DriveProbeUnknown)
	}
}

// TestProbeDrive_PendingClaimIsUnknown: a claim that EXISTS but has not Bound
// yet answers unknown too — Pending is a fact about provisioning, not about
// whether the agent uid will be able to read it, so it is no more a proof of
// readability than of unreadability.
func TestProbeDrive_PendingClaimIsUnknown(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	drive := testDriveMount()
	if _, err := cs.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: drive.ObjectName, Namespace: testNamespace},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	probe, err := d.ProbeDrive(context.Background(), *drive)
	if err != nil {
		t.Fatalf("ProbeDrive: %v", err)
	}
	if probe.Result != runner.DriveProbeUnknown {
		t.Errorf("result = %q, want %q for a Pending claim", probe.Result, runner.DriveProbeUnknown)
	}
}
