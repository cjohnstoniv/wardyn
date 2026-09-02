// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The user drive is the FIRST and ONLY Volume/VolumeMount this substrate
// creates. It is not a host bind and it is not a spec.Mounts entry — it arrives
// on its own field (SandboxSpec.Drive), which is exactly why CreateSandbox's
// blanket refusal of spec.Mounts (errMountsUnsupported) needs no drive
// exemption: the two can never be confused for one another.
//
// PSS Restricted admits `persistentVolumeClaim` as a volume type; it forbids
// `hostPath` under Baseline and Restricted alike. No drive backend on this
// substrate offers a host path, so nothing here has to refuse one.
const (
	// driveVolumeName ties the agent pod's Volume to the VolumeMount on its main
	// container and on the ephemeral exec container Exec adds. Static, with no
	// per-run entropy: one principal has at most one drive, so one run mounts at
	// most one claim.
	driveVolumeName = "drive"

	// labelDrive marks a claim as belonging to one drive ROW (by id, so a rename
	// does not orphan the label the way the object name's slug would), and
	// labelDriveHome to one person's home within it. Byte-for-byte the pair the
	// docker driver stamps on a managed volume, so an operator's selector reads
	// the same on either substrate — the same argument naming.go's label
	// vocabulary makes for the run/component/managed keys, and the reason
	// neither key takes a "/" prefix.
	//
	// There is deliberately NO wardyn.run-id label here. A drive outlives every
	// run that mounts it, and teardownByRunID sweeps by exactly that selector:
	// one run label on this object would make the first run to finish delete the
	// person's storage.
	labelDrive     = "wardyn.drive"
	labelDriveHome = "wardyn.home"

	// driveFSGroup is the agent uid/gid every wardyn agent image runs as
	// (agentSecurityContext pins RunAsUser to the same 1000). Set as the pod's
	// FSGroup so the kubelet group-owns a freshly provisioned volume's root for
	// the agent — otherwise a block volume comes up root-owned and the agent
	// cannot write to its own drive.
	driveFSGroup int64 = 1000
)

// ensureDrivePVC makes the run's PersistentVolumeClaim exist, and is the only
// place this substrate provisions storage.
//
//   - k8s_pvc (managed): Get by name, Create on NotFound. Idempotent by name, so
//     a member's second run reuses the first run's claim instead of racing
//     another one into the namespace.
//   - k8s_pvc_static (share): Get only. An admin provisioned that claim; a
//     missing one is a refusal, never a silently-created empty volume standing
//     in for the corporate home a member expected to find.
//
// The RBAC this needs is therefore exactly `persistentvolumeclaims: [get,
// create]` — no list, no watch, and above all no delete: reclaim is a documented
// operator command (deploy/helm/wardyn/README.md, docs/OPERATIONS.md), not a
// verb wardynd holds.
//
// It takes the clientset and namespace rather than hanging off *Driver because
// it reads no driver state, which also lets its tests call it without standing
// up the boot-time egress canary.
func ensureDrivePVC(ctx context.Context, client kubernetes.Interface, ns string, drive *types.DriveMount) error {
	switch drive.Backend {
	case types.DriveBackendK8sPVC, types.DriveBackendK8sPVCStatic:
	default:
		// A Docker-backed drive reaching this driver is a control-plane bug (the
		// run-create seam already refuses a backend whose RunnerTarget is not
		// this deployment's — see driveMountFor). Refusing loudly beats mounting
		// nothing: a run that quietly came up drive-less is how a member's work
		// disappears into a container that is thrown away.
		return fmt.Errorf("k8s: drive %q is a %q drive, which this substrate cannot mount: %w",
			drive.ObjectName, drive.Backend, errDriveBackendUnsupported)
	}

	_, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, drive.ObjectName, metav1.GetOptions{})
	switch {
	case err == nil:
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("k8s: drive: look up claim %q: %w", drive.ObjectName, err)
	case drive.Backend == types.DriveBackendK8sPVCStatic:
		return fmt.Errorf("k8s: drive: claim %q is absent from namespace %q: %w", drive.ObjectName, ns, errDriveClaimNotProvisioned)
	}

	// HomeName is used raw, and that is a guarantee rather than a hope: the API
	// refuses a home that is not a DNS-1123 subdomain of at most 63 characters
	// before a run ever reaches a driver, which makes ObjectName a legal object
	// name and HomeName a legal label value by construction. Nothing here
	// re-derives or re-shapes either.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      drive.ObjectName,
			Namespace: ns,
			Labels: map[string]string{
				labelManaged:   "true",
				labelDrive:     drive.DriveID.String(),
				labelDriveHome: drive.HomeName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				// The allocation, in bytes with a binary format, which is
				// <SizeMiB>Mi in canonical form (the apiserver re-canonicalizes
				// any spelling of it anyway — 10240Mi comes back as 10Gi). It is
				// a REQUEST: whether it binds is the storage class's answer, not
				// Wardyn's. See StorageEnforcement, and the honesty sentence.
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(int64(drive.SizeMiB)*1024*1024, resource.BinarySI)},
			},
		},
	}
	if drive.StorageClass != "" {
		pvc.Spec.StorageClassName = &drive.StorageClass
	}

	if _, cerr := client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); cerr != nil {
		switch {
		case apierrors.IsAlreadyExists(cerr):
			// Lost the Get→Create race with this same person's other run. The
			// claim exists, which is all this function promises.
			return nil
		case apierrors.IsForbidden(cerr):
			return fmt.Errorf("k8s: drive: creating claim %q in namespace %q was refused by the apiserver (%v): %w",
				drive.ObjectName, ns, cerr, errDrivePVCForbidden)
		default:
			return fmt.Errorf("k8s: drive: create claim %q: %w", drive.ObjectName, cerr)
		}
	}
	return nil
}

// driveVolume is the agent pod's claim reference. ReadOnly is set on the volume
// AND on every VolumeMount: the volume-level flag is what the kubelet passes to
// the mount, and the mount-level flag is what a reader of the pod spec (and an
// admission policy) sees — disagreeing halves are how a read-only allocation
// comes up writable.
func driveVolume(drive *types.DriveMount) corev1.Volume {
	return corev1.Volume{
		Name: driveVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: drive.ObjectName,
				ReadOnly:  drive.ReadOnly,
			},
		},
	}
}

// driveVolumeMount mounts the drive at the RESERVED target the control plane
// carried on the mount (runner.DriveTarget) — never a literal re-typed here, so
// the path a policy or workspace source is refused for naming and the path this
// binds at are the same string by construction.
func driveVolumeMount(drive *types.DriveMount) corev1.VolumeMount {
	return corev1.VolumeMount{Name: driveVolumeName, MountPath: drive.Target, ReadOnly: drive.ReadOnly}
}

// drivePodSecurityContext is the ONE pod-level security context this substrate
// sets, and only on a pod that actually has a drive (a drive-less pod keeps the
// nil it has always had — every other hardening decision here is container-level
// on purpose, see baseSecurityContext).
//
// FSGroupChangePolicy OnRootMismatch, not Always: a recursive chown of a large
// existing drive on every single run is how a pod start goes from seconds to
// minutes, and the root's own ownership already answers the question.
//
// The kubelet applies fsGroup for CSI drivers that declare
// ReadWriteOnceWithFSType volume ownership — i.e. block storage, the managed
// k8s_pvc case. It does NOT apply it to an NFS-type volume: a k8s_pvc_static
// share is owned by whatever its export says, and the recipe for that is the
// export's own uid/gid mapping (docs/OPERATIONS.md, "User drives on
// Kubernetes"), not this field.
func drivePodSecurityContext() *corev1.PodSecurityContext {
	policy := corev1.FSGroupChangeOnRootMismatch
	return &corev1.PodSecurityContext{FSGroup: int64Ptr(driveFSGroup), FSGroupChangePolicy: &policy}
}
