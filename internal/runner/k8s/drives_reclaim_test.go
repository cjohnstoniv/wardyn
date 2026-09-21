// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// reclaimDriver is the driver over a fake apiserver holding seed.
func reclaimDriver(seed ...runtime.Object) (*fake.Clientset, *Driver) {
	cs := fake.NewClientset(seed...)
	return cs, &Driver{clientset: cs, cfg: Config{Namespace: testNamespace}}
}

// podMountingClaim is a pod whose spec references claim by name — the evidence
// driveClaimHolder looks for, and deliberately NOT a label, because an
// operator's own pod may mount a member's drive and a drive object carries no
// wardyn.run-id at all.
func podMountingClaim(name, claim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "drive",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		}}},
	}
}

// claimExists reports whether the fake still holds the drive's claim.
func claimExists(t *testing.T, cs *fake.Clientset, name string) bool {
	t.Helper()
	for _, c := range createdPVCs(t, cs) {
		if c.Name == name {
			return true
		}
	}
	return false
}

// TestReclaimDrive_DeletesTheDrivesOwnClaim is the happy path.
func TestReclaimDrive_DeletesTheDrivesOwnClaim(t *testing.T) {
	drive := testDriveMount()
	cs, d := reclaimDriver(existingDriveClaim(drive))

	got, err := d.ReclaimDrive(context.Background(), *drive)
	if err != nil {
		t.Fatalf("ReclaimDrive: %v", err)
	}
	if got != runner.DriveReclaimDeleted {
		t.Errorf("outcome = %q, want %q", got, runner.DriveReclaimDeleted)
	}
	if claimExists(t, cs, drive.ObjectName) {
		t.Error("the claim survived a reported delete")
	}
}

// TestReclaimDrive_AlreadyAbsentIsNotAnError: an operator's own
// `kubectl delete pvc`, or a prior half-finished reclaim, reaches the same end
// state — and it is never reported as `deleted`.
func TestReclaimDrive_AlreadyAbsentIsNotAnError(t *testing.T) {
	drive := testDriveMount()
	_, d := reclaimDriver()

	got, err := d.ReclaimDrive(context.Background(), *drive)
	if err != nil {
		t.Fatalf("ReclaimDrive against a missing claim: %v", err)
	}
	if got != runner.DriveReclaimAlreadyAbsent {
		t.Errorf("outcome = %q, want %q", got, runner.DriveReclaimAlreadyAbsent)
	}
}

// TestReclaimDrive_RefusesAClaimThatIsNotThisDrivesObject is the identity rail.
// A claim NAME cannot answer whose object it is — DriveObjectName folds two
// variable-width fields with a separator both admit — so the labels decide,
// through the same predicate that refuses to MOUNT a foreign claim.
func TestReclaimDrive_RefusesAClaimThatIsNotThisDrivesObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*corev1.PersistentVolumeClaim)
	}{
		{
			"another drive's claim under a colliding minted name",
			func(c *corev1.PersistentVolumeClaim) { c.Labels[labelDrive] = "99999999-8888-7777-6666-555555555555" },
		},
		{
			"another person's home under a name a rename moved",
			func(c *corev1.PersistentVolumeClaim) { c.Labels[labelDriveHome] = "d-somebodyelse" },
		},
		{
			"another principal's claim under a home that folds two people onto one",
			func(c *corev1.PersistentVolumeClaim) { c.Labels[labelDriveSubject] = "ffffffffffffffffffff" },
		},
		{
			"an administrator's own pre-existing claim, carrying none of Wardyn's labels",
			func(c *corev1.PersistentVolumeClaim) { c.Labels = map[string]string{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drive := testDriveMount()
			claim := existingDriveClaim(drive)
			tc.mut(claim)
			cs, d := reclaimDriver(claim)

			_, err := d.ReclaimDrive(context.Background(), *drive)
			if !errors.Is(err, runner.ErrDriveNotReclaimable) {
				t.Fatalf("err = %v, want ErrDriveNotReclaimable", err)
			}
			if !claimExists(t, cs, drive.ObjectName) {
				t.Error("the foreign claim was deleted anyway — a wrong reclaim destroys another member's storage")
			}
		})
	}
}

// TestReclaimDrive_RefusesAClaimAlreadyTerminating: a second delete against a
// finalizer-pinned claim changes nothing, and the honest answer is that a
// reclaim is already in flight — the same thing reuseDriveClaim tells the next
// run that tries to mount it.
func TestReclaimDrive_RefusesAClaimAlreadyTerminating(t *testing.T) {
	drive := testDriveMount()
	claim := existingDriveClaim(drive)
	now := metav1.Now()
	claim.DeletionTimestamp = &now
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	_, d := reclaimDriver(claim)

	if _, err := d.ReclaimDrive(context.Background(), *drive); !errors.Is(err, runner.ErrDriveNotReclaimable) {
		t.Fatalf("err = %v, want ErrDriveNotReclaimable", err)
	}
}

// TestReclaimDrive_RefusesWhileAPodStillReferencesTheClaim is the check that
// has to happen HERE rather than being left to the apiserver, because the
// apiserver does not refuse: it accepts the delete and parks the claim in
// Terminating behind the pvc-protection finalizer until the last pod goes.
// That destroys nothing now AND refuses the member's next run in the meantime —
// the worst of both answers.
func TestReclaimDrive_RefusesWhileAPodStillReferencesTheClaim(t *testing.T) {
	drive := testDriveMount()
	cs, d := reclaimDriver(existingDriveClaim(drive), podMountingClaim("wardyn-run-agent", drive.ObjectName))

	_, err := d.ReclaimDrive(context.Background(), *drive)
	if !errors.Is(err, runner.ErrDriveInUse) {
		t.Fatalf("err = %v, want ErrDriveInUse", err)
	}
	if !claimExists(t, cs, drive.ObjectName) {
		t.Error("the claim was deleted while a pod still mounted it — the apiserver would have accepted that " +
			"delete and left it Terminating, refusing the member's next run without freeing anything")
	}
}

// …and the CONTROL for the case above: a pod that mounts a DIFFERENT claim is
// not a holder. Without this, "refuse while any pod exists" would pass the
// assertion above and break every reclaim on a busy namespace.
func TestReclaimDrive_APodMountingAnotherClaimIsNotAHolder(t *testing.T) {
	drive := testDriveMount()
	cs, d := reclaimDriver(existingDriveClaim(drive), podMountingClaim("someone-else", "wardyn-drive-research-d-other"))

	got, err := d.ReclaimDrive(context.Background(), *drive)
	if err != nil {
		t.Fatalf("ReclaimDrive: %v", err)
	}
	if got != runner.DriveReclaimDeleted {
		t.Errorf("outcome = %q, want %q", got, runner.DriveReclaimDeleted)
	}
	if claimExists(t, cs, drive.ObjectName) {
		t.Error("the claim survived a reported delete")
	}
}
