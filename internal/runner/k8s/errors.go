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
// A 403 has a second cause worth naming in the same sentence, because the two
// are indistinguishable by status code and take opposite remedies: a namespace
// ResourceQuota refusing the claim also answers 403. The apiserver's own message
// is interpolated ahead of this text at both call sites, so the sentence tells
// the reader which half of it to act on.
var errDrivePVCForbidden = errors.New(
	"this deployment may not create or read per-person volumes: grant the wardynd ServiceAccount " +
		"`persistentvolumeclaims: get, create` in the runs namespace (Helm chart: userDrives.enabled=true) " +
		"— unless the apiserver's message above says `exceeded quota`, in which case a ResourceQuota in " +
		"that namespace refused the claim and RBAC is not the problem")

// errDriveNameInvalid is validateDriveMount's refusal of a mount whose resolved
// object name cannot name a PersistentVolumeClaim, whose home name cannot be a
// label value, or whose managed allocation is zero. The driver validates its own
// inputs rather than trusting the resolver that derived them: names cross a
// process boundary (an older control plane, an operator's own API call, a future
// backend), and a driver that trusts its input has no fail-closed path left —
// only the apiserver's own 422, mid-dispatch, as somebody's run failure hint.
var errDriveNameInvalid = errors.New("this drive cannot be mounted on Kubernetes: the directory name it resolves to is not a legal object name (ask an administrator to set your directory name on the allocation)")

// errDriveClaimTerminating is reuseDriveClaim's refusal of an existing claim
// that is being deleted. A finalizer-pinned claim admits no new pod, so reusing
// it would hang the run until the dispatch timeout with no readable cause; and
// re-creating it under the same name would undo the reclaim an operator is
// deliberately performing, handing the member an empty volume where their files
// were. Neither is a choice a driver may make silently.
var errDriveClaimTerminating = errors.New("your drive's volume claim is being deleted and cannot be mounted; wait for the deletion to finish, or ask an administrator whether it should have been deleted at all")

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
