// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import "errors"

// errProxyImageUnset: the Driver needs the egress sidecar/canary image and
// none was configured. Mirrors docker's errProxyImageUnset.
var errProxyImageUnset = errors.New("k8s: wardyn-proxy image not configured")

// errRuntimeClassUnavailable: fail-closed sentinel for a demanded RuntimeClass that isn't registered (invariant 5).
var errRuntimeClassUnavailable = errors.New("required confinement RuntimeClass unavailable")

// errCanaryIndeterminate: the boot-time canary couldn't tell enforced from not.
var errCanaryIndeterminate = errors.New("k8s: egress canary indeterminate (non-network failure, not a NetworkPolicy verdict)")

// errNetworkPolicyUnenforced: the deny-all canary connected anyway — the CNI
// doesn't enforce NetworkPolicy.
var errNetworkPolicyUnenforced = errors.New("k8s: NetworkPolicy is not enforced on this cluster (deny-all canary connected anyway)")

// errMountsUnsupported: CreateSandbox refuses any spec.Mounts entry — no host
// filesystem is reachable from a pod the way a docker bind mount reaches it.
var errMountsUnsupported = errors.New("k8s: host bind mounts are not supported on this substrate; use a repo workspace or proxy-side credential injection instead")

// errDriveClaimNotProvisioned: a SHARE drive whose claim doesn't exist.
// wardynd never auto-creates one; the claim name/namespace go to the
// operator's log line, not this run's hint (ensureDrivePVC).
//
// DRAFT (M2 canon pending) — the frozen member sentence for this refusal.
var errDriveClaimNotProvisioned = errors.New("drive: your drive's volume is not provisioned on this cluster — ask an admin")

// errDrivePVCForbidden is ensureDrivePVC's refusal on a 403 from the claim's
// Get or Create. The apiserver's own words (naming the runs namespace and
// ServiceAccount) go to refuseForbiddenDriveClaim's log line, not this hint.
var errDrivePVCForbidden = errors.New("the apiserver refused this deployment access to your drive's storage")

// driveForbiddenRBAC and driveForbiddenQuota: drivePVCForbiddenRemedy picks
// one using the `exceeded quota` substring.
const (
	driveForbiddenRBAC = "grant the wardynd ServiceAccount `persistentvolumeclaims: get, create` in the runs namespace (Helm chart: drives.enabled=true)"

	driveForbiddenQuota = "a storage ResourceQuota in the runs namespace is the cause and RBAC is not: raise the quota, or lower this drive's allocation"
)

// errDriveNameInvalid: validateDriveMount's refusal when the resolved object
// or home name isn't a legal PVC name / label value.
var errDriveNameInvalid = errors.New("this drive cannot be mounted on Kubernetes: the directory name it resolves to is not a legal object name (ask an administrator to set your directory name on the allocation)")

// errDriveAllocationInvalid: validateDriveMount's refusal of a MANAGED mount
// whose allocation size a claim can't request — its own sentinel so the
// member is pointed at the size field, not the name field.
var errDriveAllocationInvalid = errors.New("this drive cannot be mounted on Kubernetes: its allocation is not a size a volume claim can request (ask an administrator to set the size on the allocation)")

// errDriveTargetInvalid: the mount's Target isn't runner.DriveTarget, the
// only in-container path a drive may bind at.
var errDriveTargetInvalid = errors.New("this drive cannot be mounted on Kubernetes: it did not arrive addressed to the reserved drive path; report the run to your administrator (no setting on the allocation produces this)")

var errDriveClaimTerminating = errors.New("your drive's volume claim is being deleted and cannot be mounted; wait for the deletion to finish, or ask an administrator whether it should have been deleted at all") // reuseDriveClaim's refusal; re-creating it would undo an operator's deliberate reclaim

var errDriveClaimVanished = errors.New("your drive's volume claim was deleted while your run was starting; start the run again") // ensureDrivePVC's refusal when a create race is lost twice; the next run's Get→Create self-heals

// errDriveClaimForeign: reuseDriveClaim's refusal of a claim whose IDENTITY
// labels say it belongs to a different member (driveClaimIdentity). The
// claim's NAME cannot prove that by itself: DriveObjectName's slug fold can
// collide two different (drive, home) pairs onto one name.
//
// DRAFT (M2 canon pending) — the frozen member sentence for this refusal.
var errDriveClaimForeign = errors.New("drive: your drive's volume is not the one allocated to you — ask an admin")

var errDriveBackendUnsupported = errors.New("k8s: this drive's backend is not a Kubernetes one and cannot be mounted on this substrate") // ensureDrivePVC's refusal of a drive whose backend belongs to another substrate; reaching here is a bug

// errSecondExec: Kubernetes ephemeral containers are ADD-ONLY, so a second
// Exec can't be honoured the way docker's re-exec is.
var errSecondExec = errors.New("k8s: exec: this sandbox already has an agent exec; a substrate second Exec on the same ref is not supported (ephemeral containers are add-only)")

// errBYOIUnsupported: BYOI's selftest-then-task double-exec hits the same
// add-only limit as errSecondExec.
var errBYOIUnsupported = errors.New("k8s: BYOI (wardyn-byoi/ images) is docker-only and is refused on this substrate: ephemeral containers cannot honour the selftest-then-task double-exec")

// errTeardownUnresolved: teardown found the pod but its wardyn.run-id label
// was missing/unparseable, so its sibling resources can't be located.
var errTeardownUnresolved = errors.New("k8s: teardown could not resolve run id from the sandbox's wardyn.run-id label; sibling proxy/netpols/secret may be orphaned")
