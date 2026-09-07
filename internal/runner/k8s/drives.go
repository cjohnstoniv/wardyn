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

// The user drive is the FIRST and ONLY Volume/VolumeMount this substrate
// creates. It is not a host bind and it is not a spec.Mounts entry — it arrives
// on its own field (SandboxSpec.Drive), which is exactly why CreateSandbox's
// blanket refusal of spec.Mounts (errMountsUnsupported) needs no drive
// exemption: the two can never be confused for one another.
const (
	// driveVolumeName ties the agent pod's Volume to the VolumeMount on its main
	// container and on the ephemeral exec container Exec adds. Static, with no
	// per-run entropy: one principal has at most one drive, so one run mounts at
	// most one claim.
	driveVolumeName = "drive"

	// driveDirectfsAnnotation is the key applyDriveToPod stamps on runsc drive
	// pods to serve THIS volume with directfs off. Built from driveVolumeName
	// rather than re-typed, so renaming the volume cannot leave the annotation
	// naming a mount that no longer exists.
	//
	// IT IS INERT TODAY, and nothing depends on it working. runsc keeps a mount
	// hint only when it carries `share`, `source` AND `type` alongside the
	// option — one missing any of them is discarded ("ignoring mount
	// annotations for ... because of missing required field(s)",
	// runsc/boot/mount_hints.go's NewPodMountHints) — and this key sets none of
	// the three. Nor does the <NAME> in the key bind a hint to a mount:
	// FindMount matches on the mount's SOURCE PATH, which for a
	// CSI-provisioned claim is a per-pod path the kubelet generates and no
	// static annotation can name in advance. The remedy that does work is the
	// NODE flag --directfs=false; see docs/OPERATIONS.md, "User drives on
	// Kubernetes". Left stamped because the key is the right one if the hint is
	// ever completed — which is a code change, not a configuration one.
	driveDirectfsAnnotation = "dev.gvisor.spec.mount." + driveVolumeName + ".directfs"

	// labelDrive marks a claim as belonging to one drive ROW (by id, so a rename
	// does not orphan the label the way the object name's slug would), and
	// labelDriveHome to one person's home within it. Byte-for-byte the pair the
	// docker driver stamps on a managed volume, so an operator's selector reads
	// the same on either substrate — the same argument naming.go's label
	// vocabulary makes for the run/component/managed keys, and the reason
	// neither key takes a "/" prefix.
	//
	// The pair is not merely descriptive: it is the ONLY evidence that a claim
	// found under a member's object name is that member's storage, and
	// driveClaimIdentity refuses the run when either disagrees. An object name
	// cannot answer that question — see errDriveClaimForeign.
	//
	// There is deliberately NO wardyn.run-id label here. A drive outlives every
	// run that mounts it, and teardownByRunID sweeps by exactly that selector:
	// one run label on this object would make the first run to finish delete the
	// person's storage.
	labelDrive     = "wardyn.drive"
	labelDriveHome = "wardyn.home"
	// labelDriveSubject is the resolver's fingerprint of the PERSON the home was
	// derived from (types.DriveSubjectHash) — a digest, never the claim. It is
	// the label that tells two principals apart when a non-injective home
	// template folds them onto one home; absent on claims stamped before it
	// existed, which is why its compare is guarded on presence.
	labelDriveSubject = "wardyn.subject"

	// driveFSGroup is the GROUP id the kubelet group-owns a freshly provisioned
	// volume's root with. It is 1000 because that is the gid every wardyn agent
	// image runs as — agentSecurityContext pins RunAsUser to the same number, and
	// the two being equal is a property of the images, not a fact about fsGroup:
	// this field is a GID and can never make a volume user-owned. Without it a
	// block volume comes up root-owned and the agent cannot write to its own
	// drive.
	//
	// A MANAGED claim only. applyDriveToPod states why a SHARE must never carry
	// it: the kubelet would re-own an export several principals' homes live on.
	driveFSGroup int64 = 1000
)

// validateDriveMount is the driver's OWN contract over the mount it is handed,
// re-derived from scratch and deliberately not delegated to the control plane
// that produced it.
//
// The control plane does validate — the API refuses a home name that is not a
// DNS-1123 subdomain on a Kubernetes backend, and refuses a managed drive whose
// allocation is zero. This is not a duplicate of that check, it is the reason a
// driver can fail closed at all. The names cross a process boundary: the
// resolver that derived this one may be an older release than the driver
// binding it, an operator's own API call may have written the row, and a future
// backend may derive an object name some other way. A driver that TRUSTS its
// input has no failure mode left except the apiserver's — a 422 on somebody's
// run, mid-dispatch, whose field-path prose becomes the run's failure hint.
//
// Each rule is the property one specific API object needs:
//
//   - ObjectName becomes the PersistentVolumeClaim's NAME, which the apiserver
//     validates as a DNS-1123 subdomain. An Entra `sub` carries `_`, which is
//     legal in a Docker volume name and illegal here — the exact shape that
//     validated upstream and then failed at bind time.
//   - HomeName is stamped as a LABEL VALUE (labelDriveHome), a different and
//     narrower alphabet than an object name, so it gets its own check rather
//     than riding the first one.
//   - SizeMiB is the managed claim's `requests.storage`, and a zero request is
//     not a claim the apiserver will bind. A share never asks for storage at
//     all, so the rule is scoped to the backend that does.
//   - Target becomes the agent pod's VolumeMount.MountPath, and is the one
//     field here that is not a name at all: it arrives already decided
//     (runner.DriveTarget, which the resolver copies rather than derives) and
//     nobody authors it. It is checked anyway, because it is the only field on
//     this struct whose wrong value mounts the member's storage ON TOP of
//     something else in the sandbox rather than merely failing — see
//     errDriveTargetInvalid.
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

// driveClaimDrift lists every way an EXISTING claim disagrees with the drive the
// resolver says it belongs to. Empty means the claim is the one this drive
// would have created.
//
// Everything it lists is a SHAPE disagreement, and shape is what may be warned
// about, because the claim is the member's data. Refusing the run on a size
// change would mean an admin editing the allocation in the console breaks every
// existing member's runs — and a PVC's request cannot be shrunk anyway (the
// Role holds no patch verb either), so honouring the new number is not on the
// table. What an operator actually needs is to KNOW the two disagree, in a line
// they can grep, before somebody asks why a drive shows 20 GiB in the console
// and 10 GiB in the pod.
//
// IDENTITY is deliberately NOT here. "Whose claim is this?" is not a shape
// question and has no warn-and-continue answer: see reuseDriveClaim, which
// refuses on it (errDriveClaimForeign) before this function is ever called, as
// it does for a Terminating claim.
//
// Scoped to the MANAGED backend on purpose. A static share's claim is an
// admin's object: its class and its size are facts about their storage, not
// drift from anything Wardyn asserted, and the drive row's size is a display
// value there (StorageEnforcementExternal).
//
// Returning the descriptions instead of logging them keeps the comparison
// itself testable without a log handler in the way.
func driveClaimDrift(claim *corev1.PersistentVolumeClaim, drive *types.DriveMount) []string {
	if drive.Backend != types.DriveBackendK8sPVC {
		return nil
	}
	var drift []string
	// Only when the drive names a class. An empty StorageClass means "the
	// cluster default", and a bound claim reports whatever class the default
	// resolved TO — comparing "" against that would report drift on every
	// correctly-provisioned claim on every cluster with a default class.
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

// driveClaimIdentity answers the one question about an existing claim that has
// no warn-and-continue answer: is this object the storage this member's drive
// names, or somebody else's?
//
// The claim's own labels are the only evidence available, because the NAME
// cannot supply it — see errDriveClaimForeign for the two ways one name comes
// to cover two (drive, home) pairs. Both are stamped at create (ensureDrivePVC)
// and both are compared, because either alone is satisfiable by the wrong
// object: one drive's two members share a wardyn.drive value, and two drives'
// same-named homes share a wardyn.home value.
//
// A LABEL-LESS claim is foreign too. Every claim this driver has ever created
// carries all three labels, so an unlabelled object under a member's claim name
// is an admin's own object (or another tool's) that happens to collide — not an
// older Wardyn claim to be adopted.
//
// The share arm is the mirror image. A k8s_pvc_static claim is an admin's
// pre-provisioned handle and carries none of Wardyn's labels; one that carries
// wardyn.managed=true is a MANAGED claim this driver provisioned for ONE person,
// under the very name an admin has now pointed a whole share at. Mounting it
// would hand every member of that share one member's private drive.
func driveClaimIdentity(claim *corev1.PersistentVolumeClaim, drive *types.DriveMount) error {
	switch drive.Backend {
	case types.DriveBackendK8sPVC:
		if got := claim.Labels[labelDrive]; got != drive.DriveID.String() {
			return fmt.Errorf("k8s: drive: claim %q carries %s=%q, this drive's row id is %q: %w",
				claim.Name, labelDrive, got, drive.DriveID, errDriveClaimForeign)
		}
		if got := claim.Labels[labelDriveHome]; got != drive.HomeName {
			return fmt.Errorf("k8s: drive: claim %q carries %s=%q, this run's home is %q: %w",
				claim.Name, labelDriveHome, got, drive.HomeName, errDriveClaimForeign)
		}
		if got := claim.Labels[labelDriveSubject]; got != "" && got != drive.SubjectHash {
			// Presence-guarded on purpose: every claim this driver stamped before
			// the label existed would otherwise turn foreign on upgrade and orphan
			// every member's drive.
			return fmt.Errorf("k8s: drive: claim %q carries %s=%q, this run's subject hashes to %q: %w",
				claim.Name, labelDriveSubject, got, drive.SubjectHash, errDriveClaimForeign)
		}
	case types.DriveBackendK8sPVCStatic:
		if claim.Labels[labelManaged] == "true" {
			return fmt.Errorf("k8s: drive: claim %q is a %s=true claim wardynd provisioned as one person's managed drive, not an administrator's share: %w",
				claim.Name, labelManaged, errDriveClaimForeign)
		}
		// AND THE SAME SUBJECT CHECK THE MANAGED ARM MAKES, presence-guarded for
		// the same reason. A static claim's NAME is minted by Wardyn
		// (types.DriveObjectNamedByWardyn), so a template that folds two
		// principals onto one home folds them onto one claim — and this arm,
		// which had no per-principal evidence at all, bound it for both of them.
		// types.ValidateUserDrive now refuses `email_local` on this backend,
		// which closes the shape that reaches here by authoring; this closes the
		// row that predates the rule, the one written by hand, and the admin's
		// own mistake of pointing two members at one pre-provisioned claim.
		//
		// PRESENCE-GUARDED, so it is opt-in for the operator: an admin's
		// pre-provisioned claim carries none of Wardyn's labels and stays
		// bindable exactly as before. An admin who DOES stamp
		// wardyn.subject=<DriveSubjectHash(subject)> on the claims they
		// provision gets per-principal binding enforced by the driver — a digest,
		// never the subject itself, which is why it is a label a share owner can
		// safely publish.
		if got := claim.Labels[labelDriveSubject]; got != "" && got != drive.SubjectHash {
			return fmt.Errorf("k8s: drive: claim %q carries %s=%q, this run's subject hashes to %q: %w",
				claim.Name, labelDriveSubject, got, drive.SubjectHash, errDriveClaimForeign)
		}
	}
	return nil
}

// driveRequestBytes is the allocation in bytes: <SizeMiB>Mi, which is what the
// claim asks for and what a drifted claim is compared against. One expression,
// so the create path and the comparison path can never disagree about what the
// drive asked for.
func driveRequestBytes(drive *types.DriveMount) int64 { return int64(drive.SizeMiB) * 1024 * 1024 }

// ensureDrivePVC makes the run's PersistentVolumeClaim exist, and is the only
// place this substrate provisions storage.
//
//   - k8s_pvc (managed): Get by name, Create on NotFound. Idempotent by name, so
//     a member's second run reuses the first run's claim instead of racing
//     another one into the namespace. A Create that comes back AlreadyExists is
//     a LOST race and not a success — the claim that won it is somebody's, and
//     WHOSE is a question only a re-read answers: see reuseRaceWinnerClaim.
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
	// Before anything is asked of the cluster: an illegal name is this driver's
	// refusal to make, not the apiserver's to make for it.
	if err := validateDriveMount(drive); err != nil {
		return err
	}

	existing, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, drive.ObjectName, metav1.GetOptions{})
	switch {
	case err == nil:
		return reuseDriveClaim(existing, drive)
	case apierrors.IsForbidden(err):
		// The DEFAULT deployment's failure. userDrives.enabled is off out of the
		// box, so the Role carries no persistentvolumeclaims rule at all and the
		// very first thing a drive does — the lookup, which even a static share
		// needs — is refused. Mapped to the same sentinel the Create arm uses,
		// through the same scrub, because the raw apiserver text ("cannot get
		// resource ... in the namespace") names no switch an operator could flip
		// and names two things a member should never be handed.
		return refuseForbiddenDriveClaim("get", ns, drive, err)
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("k8s: drive: look up claim %q: %w", drive.ObjectName, err)
	case drive.Backend == types.DriveBackendK8sPVCStatic:
		return fmt.Errorf("k8s: drive: claim %q is absent from namespace %q: %w", drive.ObjectName, ns, errDriveClaimNotProvisioned)
	}

	// ObjectName and HomeName are used raw, and validateDriveMount above is what
	// makes that safe rather than the upstream promise that they would be.
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
				// The allocation, in bytes with a binary format, which is
				// <SizeMiB>Mi in canonical form (the apiserver re-canonicalizes
				// any spelling of it anyway — 10240Mi comes back as 10Gi). It is
				// a REQUEST: whether it binds is the storage class's answer, not
				// Wardyn's. See StorageEnforcement, and the honesty sentence.
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

// reuseRaceWinnerClaim decides whether the claim that WON a Get→Create race may
// back this run — the arm an AlreadyExists lands in, and the one place in this
// file where "the object exists" is deliberately NOT good enough.
//
// The loser of the race has learned one fact: a claim now sits under its
// object name. It has learned nothing about WHOSE. Two DIFFERENT first runs can
// reach this line over the same name, because the name folds two variable-width
// fields with a separator both admit — drive "eng" with home "us-bob" and drive
// "eng-us" with home "bob" both resolve to wardyn-drive-eng-us-bob (see
// errDriveClaimForeign). Both Get→NotFound inside the window, both Create, one
// wins; a loser that treated AlreadyExists as success would mount the winner's
// storage at the drive target inside its own member's agent — the exact
// cross-mount reuseDriveClaim exists to refuse, arriving through the one door
// that used to skip it. The same window swallows a Terminating claim: an
// operator's `kubectl delete pvc` between the Get and the Create leaves a
// finalizer-pinned object whose AlreadyExists reads identical from here.
//
// So the loser goes back and READS what it lost to, then hands it to
// reuseDriveClaim — identity first, Terminating second, drift warned — exactly
// as though its own Get had found it. Neither refusal is one this function may
// make on its own evidence; both are ones reuseDriveClaim already knows how to
// make on the claim's.
//
// It does NOT retry the Create. This driver holds `get` and `create` and
// nothing else: it cannot delete the object in its way, and a second Create
// would answer AlreadyExists again or resurrect a claim an operator is
// reclaiming.
func reuseRaceWinnerClaim(ctx context.Context, client kubernetes.Interface, ns string, drive *types.DriveMount) error {
	winner, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, drive.ObjectName, metav1.GetOptions{})
	switch {
	case err == nil:
		return reuseDriveClaim(winner, drive)
	case apierrors.IsNotFound(err):
		// Present for the Create and gone for the read: a reclaim landing
		// mid-dispatch. Refused rather than re-created — see errDriveClaimVanished.
		return fmt.Errorf("k8s: drive: claim %q existed when this run tried to create it and was gone a moment later: %w",
			drive.ObjectName, errDriveClaimVanished)
	case apierrors.IsForbidden(err):
		// Routed through the same scrub as every other 403 on a claim: the raw
		// text names the ServiceAccount, and a re-read is no more entitled to
		// hand a member that than the first read was.
		return refuseForbiddenDriveClaim("get", ns, drive, err)
	default:
		return fmt.Errorf("k8s: drive: re-read claim %q after losing the race to create it: %w", drive.ObjectName, err)
	}
}

// refuseForbiddenDriveClaim is the ONE place a 403 on a drive claim becomes an
// error somebody reads, and it splits the audience in two on purpose.
//
// The OPERATOR gets the raw apiserver refusal, in a log line with the claim,
// the namespace and the drive it belongs to — everything needed to tell an
// absent RBAC rule from a full ResourceQuota, and nothing has been taken away
// from them.
//
// The MEMBER gets the claim name and one remedy. What they must not get is the
// apiserver's own sentence: a CreateSandbox error is the run's failure hint
// verbatim, and that sentence carries `system:serviceaccount:<ns>:<sa>` — the
// runs namespace and the runner's ServiceAccount, handed to every member whose
// run happens to fail. The claim name is in both halves, which is what lets an
// operator join a member's screenshot to the log line without the member ever
// having held the cluster's names.
//
// The verb ("get"/"create") is the operator's only clue to WHICH rule is
// missing when only one of the two is granted, so it rides the log line rather
// than the message.
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

// drivePVCForbiddenRemedy picks the ONE of the two 403 causes to tell the reader
// about, on the same evidence a reader of the raw text would have used: the
// quota admission plugin's message always carries `exceeded quota`, and an RBAC
// refusal never does. Wrong-way round it is still safe — an operator whose quota
// is full and who is told about RBAC finds the rule already granted, and the log
// line has the real text either way.
func drivePVCForbiddenRemedy(err error) string {
	if strings.Contains(err.Error(), "exceeded quota") {
		return driveForbiddenQuota
	}
	return driveForbiddenRBAC
}

// reuseDriveClaim decides whether a claim that already exists may back this run.
// Two states refuse it and everything else is a warning.
//
// IDENTITY IS A REFUSAL, and it is checked FIRST because it is the only one of
// the three whose wrong answer hands one member another member's files. See
// driveClaimIdentity for what the labels decide and errDriveClaimForeign for why
// the object name cannot.
//
// TERMINATING IS A REFUSAL, the one existing-claim state that cannot be a
// warning even for a claim that IS this member's. A claim with a
// DeletionTimestamp is finalizer-pinned until the
// last pod using it goes away; a NEW pod mounting it is never admitted (the
// apiserver refuses a pod referencing a terminating claim, and where it does
// not, the scheduler leaves it Pending forever). Reusing it would turn one
// operator's `kubectl delete pvc` into a run that hangs until the dispatch
// timeout with no readable cause. Recreating it is worse: the name is the same,
// so the claim an operator is deliberately reclaiming would come back under
// them, and the run would mount an empty volume where the member's files were.
//
// Everything else — every SHAPE disagreement — is a WARNING; see driveClaimDrift
// for why the member's data wins over Wardyn's opinion of its shape.
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

// applyDriveToPod attaches the run's drive to the agent pod: the claim as a
// Volume, the mount at the RESERVED target the control plane carried on the
// mount (runner.DriveTarget — never a literal re-typed here, so the path a
// policy or workspace source is refused for naming and the path this binds at
// are the same string by construction), the one pod-level SecurityContext this
// substrate ever sets, and — on a gVisor pod only — the per-mount directfs
// annotation below.
//
// APPEND AND SET, NEVER ASSIGN OVER, on all four. This is the only code in the
// package that gives the agent pod a Volume, a VolumeMount, a pod-level
// SecurityContext or an annotation today — so a wholesale assignment is correct
// right now and silently wrong the first time anything else adds one (a
// projected token volume, a recording volume, a second security field, a
// checksum annotation). An assignment does not fail a test when that day comes;
// it drops the other half and the pod comes up missing something nobody was
// watching. The mount goes on the MAIN container by name rather than by index
// for the same reason.
//
// ReadOnly is set on the Volume AND on the VolumeMount: the volume-level flag is
// what the kubelet passes to the mount, and the mount-level flag is what a
// reader of the pod spec (and an admission policy) sees — disagreeing halves are
// how a read-only allocation comes up writable.
//
// FSGROUP IS A MANAGED-CLAIM FIELD, AND THE BACKEND DECIDES. A
// dynamically-provisioned k8s_pvc comes up EMPTY and belongs to ONE principal,
// so group-owning its root to the gid every agent image runs as is both
// necessary (a root-owned volume root is EACCES for uid 1000, and the control
// plane must never chown volume state itself) and harmless — there is nothing
// in it yet but that member's own future files.
//
// A SHARE (k8s_pvc_static) is the opposite object: an admin-precreated claim
// over an export whose files are owned by whatever the export says, with
// several principals' homes among them. This function used to set fsGroup on it
// too, justified by a claim that the kubelet does not apply fsGroup to an
// NFS-type volume. THAT CLAIM IS FALSE for the upstream CSI NFS driver
// (kubernetes-csi/csi-driver-nfs), which ships `fsGroupPolicy: File` — and File
// means, verbatim, that Kubernetes may use fsGroup to change permissions and
// ownership of the volume "regardless of fstype or access mode".
// ReadWriteOnceWithFSType, the policy that really is limited to block storage,
// is only the DEFAULT for a driver that declares none. OnRootMismatch narrows
// WHEN, never WHAT: the first run whose export root is not already gid 1000
// walks the volume and re-owns what it finds, which on a share is other
// people's files. So the field is not set at all for a share; the ownership
// recipe for one is the export's own uid/gid mapping (docs/OPERATIONS.md, "User
// drives on Kubernetes").
//
// FSGroupChangePolicy stays OnRootMismatch rather than Always for the managed
// case it survives on: a recursive chown of a large existing drive on every
// single run is how a pod start goes from seconds to minutes, and the root's own
// ownership already answers the question.
//
// Kind(), not a backend list: types.DriveBackend.Kind reads an UNKNOWN backend
// as a SHARE, so a row this binary does not understand gets no fsGroup either.
// That is the fail-closed direction here — the harm is in setting the field on
// storage Wardyn does not own, never in omitting it.
//
// PSS Restricted admits `persistentVolumeClaim` as a volume type; it forbids
// `hostPath` under Baseline and Restricted alike. No drive backend on this
// substrate offers a host path, so nothing here has to refuse one.
//
// GVISOR (the CC2/CC3 RuntimeClasses resolveRuntimeClassName pins): a
// network-backed volume under runsc wants `directfs` OFF — the gofer donates a
// file descriptor per mount point and the sandbox then operates on it directly,
// which a 9p/NFS-backed export does not reliably support. This function stamps
// the per-mount annotation (`dev.gvisor.spec.mount.<NAME>.directfs: "off"`) on
// exactly the pods that would want it and no others: a drive pod whose resolved
// RuntimeClass handler is runsc. runtimeHandler is "" for every other pod, which
// is why the parameter exists at all — it is the RuntimeClass's .Handler, not
// its object name, that names the runtime family (see handlerRunscPrefix). It is
// stamped here because this is the only place in the tree where a drive volume
// and a RuntimeClass meet.
//
// THE ANNOTATION DOES NOT DELIVER IT: runsc discards this hint outright (see
// driveDirectfsAnnotation for why, with the upstream reference). The node flag
// `--directfs=false` is therefore not one remedy among two — it is the only
// one, and docs/OPERATIONS.md "User drives on Kubernetes" carries that recipe
// plus the `--file-access-mounts` caching caveat a share other writers touch
// needs. containerd's `pod_annotations = ["dev.gvisor.*"]` gate sits upstream
// of all of it: without that, the annotation never reaches runsc to be
// discarded in the first place.
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
