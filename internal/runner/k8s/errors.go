// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import "errors"

// errProxyImageUnset is returned when a Driver is asked to build the egress
// sidecar (and run the boot-time canary, which reuses the same image) but no
// proxy image was configured. Mirrors docker's errProxyImageUnset.
var errProxyImageUnset = errors.New("k8s: wardyn-proxy image not configured")

// errRuntimeClassUnavailable: fail-closed sentinel when the policy demands a
// confinement class whose RuntimeClass isn't registered (or fails docker's
// same floor guard). Wrapped so callers can errors.Is and refuse rather than
// silently downgrade (invariant 5).
var errRuntimeClassUnavailable = errors.New("required confinement RuntimeClass unavailable")

// errCanaryIndeterminate: the boot-time canary couldn't tell NetworkPolicy
// enforced from not enforced (a non-network failure in either phase). Refuses
// to boot rather than guess.
var errCanaryIndeterminate = errors.New("k8s: egress canary indeterminate (non-network failure, not a NetworkPolicy verdict)")

// errNetworkPolicyUnenforced: the deny-all canary phase failed to block the
// connect — the CNI doesn't enforce NetworkPolicy.
// WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1 downgrades this to a warning; either
// way NetworkPolicy=false is what gets advertised.
var errNetworkPolicyUnenforced = errors.New("k8s: NetworkPolicy is not enforced on this cluster (deny-all canary connected anyway)")

// errMountsUnsupported: CreateSandbox's fail-closed rejection of any
// spec.Mounts entry — no host filesystem is reachable from a pod the way a
// docker bind mount reaches the daemon host.
var errMountsUnsupported = errors.New("k8s: host bind mounts are not supported on this substrate; use a repo workspace or proxy-side credential injection instead")

// errDriveClaimNotProvisioned: a SHARE drive (k8s_pvc_static) whose claim
// doesn't exist. wardynd never creates it — it's an admin's pre-provisioned
// handle on a real share, and auto-creating an empty one would hand the
// member a blank volume where their files should be. This becomes the run's
// failure hint verbatim, so it must never be wrapped with the claim name or
// namespace (those go to the operator's log line instead, in ensureDrivePVC).
//
// DRAFT (M2 canon pending) — the frozen member sentence for this refusal.
var errDriveClaimNotProvisioned = errors.New("drive: your drive's volume is not provisioned on this cluster — ask an admin")

// errDrivePVCForbidden is ensureDrivePVC's refusal when the apiserver answers
// EITHER the claim's Get or its Create with a 403.
//
// Both arms map here, and the Get one is the failure a DEFAULT deployment
// actually meets: drives.enabled is off out of the box, so the runner Role
// carries no persistentvolumeclaims rule at all and the LOOKUP — the one call
// even an admin-provisioned share makes — is what gets refused first. Left
// unmapped it surfaced the apiserver's own "cannot get resource" text, which
// names no switch an operator could flip.
//
// The apiserver's own words are not in it, and that is what the two remedies
// below are for. A CreateSandbox error becomes the run's failure hint verbatim
// (dispatchRun's failAndRevoke, internal/api), and that hint is read by the
// MEMBER whose run failed. A raw 403 reads `persistentvolumeclaims is
// forbidden: User "system:serviceaccount:<ns>:<sa>" cannot get resource ...` —
// which hands every member of the deployment the runs namespace and the
// runner's ServiceAccount name, the two strings anybody aiming at this
// cluster's RBAC needs first. refuseForbiddenDriveClaim slogs the raw error
// instead, and the CLAIM NAME appears in both the log line and the hint, so an
// operator joins the member's report to the full text without the member ever
// having held it.
var errDrivePVCForbidden = errors.New("the apiserver refused this deployment access to your drive's storage")

// driveForbiddenRBAC and driveForbiddenQuota are the two causes of a 403, and
// exactly one of them is appended to errDrivePVCForbidden.
//
// They are indistinguishable by status code and take opposite remedies.
// Because the apiserver text that would let a reader tell them apart is never
// shown, drivePVCForbiddenRemedy picks instead, on the same evidence the
// reader would have used (`exceeded quota`, the substring the quota admission
// plugin's message always carries), so one instruction arrives instead of two.
const (
	driveForbiddenRBAC = "grant the wardynd ServiceAccount `persistentvolumeclaims: get, create` in the runs namespace (Helm chart: drives.enabled=true)"

	driveForbiddenQuota = "a storage ResourceQuota in the runs namespace is the cause and RBAC is not: raise the quota, or lower this drive's allocation"
)

// errDriveNameInvalid: validateDriveMount's refusal when the resolved object
// or home name isn't legal (PVC name / label value). The driver validates its
// own inputs rather than trusting the resolver, since names cross a process
// boundary and the only other failure path is the apiserver's 422 mid-dispatch.
var errDriveNameInvalid = errors.New("this drive cannot be mounted on Kubernetes: the directory name it resolves to is not a legal object name (ask an administrator to set your directory name on the allocation)")

// errDriveAllocationInvalid: validateDriveMount's refusal of a MANAGED mount
// whose allocation size a claim can't request. Its own sentinel (not
// errDriveNameInvalid's) so the member is pointed at the size field, not the
// directory-name field.
var errDriveAllocationInvalid = errors.New("this drive cannot be mounted on Kubernetes: its allocation is not a size a volume claim can request (ask an administrator to set the size on the allocation)")

// errDriveTargetInvalid: the mount's Target isn't runner.DriveTarget, the one
// in-container path a drive may bind at. Nobody authors this field (the
// resolver copies the constant), so a wrong value is never a typo — and it's
// the only drive field whose wrong value could shadow something else inside
// the sandbox (the credential directory, or the image root) with persistent
// storage. Its own sentinel, since errDriveNameInvalid's wording points at a
// field an admin can't fix this from.
var errDriveTargetInvalid = errors.New("this drive cannot be mounted on Kubernetes: it did not arrive addressed to the reserved drive path; report the run to your administrator (no setting on the allocation produces this)")

// errDriveClaimTerminating: reuseDriveClaim's refusal of an existing claim
// being deleted. A finalizer-pinned claim admits no new pod (would hang the
// run to the dispatch timeout with no readable cause), and re-creating it
// under the same name would undo an operator's deliberate reclaim.
var errDriveClaimTerminating = errors.New("your drive's volume claim is being deleted and cannot be mounted; wait for the deletion to finish, or ask an administrator whether it should have been deleted at all")

// errDriveClaimVanished: ensureDrivePVC's refusal of the one window a lost
// create race can lose twice — Create answered AlreadyExists, the re-read it
// forces answered NotFound. Only an operator's reclaim landing mid-dispatch
// does that; the driver holds `get`/`create` only, so it can't investigate,
// and the next run's Get→NotFound→Create self-heals.
var errDriveClaimVanished = errors.New("your drive's volume claim was deleted while your run was starting; start the run again")

// errDriveClaimForeign: reuseDriveClaim's refusal of an existing claim whose
// IDENTITY labels (not its name — types.DriveObjectName's slug-folding join
// can collide two different (drive, home) pairs onto one name) say it belongs
// to a different member. The member-facing text carries no evidence: wrapping
// it with the claim name, the label, or wardyn.subject (a digest of a person)
// would hand one member's run failure another principal's identity. The
// comparison goes to the operator's log line instead (driveClaimIdentity).
//
// DRAFT (M2 canon pending) — the frozen member sentence for this refusal.
var errDriveClaimForeign = errors.New("drive: your drive's volume is not the one allocated to you — ask an admin")

// errDriveBackendUnsupported: ensureDrivePVC's refusal of a drive whose
// backend belongs to another substrate. The control plane already refuses
// this at run create (driveMountFor), so reaching here is a bug — fail closed
// rather than come up with no drive at all.
var errDriveBackendUnsupported = errors.New("k8s: this drive's backend is not a Kubernetes one and cannot be mounted on this substrate")

// errSecondExec: Kubernetes ephemeral containers are ADD-ONLY, so a second
// Exec against the same ref can't be honoured the way docker's "latest Exec
// wins" re-exec is.
var errSecondExec = errors.New("k8s: exec: this sandbox already has an agent exec; a substrate second Exec on the same ref is not supported (ephemeral containers are add-only)")

// errBYOIUnsupported: BYOI's selftest-then-task double-exec is impossible on
// k8s for the same add-only reason as errSecondExec; refusing on the FIRST
// Exec call gives a specific error instead of a generic once-per-ref one.
var errBYOIUnsupported = errors.New("k8s: BYOI (wardyn-byoi/ images) is docker-only and is refused on this substrate: ephemeral containers cannot honour the selftest-then-task double-exec")

// errTeardownUnresolved: teardown found the ref'd pod but its wardyn.run-id
// label was missing/unparseable, so the sibling proxy pod, NetworkPolicies
// and Secret (all selected by that label) can't be located. Surfaced rather
// than reporting a false success.
var errTeardownUnresolved = errors.New("k8s: teardown could not resolve run id from the sandbox's wardyn.run-id label; sibling proxy/netpols/secret may be orphaned")
