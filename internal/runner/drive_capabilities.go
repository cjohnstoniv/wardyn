// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DriveProbeResult is the closed set of answers a DriveProber gives about one
// resolved drive mount. Three states, not a bool, because "I checked and it
// is fine" and "I could not tell" are different claims with different
// remedies — see DriveProbeUnknown.
type DriveProbeResult string

const (
	DriveProbeReadable   DriveProbeResult = "readable"   // probe ran AS THE AGENT'S OWN UID (never root) and could read the mount
	DriveProbeUnreadable DriveProbeResult = "unreadable" // agent uid could NOT read it — the failure a daemon-side root os.Stat cannot see
	// DriveProbeUnknown: the probe could not be run to a conclusion (e.g. k8s
	// can only Get the claim's phase). A caller MUST NOT treat Unknown as
	// DriveProbeReadable.
	DriveProbeUnknown DriveProbeResult = "unknown"
)

// DriveProbe is a DriveProber's answer for one resolved mount.
type DriveProbe struct {
	Result DriveProbeResult
	Detail string // operator-facing context; never shown to the member
}

// DriveProber is an OPTIONAL Runner capability, modelled on ImageChecker: a
// substrate that can ask whether the SANDBOX'S OWN USER — not the daemon's
// own root process — can actually read a resolved user-drive mount (a
// daemon-side os.Stat always runs as root and misses this).
type DriveProber interface {
	// ProbeDrive answers whether mount would be readable by the sandbox's
	// uid, bounded by ctx. Called BEFORE a sandbox exists and MUST honour
	// ctx's deadline.
	ProbeDrive(ctx context.Context, mount types.DriveMount) (DriveProbe, error)
}

// SubstrateState is the closed set of answers a SubstrateProber gives about
// whether the control plane can use its substrate right now. Three failure
// classes, not one bool, because the remedies differ: a substrate that cannot
// be reached is a network or daemon fault, a refused credential is a token to
// renew, and a refused verb is a missing grant.
type SubstrateState string

const (
	SubstrateOK           SubstrateState = "ok"
	SubstrateUnreachable  SubstrateState = "unreachable"  // no answer, a transport error, or the probe's deadline
	SubstrateUnauthorized SubstrateState = "unauthorized" // the substrate refused the credential (401)
	SubstrateForbidden    SubstrateState = "forbidden"    // the credential is valid but may not do this (403, a permission error)
)

// SubstrateProber is an OPTIONAL Runner capability, in the shape of
// DriveProber: a cheap read-only call that proves the control plane can reach
// and use its substrate (Kubernetes: one namespaced pod list; Docker: a daemon
// ping). It never creates anything. The implementation classifies its own
// errors, and MUST honour ctx's deadline; a ctx that ended is SubstrateUnreachable.
type SubstrateProber interface {
	ProbeSubstrate(ctx context.Context) SubstrateState
}

// DriveReclaimOutcome is the closed set of answers a DriveReclaimer gives for
// one object it was asked to destroy. The second is not an error: the object
// being gone already is the same END STATE the caller asked for. Telling the
// two apart matters only to the audit row.
type DriveReclaimOutcome string

const (
	DriveReclaimDeleted DriveReclaimOutcome = "deleted" // this call issued the delete and the substrate accepted it
	// DriveReclaimAlreadyAbsent: no object answered to that name. Not an
	// error, but never reported as "deleted" — an operator must be able to
	// trust that row.
	DriveReclaimAlreadyAbsent DriveReclaimOutcome = "already_absent"
)

// ErrDriveInUse is the sentinel a DriveReclaimer returns when a sandbox still
// holds the object; the caller answers 409. A refusal rather than a
// force-delete: a forced remove would pull the volume out from under a live
// agent mid-write.
var ErrDriveInUse = errors.New("runner: the drive's storage is still held by a running sandbox")

// ErrDriveNotReclaimable is the sentinel a DriveReclaimer returns when the
// object answering to the drive's name is NOT the storage it allocated — a
// reclaim that gets this wrong deletes another member's files.
var ErrDriveNotReclaimable = errors.New("runner: the object under this drive's name is not the storage it allocated")

// DriveReclaimer is an OPTIONAL Runner capability, modelled on ImageRemover: a
// substrate that can DESTROY the per-person storage object a user drive
// allocated. The one irreversible verb in this file; a drive OUTLIVES every
// run that mounts it, so no automatic reclaim path may exist. On Kubernetes
// the chart's Role gains `delete` only under `drives.reclaim.enabled`.
type DriveReclaimer interface {
	// ReclaimDrive destroys the storage object mount names, bounded by ctx. MUST refuse rather than destroy when the
	// object is not this drive's (ErrDriveNotReclaimable) or is still held by a sandbox (ErrDriveInUse), and MUST
	// answer DriveReclaimAlreadyAbsent, not an error, when nothing answers to the name.
	ReclaimDrive(ctx context.Context, mount types.DriveMount) (DriveReclaimOutcome, error)
}

// ErrDriveLifecycleUnavailable is the sentinel a DriveLifecycleChecker returns
// (and the orchestrator returns when no substrate can check) when this
// deployment could not later destroy a drive it created.
var ErrDriveLifecycleUnavailable = errors.New("runner: this substrate cannot delete the drives it would create")

// DriveLifecycleChecker is an OPTIONAL Runner capability beside DriveReclaimer:
// whether this substrate can both create and destroy a managed drive object of
// its own backend. nil means yes. Member drive creation asks it first, so no
// object is ever created that DriveReclaimer couldn't remove. Any other answer
// wraps ErrDriveLifecycleUnavailable.
type DriveLifecycleChecker interface {
	CanManageDrives(ctx context.Context) error
}
