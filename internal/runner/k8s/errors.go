// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import "errors"

// errProxyImageUnset is returned when a Driver is asked to build the egress
// sidecar (and run the boot-time canary, which reuses the same image) but no
// proxy image was configured. Mirrors docker's errProxyImageUnset.
var errProxyImageUnset = errors.New("k8s: wardyn-proxy image not configured")

// errRuntimeClassUnavailable is the fail-closed sentinel for Confinement-Class
// gating: the policy (or WARDYN_CONFINEMENT_MAP) demanded a class whose
// RuntimeClass is not registered in the cluster, or whose .Handler fails the
// same floor guard docker applies. Wrapped (%w) so callers can errors.Is on it
// and refuse the run rather than silently downgrade (invariant 5). Mirrors
// docker's errRuntimeUnavailable.
var errRuntimeClassUnavailable = errors.New("required confinement RuntimeClass unavailable")

// errCanaryIndeterminate is returned by the constructor when the boot-time
// egress canary could not distinguish "NetworkPolicy enforced" from "not
// enforced" — a non-network failure (ImagePullBackOff, scheduling failure, a
// crash before Running) in either canary phase. The substrate refuses to boot
// rather than guess: see the package doc's two-phase canary contract.
var errCanaryIndeterminate = errors.New("k8s: egress canary indeterminate (non-network failure, not a NetworkPolicy verdict)")

// errNetworkPolicyUnenforced is returned by the constructor when Phase B (the
// deny-all NetworkPolicy) failed to block the canary's connect — the cluster's
// CNI does not enforce NetworkPolicy. WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1
// downgrades this to a loud warning instead of refusing boot; either way the
// substrate advertises NetworkPolicy=false (never overclaims).
var errNetworkPolicyUnenforced = errors.New("k8s: NetworkPolicy is not enforced on this cluster (deny-all canary connected anyway)")

// errMountsUnsupported is CreateSandbox's preflight rejection of any
// spec.Mounts entry — the single chokepoint that makes host-mount paths fail
// closed on k8s (no host filesystem is available to a pod the way a docker
// bind mount reaches the daemon host).
var errMountsUnsupported = errors.New("k8s: host bind mounts are not supported on this substrate; use a repo workspace or proxy-side credential injection instead")

// errDriveClaimNotProvisioned is ensureDrivePVC's refusal of a SHARE drive
// (k8s_pvc_static) whose claim does not exist in the runs namespace. wardynd
// deliberately does not create it: that claim is an admin's pre-provisioned
// handle on a real corporate share, and creating an empty one under the same
// name would hand the member a blank volume where their files should be. The
// wording is what an operator reads back, because a CreateSandbox error becomes
// the run's failure hint verbatim (dispatchRun's failAndRevoke, internal/api).
var errDriveClaimNotProvisioned = errors.New("your drive's volume is not provisioned on this cluster; ask an administrator to create the claim (or check the drive's directory-name template)")

// errDrivePVCForbidden is ensureDrivePVC's refusal when the apiserver answers
// EITHER the claim's Get or its Create with a 403.
//
// Both arms map here, and the Get one is the failure a DEFAULT deployment
// actually meets: userDrives.enabled is off out of the box, so the runner Role
// carries no persistentvolumeclaims rule at all and the LOOKUP — the one call
// even an admin-provisioned share makes — is what gets refused first. Left
// unmapped it surfaced the apiserver's own "cannot get resource" text, which
// names no switch an operator could flip.
//
// THE APISERVER'S OWN WORDS ARE NOT IN IT, and that is what the two remedies
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
// EXACTLY ONE of them is appended to errDrivePVCForbidden.
//
// They are indistinguishable by status code and take opposite remedies, which
// is why the sentinel used to carry both halves and ask the reader to pick
// using the apiserver text printed ahead of it — precisely the text that is no
// longer shown. drivePVCForbiddenRemedy picks instead, on the same evidence the
// reader would have used (`exceeded quota`, the substring the quota admission
// plugin's message always carries), and one instruction arrives instead of two.
const (
	driveForbiddenRBAC = "grant the wardynd ServiceAccount `persistentvolumeclaims: get, create` in the runs namespace (Helm chart: userDrives.enabled=true)"

	driveForbiddenQuota = "a storage ResourceQuota in the runs namespace is the cause and RBAC is not: raise the quota, or lower this drive's allocation"
)

// errDriveNameInvalid is validateDriveMount's refusal of a mount whose resolved
// object name cannot name a PersistentVolumeClaim, whose home name cannot be a
// label value, or whose managed allocation is zero. The driver validates its own
// inputs rather than trusting the resolver that derived them: names cross a
// process boundary (an older control plane, an operator's own API call, a future
// backend), and a driver that trusts its input has no fail-closed path left —
// only the apiserver's own 422, mid-dispatch, as somebody's run failure hint.
var errDriveNameInvalid = errors.New("this drive cannot be mounted on Kubernetes: the directory name it resolves to is not a legal object name (ask an administrator to set your directory name on the allocation)")

// errDriveTargetInvalid is validateDriveMount's refusal of a mount whose Target
// is not runner.DriveTarget, the one in-container path a drive may ever bind at.
//
// The target is the field with the least excuse for being trusted and the most
// to lose by it: nobody authors it (the resolver copies the constant), so a
// wrong one is never a typo to be forgiven — and it is the only drive field
// whose wrong value puts the member's persistent, cross-run storage ON TOP of
// something else inside the sandbox. `/home/agent/.claude` would shadow the
// injected credential directory with a writable volume that survives the run;
// `/` would shadow the image. Nothing upstream is trying to do either today,
// which is exactly why the day something does must end in a refusal rather than
// a mount.
//
// Its own sentinel rather than errDriveNameInvalid's, because that one's words
// send the reader to the allocation's directory-name field and no field an
// admin can edit produces this one.
var errDriveTargetInvalid = errors.New("this drive cannot be mounted on Kubernetes: it did not arrive addressed to the reserved drive path; report the run to your administrator (no setting on the allocation produces this)")

// errDriveClaimTerminating is reuseDriveClaim's refusal of an existing claim
// that is being deleted. A finalizer-pinned claim admits no new pod, so reusing
// it would hang the run until the dispatch timeout with no readable cause; and
// re-creating it under the same name would undo the reclaim an operator is
// deliberately performing, handing the member an empty volume where their files
// were. Neither is a choice a driver may make silently.
var errDriveClaimTerminating = errors.New("your drive's volume claim is being deleted and cannot be mounted; wait for the deletion to finish, or ask an administrator whether it should have been deleted at all")

// errDriveClaimVanished is ensureDrivePVC's refusal of the one window a lost
// create race can lose twice: the Create answered AlreadyExists, and the re-read
// that AlreadyExists forces answered NotFound. Something deleted the claim
// between the two calls, and an operator's reclaim landing mid-dispatch is the
// only thing that does.
//
// Creating it again is not on the table — that is the recreate-under-a-reclaim
// errDriveClaimTerminating refuses, arriving one moment later — and this driver
// holds `get` and `create` and nothing else, so it cannot investigate either. A
// refused run is the whole remedy, and the next run's Get→NotFound→Create makes
// it right on its own.
var errDriveClaimVanished = errors.New("your drive's volume claim was deleted while your run was starting; start the run again")

// errDriveClaimForeign is reuseDriveClaim's refusal of an existing claim whose
// IDENTITY labels say it is not this member's storage: a managed claim whose
// wardyn.drive or wardyn.home names a different (drive, home) pair, or a share
// whose object turns out to be a wardyn.managed claim this driver provisioned
// for one person.
//
// The object NAME cannot decide this, which is why the labels have to.
// types.DriveObjectName joins two variable-width fields with the separator both
// of them admit (`wardyn-drive-<slug>-<home>`, and a slug folds case), so the
// pair ("eng", "us-bob") and the pair ("eng-us", "bob") resolve to one claim
// name; a rename does the same thing over time, moving a second person's home
// under a name a first person already holds. Wardyn holds no `delete` verb and
// cannot repair either collision, so the only fail-closed answer is to refuse
// the run — mounting the claim would put one member's private drive at the
// drive target inside another member's agent.
var errDriveClaimForeign = errors.New("your drive's volume claim belongs to a different drive or a different person and will not be mounted; ask an administrator to check the drive's name and directory-name template (two drives whose names differ only in where a dash falls can resolve to the same claim)")

// errDriveBackendUnsupported is ensureDrivePVC's refusal of a drive whose
// backend belongs to another substrate (a Docker volume, a host path). The
// control plane already refuses such a drive at run create — driveMountFor
// compares the backend's RunnerTarget against the deployment's — so reaching
// here is a bug, and the fail-closed answer is an error rather than a run that
// silently comes up with no drive at all.
var errDriveBackendUnsupported = errors.New("k8s: this drive's backend is not a Kubernetes one and cannot be mounted on this substrate")

// errSecondExec is returned when Exec is called twice against the same ref.
// Kubernetes ephemeral containers are ADD-ONLY (a pod's ephemeral-container
// list can only grow), so a second Exec cannot be honoured the way docker's
// "latest Exec wins" re-exec is: never silently no-op, never return the prior
// exec's id (see substrate.Substrate.Exec's doc).
var errSecondExec = errors.New("k8s: exec: this sandbox already has an agent exec; a substrate second Exec on the same ref is not supported (ephemeral containers are add-only)")

// errBYOIUnsupported is Exec's refusal of a wardyn-byoi/ image ref before any
// ephemeral container is created. BYOI's selftest-then-task double-exec
// (internal/api's byoiSelftest + startAgentOrIdle, "latest Exec wins" on
// docker) is impossible on k8s for the same add-only reason as errSecondExec;
// refusing on the FIRST Exec call (rather than letting the selftest succeed
// and only the second Exec fail) gives a clear, specific error instead of a
// generic once-per-ref one.
var errBYOIUnsupported = errors.New("k8s: BYOI (wardyn-byoi/ images) is docker-only and is refused on this substrate: ephemeral containers cannot honour the selftest-then-task double-exec")

// errTeardownUnresolved is returned when teardown found the ref'd pod but its
// wardyn.run-id label was missing or unparseable, so the sibling proxy pod,
// NetworkPolicies, and Secret (all selected by that label) cannot be located.
// Surfaced instead of reporting a false success — mirrors docker's identical
// fail-closed sentinel.
var errTeardownUnresolved = errors.New("k8s: teardown could not resolve run id from the sandbox's wardyn.run-id label; sibling proxy/netpols/secret may be orphaned")
