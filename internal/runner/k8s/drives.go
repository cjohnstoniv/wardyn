// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The user drive is the FIRST and ONLY Volume/VolumeMount this substrate creates, arriving on its
// own field (SandboxSpec.Drive) — why CreateSandbox's blanket refusal of spec.Mounts needs no exemption for it.
const (
	// driveVolumeName ties the agent pod's Volume to the VolumeMount on its main container and the
	// ephemeral exec container Exec adds. Static: one principal has at most one drive.
	driveVolumeName = "drive"

	// driveDirectfsAnnotation is the key applyDriveToPod stamps on runsc drive pods. It is inert today:
	// runsc only honors a mount hint carrying `share`, `source` AND `type` together (this key sets
	// none); the working remedy is the NODE flag --directfs=false (docs/OPERATIONS.md) — kept
	// stamped as the right key if the hint is ever completed.
	driveDirectfsAnnotation = "dev.gvisor.spec.mount." + driveVolumeName + ".directfs"

	// labelDrive marks a claim as belonging to one drive ROW, labelDriveHome to one person's home
	// within it — the same pair the docker driver stamps, so an operator's selector reads the same
	// on either substrate.
	//
	// SECURITY: this pair is the ONLY evidence that a claim under a member's object name is that
	// member's storage; driveClaimIdentity refuses the run when either disagrees (the object name
	// alone can't answer that — see errDriveClaimForeign). Deliberately NO wardyn.run-id label: a
	// drive outlives every run that mounts it, and teardownByRunID sweeps by that selector, so a run
	// label here would let the first run to finish delete the person's storage.
	labelDrive     = "wardyn.drive"
	labelDriveHome = "wardyn.home"
	// labelDriveSubject is the resolver's fingerprint of the PERSON the home was derived from — a
	// digest, telling two principals apart when a non-injective home template folds them onto one
	// home. Absent on pre-existing claims, so its compare is presence-guarded.
	labelDriveSubject = "wardyn.subject"

	// driveFSGroup group-owns a freshly provisioned volume's root to gid 1000 (every agent image's
	// uid), MANAGED claim only — applyDriveToPod explains why a SHARE must never carry it.
	driveFSGroup int64 = 1000
)

// validateDriveMount is the driver's OWN contract over the mount it is handed, re-derived from
// scratch rather than trusted from the control plane: names cross a process boundary, so trusting
// the input leaves no failure mode but a mid-dispatch apiserver 422. Each rule matches one API
// object's own alphabet: ObjectName is the PVC NAME (DNS-1123, narrower than a Docker volume name);
// HomeName is a LABEL VALUE, narrower still; SizeMiB (managed backend only) must bind a non-zero
// request; Target is checked even though nobody authors it, because a wrong value would mount a
// member's storage ON TOP of something else rather than merely fail (errDriveTargetInvalid).
func validateDriveMount(drive *types.DriveMount) error {
	if msgs := validation.IsDNS1123Subdomain(drive.ObjectName); len(msgs) > 0 {
		return fmt.Errorf("k8s: drive: %q cannot name a volume claim (%s): %w",
			drive.ObjectName, strings.Join(msgs, "; "), errDriveNameInvalid)
	}
	if msgs := validation.IsValidLabelValue(drive.HomeName); len(msgs) > 0 {
		return fmt.Errorf("k8s: drive: home name %q cannot be a %s label value (%s): %w",
			drive.HomeName, labelDriveHome, strings.Join(msgs, "; "), errDriveNameInvalid)
	}
	if drive.Backend == types.DriveBackendK8sPVC && drive.SizeMiB <= 0 {
		return fmt.Errorf("k8s: drive: a managed drive's allocation is its volume request and %d MiB cannot be requested: %w",
			drive.SizeMiB, errDriveAllocationInvalid)
	}
	if drive.Target != runner.DriveTarget {
		return fmt.Errorf("k8s: drive: %q is not the reserved drive target %q: %w",
			drive.Target, runner.DriveTarget, errDriveTargetInvalid)
	}
	return nil
}

// driveClaimDrift lists every way an EXISTING claim disagrees with the drive the resolver says it
// belongs to (empty = matches). Everything here is a SHAPE disagreement, warn-only: the claim is
// the member's data and a PVC's request can't shrink anyway. Scoped to the MANAGED backend: a
// static share's claim is an admin's own object, so its class/size are facts about their storage,
// not drift. IDENTITY is deliberately NOT here — "whose claim is this?" has no warn-and-continue
// answer; see reuseDriveClaim, which refuses on it first. Returns descriptions instead of logging
// them, keeping the comparison testable without a log handler.
func driveClaimDrift(claim *corev1.PersistentVolumeClaim, drive *types.DriveMount) []string {
	if drive.Backend != types.DriveBackendK8sPVC {
		return nil
	}
	var drift []string
	// Only when the drive names a class: "" means the cluster default, and a bound claim reports
	// whatever class that resolved TO — comparing "" against it would flag drift on every correctly-provisioned claim.
	if drive.StorageClass != "" {
		got := ""
		if claim.Spec.StorageClassName != nil {
			got = *claim.Spec.StorageClassName
		}
		if got != drive.StorageClass {
			drift = append(drift, fmt.Sprintf("storage class is %q, the drive asks for %q", got, drive.StorageClass))
		}
	}
	wantBytes := driveRequestBytes(drive)
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.Value() != wantBytes {
		drift = append(drift, fmt.Sprintf("request is %s, the drive's allocation is %d MiB", got.String(), drive.SizeMiB))
	}
	if !slices.Contains(claim.Spec.AccessModes, corev1.ReadWriteOnce) {
		drift = append(drift, fmt.Sprintf("access modes are %v, a managed drive is provisioned ReadWriteOnce", claim.Spec.AccessModes))
	}
	return drift
}

// driveClaimIdentity answers the one question with no warn-and-continue answer: is this existing
// object the storage this member's drive names, or somebody else's? The claim's labels are the only
// evidence (the NAME cannot supply it — see errDriveClaimForeign); both wardyn.drive and
// wardyn.home are checked since either alone is satisfiable by the wrong object. A LABEL-LESS claim
// is foreign too: every claim this driver creates carries all three labels, so an unlabelled object
// is an admin's (or another tool's), not an older Wardyn claim to adopt. The share arm mirrors
// this: a k8s_pvc_static claim is an admin's pre-provisioned handle carrying none of Wardyn's
// labels; one carrying wardyn.managed=true is a MANAGED claim provisioned for ONE person, under a
// name an admin has now pointed a whole share at — mounting it would hand every member of that
// share one member's private drive.
func driveClaimIdentity(claim *corev1.PersistentVolumeClaim, drive *types.DriveMount) error {
	switch drive.Backend {
	case types.DriveBackendK8sPVC:
		if got := claim.Labels[labelDrive]; got != drive.DriveID.String() {
			return refuseForeignDriveClaim(claim, drive, labelDrive, got, drive.DriveID.String())
		}
		if got := claim.Labels[labelDriveHome]; got != drive.HomeName {
			return refuseForeignDriveClaim(claim, drive, labelDriveHome, got, drive.HomeName)
		}
		// Presence-guarded: a claim stamped before this label existed would otherwise turn foreign on upgrade and orphan every member's drive.
		if got := claim.Labels[labelDriveSubject]; got != "" && got != drive.SubjectHash {
			return refuseForeignDriveClaim(claim, drive, labelDriveSubject, got, drive.SubjectHash)
		}
	case types.DriveBackendK8sPVCStatic:
		if claim.Labels[labelManaged] == "true" {
			return refuseForeignDriveClaim(claim, drive, labelManaged, "true",
				"an administrator's share, which carries none of Wardyn's labels")
		}
		// Same subject check, presence-guarded for the same reason; a digest, never the subject
		// itself, safe for a share owner to publish. Opt-in: an unstamped pre-provisioned claim
		// stays bindable as before.
		if got := claim.Labels[labelDriveSubject]; got != "" && got != drive.SubjectHash {
			return refuseForeignDriveClaim(claim, drive, labelDriveSubject, got, drive.SubjectHash)
		}
	}
	return nil
}

// refuseForeignDriveClaim is the ONE place a claim-identity mismatch becomes an error, splitting the
// audience: the OPERATOR gets the claim, namespace, label and both values; the MEMBER gets only the
// frozen sentence (a CreateSandbox error is the run's failure hint verbatim) — wardyn.subject is a
// DIGEST OF A PERSON, and handing it (or the other party's home name) to a failed run's screen is exactly what must not happen.
func refuseForeignDriveClaim(claim *corev1.PersistentVolumeClaim, drive *types.DriveMount, label, got, want string) error {
	slog.Warn("wardynd: k8s substrate: a drive claim is not this run's",
		slog.String("claim", claim.Name), slog.String("namespace", claim.Namespace),
		slog.String("label", label), slog.String("claim_value", got), slog.String("run_value", want),
		slog.String("drive", drive.DriveName), slog.String("backend", string(drive.Backend)))
	return errDriveClaimForeign
}

// driveRequestBytes is the allocation in bytes: <SizeMiB>Mi, what the claim asks for and what a
// drifted claim is compared against — one expression, so create and comparison can never disagree.
func driveRequestBytes(drive *types.DriveMount) int64 { return int64(drive.SizeMiB) * 1024 * 1024 }

// ensureDrivePVC makes the run's PersistentVolumeClaim exist, the only place this substrate
// provisions storage: k8s_pvc (managed) does Get-then-Create-on-NotFound, idempotent by name so a
// second run reuses the first's claim rather than racing one into the namespace (AlreadyExists is a
// LOST race, not a success — see reuseRaceWinnerClaim); k8s_pvc_static (share) is Get-only, since a
// missing admin-provisioned claim must be a refusal, never a silently-created empty volume. RBAC
// needed is exactly `persistentvolumeclaims: [get, create]` — reclaim is an operator command, not a
// verb wardynd holds. Takes the clientset/namespace rather than *Driver so tests can call it without the boot-time egress canary.
func ensureDrivePVC(ctx context.Context, client kubernetes.Interface, ns string, drive *types.DriveMount) error {
	switch drive.Backend {
	case types.DriveBackendK8sPVC, types.DriveBackendK8sPVCStatic:
	default:
		// A Docker-backed drive reaching this driver is a control-plane bug — refuse loudly rather than mount nothing.
		return fmt.Errorf("k8s: drive %q is a %q drive, which this substrate cannot mount: %w",
			drive.ObjectName, drive.Backend, errDriveBackendUnsupported)
	}
	if err := validateDriveMount(drive); err != nil { // an illegal name is this driver's refusal to make, not the apiserver's
		return err
	}

	existing, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, drive.ObjectName, metav1.GetOptions{})
	switch {
	case err == nil:
		return reuseDriveClaim(existing, drive)
	case apierrors.IsForbidden(err):
		// The DEFAULT deployment's failure: drives.enabled is off out of the box, so the Role
		// carries no persistentvolumeclaims rule.
		return refuseForbiddenDriveClaim("get", ns, drive, err)
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("k8s: drive: look up claim %q: %w", drive.ObjectName, err)
	case drive.Backend == types.DriveBackendK8sPVCStatic:
		slog.Warn("wardynd: k8s substrate: a share drive's claim is not provisioned", // claim/namespace go to the operator, not the member's failure hint
			slog.String("claim", drive.ObjectName), slog.String("namespace", ns),
			slog.String("drive", drive.DriveName), slog.String("home", drive.HomeName))
		return errDriveClaimNotProvisioned
	}

	// ObjectName and HomeName are used raw, and validateDriveMount above is what makes that safe.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      drive.ObjectName,
			Namespace: ns,
			Labels: map[string]string{
				labelManaged:      "true",
				labelDrive:        drive.DriveID.String(),
				labelDriveHome:    drive.HomeName,
				labelDriveSubject: drive.SubjectHash,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				// A REQUEST only: whether it binds is the storage class's answer, not Wardyn's.
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(driveRequestBytes(drive), resource.BinarySI)},
			},
		},
	}
	if drive.StorageClass != "" {
		pvc.Spec.StorageClassName = &drive.StorageClass
	}

	if _, cerr := client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); cerr != nil {
		switch {
		case apierrors.IsAlreadyExists(cerr):
			return reuseRaceWinnerClaim(ctx, client, ns, drive)
		case apierrors.IsForbidden(cerr):
			return refuseForbiddenDriveClaim("create", ns, drive, cerr)
		default:
			return fmt.Errorf("k8s: drive: create claim %q: %w", drive.ObjectName, cerr)
		}
	}
	return nil
}

// reuseRaceWinnerClaim decides whether the claim that WON a Get→Create race may back this run — the
// one place "the object exists" is deliberately NOT good enough.
//
// SECURITY: two DIFFERENT first runs can land here over the SAME object name, since the name folds
// two variable-width fields with a shared separator (drive "eng"+home "us-bob" and drive
// "eng-us"+home "bob" both resolve to wardyn-drive-eng-us-bob — see errDriveClaimForeign). A loser
// that treated AlreadyExists as success would mount the winner's storage inside its own member's
// agent, the exact cross-mount reuseDriveClaim exists to refuse; the same window also swallows a
// Terminating claim. So the loser re-reads what it lost to and hands it to reuseDriveClaim as though
// its own Get had found it; it does NOT retry the Create (this driver holds only `get`/`create`).
func reuseRaceWinnerClaim(ctx context.Context, client kubernetes.Interface, ns string, drive *types.DriveMount) error {
	winner, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, drive.ObjectName, metav1.GetOptions{})
	switch {
	case err == nil:
		return reuseDriveClaim(winner, drive)
	case apierrors.IsNotFound(err):
		// Present for the Create, gone for the read: a reclaim landing mid-dispatch — refused rather than re-created.
		return fmt.Errorf("k8s: drive: claim %q existed when this run tried to create it and was gone a moment later: %w",
			drive.ObjectName, errDriveClaimVanished)
	case apierrors.IsForbidden(err):
		return refuseForbiddenDriveClaim("get", ns, drive, err)
	default:
		return fmt.Errorf("k8s: drive: re-read claim %q after losing the race to create it: %w", drive.ObjectName, err)
	}
}

// refuseForbiddenDriveClaim is the ONE place a 403 on a drive claim becomes an error, splitting the
// audience: the OPERATOR gets the raw apiserver refusal — enough to tell an absent RBAC rule from a
// full ResourceQuota. The MEMBER gets the claim name and one remedy only, since the apiserver's own
// sentence carries `system:serviceaccount:<ns>:<sa>`, which must never reach a failed-run screen.
func refuseForbiddenDriveClaim(verb, ns string, drive *types.DriveMount, err error) error {
	slog.Error("wardynd: k8s substrate: the apiserver refused a drive claim",
		slog.String("verb", verb),
		slog.String("claim", drive.ObjectName),
		slog.String("namespace", ns),
		slog.String("drive_id", drive.DriveID.String()),
		slog.String("home", drive.HomeName),
		slog.String("error", err.Error()))
	return fmt.Errorf("k8s: drive: %w (claim %q) — %s", errDrivePVCForbidden, drive.ObjectName, drivePVCForbiddenRemedy(err))
}

// drivePVCForbiddenRemedy picks which of the two 403 causes to report, on the same evidence a raw
// reader would use: the quota plugin's message always carries `exceeded quota`, RBAC refusals never
// do. Wrong-way round is still safe — the log line always carries the real text too.
func drivePVCForbiddenRemedy(err error) string {
	if strings.Contains(err.Error(), "exceeded quota") {
		return driveForbiddenQuota
	}
	return driveForbiddenRBAC
}

// reuseDriveClaim decides whether an existing claim may back this run: identity and Terminating
// refuse it, everything else warns. Identity is checked FIRST since it's the only one whose wrong
// answer hands one member another member's files. Terminating refuses because a DeletionTimestamp
// claim is finalizer-pinned — a new pod mounting it never gets admitted, and recreating it would let
// the run mount an empty volume where the member's files were. Every SHAPE disagreement is a
// WARNING; see driveClaimDrift for why the member's data wins over Wardyn's opinion of its shape.
func reuseDriveClaim(claim *corev1.PersistentVolumeClaim, drive *types.DriveMount) error {
	if err := driveClaimIdentity(claim, drive); err != nil {
		return err
	}
	if claim.DeletionTimestamp != nil {
		return fmt.Errorf("k8s: drive: claim %q is Terminating (deleted at %s): %w",
			claim.Name, claim.DeletionTimestamp.UTC().Format("2006-01-02T15:04:05Z"), errDriveClaimTerminating)
	}
	if drift := driveClaimDrift(claim, drive); len(drift) > 0 {
		slog.Warn("wardynd: k8s substrate: mounting an existing drive claim whose spec disagrees with the drive it was resolved from",
			slog.String("claim", claim.Name),
			slog.String("drive_id", drive.DriveID.String()),
			slog.String("home", drive.HomeName),
			slog.String("drift", strings.Join(drift, "; ")))
	}
	return nil
}

// applyDriveToPod attaches the run's drive to the agent pod: the claim as a Volume, the mount at the
// RESERVED target the control plane carried (runner.DriveTarget, never re-typed), the one pod-level
// SecurityContext this substrate ever sets, and — on a gVisor pod only — the per-mount directfs
// annotation. Append and set, never assign over, on all four: a wholesale assignment would silently
// drop whatever else adds one later with no test catching it; the mount goes on the MAIN container by name, not index.
//
// ReadOnly is set on BOTH the Volume and the VolumeMount — the volume-level flag is what the
// kubelet enforces, the mount-level flag is what a pod-spec reader sees; disagreeing halves are how
// a read-only allocation comes up writable.
//
// FSGroup is managed-claim only: a fresh k8s_pvc belongs to ONE principal, so group-owning its root
// to gid 1000 is safe. A SHARE (k8s_pvc_static) is an admin-precreated claim over an export with
// several principals' files already on it, and fsGroup must NEVER be set there — the CSI NFS
// driver's `fsGroupPolicy: File` lets Kubernetes re-own an NFS volume regardless of fstype or access mode, and
// OnRootMismatch only narrows WHEN, not WHAT: the first run against an export not already gid 1000
// walks it and re-owns other people's files. Kind(), not a backend list, decides this — an unknown
// backend reads as a SHARE, the fail-closed direction. FSGroupChangePolicy stays OnRootMismatch
// (not Always) so a recursive chown doesn't turn every pod start into minutes.
//
// gVisor (CC2/CC3): a network-backed volume under runsc wants `directfs` OFF, stamped on pods whose
// resolved RuntimeClass handler is runsc; the annotation alone does not deliver it (runsc discards
// this hint outright — see driveDirectfsAnnotation), so the node flag `--directfs=false` is the only
// real remedy (docs/OPERATIONS.md).
func applyDriveToPod(pod *corev1.Pod, drive *types.DriveMount, runtimeHandler string) {
	if strings.HasPrefix(runtimeHandler, handlerRunscPrefix) {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[driveDirectfsAnnotation] = "off"
	}
	spec := &pod.Spec
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: driveVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: drive.ObjectName,
				ReadOnly:  drive.ReadOnly,
			},
		},
	})
	mount := corev1.VolumeMount{Name: driveVolumeName, MountPath: drive.Target, ReadOnly: drive.ReadOnly}
	for i := range spec.Containers {
		if spec.Containers[i].Name == mainContainerName {
			spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts, mount)
		}
	}
	if drive.Backend.Kind() != types.DriveKindManaged {
		return
	}
	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	policy := corev1.FSGroupChangeOnRootMismatch
	spec.SecurityContext.FSGroup = int64Ptr(driveFSGroup)
	spec.SecurityContext.FSGroupChangePolicy = &policy
}
