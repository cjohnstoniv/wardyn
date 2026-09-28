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
// chart does not grant by default (`drives.reclaim.enabled`).
//
// READ, JUDGE, THEN DELETE — never delete by name: types.DriveObjectName folds
// two variable-width fields with a separator both admit, so drive `eng` +
// home `us-bob` and drive `eng-us` + home `bob` mint one claim name
// (errDriveClaimForeign), and a rename could move a second person's home
// under a name a first person already holds. reuseDriveClaim refuses to mount
// on that evidence; this refuses to delete on it through the same predicate
// (driveClaimIdentity) — the same mistake with a worse blast radius: one shows
// a member another's files, the other destroys them.
//
// Refusals, checked in order:
//
//  1. NotFound → DriveReclaimAlreadyAbsent: nothing answers to the name, so
//     nothing was destroyed (an operator's own `kubectl delete pvc`, or a
//     prior half-finished reclaim, reaches the same end state) — never
//     reported as "deleted".
//  2. Not this drive's object → ErrDriveNotReclaimable (driveClaimIdentity).
//  3. Already Terminating → ErrDriveNotReclaimable: a second delete against a
//     finalizer-pinned claim changes nothing, and a reclaim is already in
//     flight (also what reuseDriveClaim tells the next mount attempt).
//  4. A pod still references it → ErrDriveInUse. Checked here rather than left
//     to the apiserver, which does not refuse: it accepts the delete and
//     parks the claim in Terminating behind the pvc-protection finalizer
//     until the last pod goes, destroying nothing now while still refusing
//     the member's next run (errDriveClaimTerminating) — the worst of both
//     answers. Uses the `pods: list` verb the Role already grants for the
//     orphan sweep; no new verb is needed.
//  5. The claim changed after being judged → ErrDriveNotReclaimable: the
//     delete carries the inspected claim's UID and resourceVersion as
//     preconditions, so a claim deleted and re-created under the same name
//     (or relabelled in place) between Get and Delete answers Conflict
//     instead of being destroyed unjudged. Never retried by name — a fresh
//     reclaim re-reads and re-judges whatever answers to the name now.
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
		// driveClaimIdentity already logged the operator's half; the sentinel
		// error never carries the comparison itself (errDriveClaimForeign).
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
	pre := metav1.Preconditions{UID: &claim.UID, ResourceVersion: &claim.ResourceVersion}
	if err := claims.Delete(ctx, drive.ObjectName, metav1.DeleteOptions{Preconditions: &pre}); err != nil {
		if apierrors.IsConflict(err) {
			// The apiserver's text names both UIDs; kept in the log, never in
			// the 409 body.
			slog.Warn("wardynd: k8s substrate: a drive's volume claim changed between its inspection and its reclaim; nothing was deleted",
				slog.String("claim", drive.ObjectName), slog.String("namespace", ns),
				slog.String("inspected_uid", string(claim.UID)), slog.String("error", err.Error()))
			return "", fmt.Errorf("k8s: drive: claim %q changed after it was inspected, nothing was deleted: %w",
				drive.ObjectName, runner.ErrDriveNotReclaimable)
		}
		if apierrors.IsNotFound(err) {
			// Raced by another reclaim between Get and Delete, with nothing
			// replacing it (a replacement would answer Conflict above) — this
			// call did not cause the end state, so it's not an error.
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
// none does. A List error is returned rather than swallowed: "could not ask"
// must never read as "no run holds this" on the path that then destroys it.
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
// Checked against the pod's own volume list, not a label selector: a claim is
// referenced by name, an operator's own pod may mount a member's drive, and
// the wardyn.run-id label the sweep selects on is deliberately absent from a
// drive object (lifecycle.go's SweepOrphanedSandboxes) — the reference is the
// fact.
func podMountsClaim(pod *corev1.Pod, claim string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}
