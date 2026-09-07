// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

// PIN for the half of the static-PVC finding that lives in this driver: a
// pre-provisioned claim carried no per-principal evidence at all, so one claim
// name served every member the admin pointed at it.
//
// The claim's NAME is minted by Wardyn (types.DriveObjectNamedByWardyn), so a
// home template that folds two principals onto one home folds them onto one
// claim — and the static arm of driveClaimIdentity, which only asked whether the
// claim was a managed one, bound it for both. types.ValidateUserDrive now
// refuses `email_local` on this backend, which closes the authoring route; this
// arm closes the row that predates the rule, the row written by hand, and the
// admin who points two members at one claim.
//
// PRESENCE-GUARDED, exactly as the managed arm's own subject check is: an
// admin's claim carries none of Wardyn's labels and stays bindable unchanged.
// An admin who stamps wardyn.subject=<DriveSubjectHash(subject)> on the claims
// they provision gets per-principal binding enforced by the driver.

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestEnsureDrivePVC_StaticShareRefusesAnotherPrincipalsLabelledClaim(t *testing.T) {
	staticMount := func() *types.DriveMount {
		d := testDriveMount()
		d.Backend = types.DriveBackendK8sPVCStatic
		d.Enforcement = types.StorageEnforcementExternal
		return d
	}
	adminClaim := func(labels map[string]string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: staticMount().ObjectName, Namespace: testNamespace, Labels: labels,
		}}
	}

	t.Run("a claim stamped for another principal is foreign", func(t *testing.T) {
		drive := staticMount()
		// The digest of somebody else — the shape an admin gets by stamping the
		// claims they pre-provision, and the shape a folded `email_local` row
		// written before the write-boundary rule leaves behind.
		const otherSubject = "aaaabbbbccccddddeeee"
		if otherSubject == drive.SubjectHash {
			t.Fatal("the probe's two subject digests are equal; it cannot tell two principals apart")
		}
		claim := adminClaim(map[string]string{
			"app.kubernetes.io/managed-by": "corp-storage",
			labelDriveSubject:              otherSubject,
		})
		err := ensureDrivePVC(context.Background(), fake.NewClientset(claim), testNamespace, drive)
		if !errors.Is(err, errDriveClaimForeign) {
			t.Fatalf("ensureDrivePVC over a static claim stamped %s=%q while this run's subject hashes to %q: "+
				"err = %v, want errDriveClaimForeign. Wardyn MINTS a static claim's name, so one name can cover two "+
				"principals — and this arm is the only place that can tell",
				labelDriveSubject, otherSubject, drive.SubjectHash, err)
		}
	})

	t.Run("the member's own stamped claim still mounts", func(t *testing.T) {
		drive := staticMount()
		claim := adminClaim(map[string]string{labelDriveSubject: drive.SubjectHash})
		if err := ensureDrivePVC(context.Background(), fake.NewClientset(claim), testNamespace, drive); err != nil {
			t.Errorf("ensureDrivePVC over the member's OWN stamped share: %v, want it mounted", err)
		}
	})

	t.Run("an unstamped administrator claim is unaffected", func(t *testing.T) {
		// The presence guard, and the reason this hardening is opt-in: an
		// admin's pre-provisioned claim carries none of Wardyn's labels, and a
		// share must still mount over it.
		drive := staticMount()
		claim := adminClaim(map[string]string{"app.kubernetes.io/managed-by": "corp-storage", "team": "eng"})
		if err := ensureDrivePVC(context.Background(), fake.NewClientset(claim), testNamespace, drive); err != nil {
			t.Errorf("ensureDrivePVC over an administrator's unlabelled share: %v, want it mounted unchanged", err)
		}
	})
}
