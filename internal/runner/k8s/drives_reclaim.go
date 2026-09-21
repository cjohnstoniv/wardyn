// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ReclaimDrive implements runner.DriveReclaimer (#166): the ONE call in this
// substrate that destroys a member's data, and the only one whose verb the
// chart does not grant by default (`userDrives.reclaim.enabled`).
//
// READ, JUDGE, THEN DELETE — never delete by name. The name cannot answer
// whose object it is: types.DriveObjectName folds two variable-width fields
// with a separator both admit, so drive `eng` + home `us-bob` and drive
// `eng-us` + home `bob` mint one claim name (errDriveClaimForeign), and a
// rename moves a second person's home under a name a first person already
// holds. reuseDriveClaim refuses to MOUNT on exactly that evidence; this
// refuses to DELETE on it, through the identical predicate
// (driveClaimIdentity), because the two failures are the same mistake with
// different blast radii — one shows a member another member's files, this one
// destroys them.
//
// Three refusals and one non-error, in the order they are asked:
//
//  1. NotFound → DriveReclaimAlreadyAbsent. Nothing answers to the name, so
//     nothing was destroyed and nothing is wrong: an operator's own
//     `kubectl delete pvc` or a prior half-finished reclaim reaches the same
//     end state. Never reported as "deleted".
//  2. Not this drive's object → ErrDriveNotReclaimable (driveClaimIdentity).
//  3. Already Terminating → ErrDriveNotReclaimable. A second delete against a
//     finalizer-pinned claim changes nothing; the honest answer is that a
//     reclaim is already in flight, which is also what reuseDriveClaim tells
//     the next run that tries to mount it.
//  4. A pod still references it → ErrDriveInUse. This is the check that has
//     to happen HERE rather than being left to the apiserver, because the
//     apiserver does not refuse: it accepts the delete and parks the claim in
//     Terminating behind the pvc-protection finalizer until the last pod goes.
//     That destroys nothing now AND refuses the member's next run
//     (errDriveClaimTerminating) in the meantime — the worst of both answers.
//     The pods List is the `pods: list` verb the Role already grants for the
//     orphan sweep; no new verb is asked for to make this safe.
func (d *Driver) ReclaimDrive(ctx context.Context, drive types.DriveMount) (runner.DriveReclaimOutcome, error) {
	ns := d.cfg.Namespace
	claims := d.clientset.CoreV1().PersistentVolumeClaims(ns)
	claim, err := claims.Get(ctx, drive.ObjectName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return runner.DriveReclaimAlreadyAbsent, nil
	case apierrors.IsForbidden(err):
		return "", refuseForbiddenDriveClaim("get", ns, &drive, err)
	case err != nil:
		return "", fmt.Errorf("k8s: drive: read claim %q before reclaiming it: %w", drive.ObjectName, err)
	}
	if idErr := driveClaimIdentity(claim, &drive); idErr != nil {
		// driveClaimIdentity has already logged the operator's half (the claim,
		// the label and both values). The sentinel is what the API turns into a
		// 409; its own words never carry the comparison, for the reason
		// errDriveClaimForeign states.
		return "", fmt.Errorf("k8s: drive: refusing to reclaim claim %q: %w", drive.ObjectName, runner.ErrDriveNotReclaimable)
	}
	if claim.DeletionTimestamp != nil {
		return "", fmt.Errorf("k8s: drive: claim %q is already Terminating (deleted at %s): %w",
			drive.ObjectName, claim.DeletionTimestamp.UTC().Format("2006-01-02T15:04:05Z"), runner.ErrDriveNotReclaimable)
	}
	if holder, herr := driveClaimHolder(ctx, d, ns, drive.ObjectName); herr != nil {
		return "", herr
	} else if holder != "" {
		return "", fmt.Errorf("k8s: drive: claim %q is mounted by pod %q: %w", drive.ObjectName, holder, runner.ErrDriveInUse)
	}
	if err := claims.Delete(ctx, drive.ObjectName, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			// Raced by another reclaim between the Get and the Delete. The end
			// state is the one that was asked for, and this call did not cause
			// it — the same distinction the NotFound arm above draws.
			return runner.DriveReclaimAlreadyAbsent, nil
		}
		if apierrors.IsForbidden(err) {
			return "", refuseForbiddenDriveClaim("delete", ns, &drive, err)
		}
		return "", fmt.Errorf("k8s: drive: delete claim %q: %w", drive.ObjectName, err)
	}
	slog.Warn("wardynd: k8s substrate: a drive's volume claim was deleted by an operator reclaim",
		slog.String("claim", drive.ObjectName), slog.String("namespace", ns),
		slog.String("drive_id", drive.DriveID.String()), slog.String("home", drive.HomeName))
	return runner.DriveReclaimDeleted, nil
}

// driveClaimHolder names a pod in ns whose spec references claim, or "" when
// none does. A List error is returned rather than swallowed: "I could not ask
// whether a run holds this" must never read as "no run holds this" on the one
// path that then destroys the object.
func driveClaimHolder(ctx context.Context, d *Driver, ns, claim string) (string, error) {
	pods, err := d.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("k8s: drive: list pods in %q to see whether a run still holds claim %q: %w", ns, claim, err)
	}
	for i := range pods.Items {
		if podMountsClaim(&pods.Items[i], claim) {
			return pods.Items[i].Name, nil
		}
	}
	return "", nil
}

// podMountsClaim reports whether pod carries a Volume backed by claim.
//
// The pod's own VOLUME LIST, not a label selector: a claim is referenced by
// name, an operator's own pod may mount a member's drive, and the
// wardyn.run-id label the sweep selects on is deliberately absent from a drive
// object (lifecycle.go's SweepOrphanedSandboxes). The reference is the fact.
func podMountsClaim(pod *corev1.Pod, claim string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}
