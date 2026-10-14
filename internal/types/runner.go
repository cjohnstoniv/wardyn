// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// RunnerState is where a registered runner is in the claim machine.
type RunnerState string

const (
	// RunnerUnclaimed: registered with a token; offered no run and relays nothing.
	RunnerUnclaimed RunnerState = "unclaimed"
	// RunnerClaimed: the token's owner proved they hold the key.
	RunnerClaimed RunnerState = "claimed"
	// RunnerRevoked is terminal; the key is never reused.
	RunnerRevoked RunnerState = "revoked"
)

// RunnerPostureSourceRunnerAsserted is the only posture source in 0.9: the runner said so.
const RunnerPostureSourceRunnerAsserted = "runner_asserted"

// RunnerPosture is what a runner reported about its device. Self-reported: it
// separates a lost or compromised laptop from a healthy one and does not
// constrain a root developer.
type RunnerPosture struct {
	MDMManaged    bool   `json:"mdm_managed"`
	DiskEncrypted bool   `json:"disk_encrypted"`
	OS            string `json:"os"`
	OSVersion     string `json:"os_version"`
	// Evidence is where the runner read mdm_managed and disk_encrypted from: a
	// root-owned file the device's MDM wrote, else its own probe of the OS.
	// Either way the org records the posture as runner_asserted.
	Evidence string `json:"evidence"`
}

// RunnerPosture.Evidence values.
const (
	PostureEvidenceMDMFile = "mdm_file"
	PostureEvidenceOSProbe = "os_probe"
)

// Runner is one registered wardyn-runnerd. Owner is a person's principal, so the
// row is personal data.
type Runner struct {
	ID           uuid.UUID `json:"id"`
	Owner        string    `json:"owner"`
	Name         string    `json:"name"`
	OrgURLSHA256 string    `json:"-"`
	// MintedBy is who minted the registration token this runner redeemed; empty for a runner
	// registered before it was recorded.
	MintedBy string `json:"-"`
	// PublicKey is the runner's Ed25519 key. The private half never leaves the runner.
	PublicKey      []byte        `json:"-"`
	KeyFingerprint string        `json:"key_fingerprint"`
	State          RunnerState   `json:"state"`
	Version        string        `json:"version,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	ClaimedAt      *time.Time    `json:"claimed_at,omitempty"`
	LastSeenAt     *time.Time    `json:"last_seen_at,omitempty"`
	RevokedAt      *time.Time    `json:"revoked_at,omitempty"`
	Posture        RunnerPosture `json:"posture"`
	// PostureReportedAt is nil until the runner reports; PostureSource is then runner_asserted.
	PostureReportedAt *time.Time `json:"posture_reported_at,omitempty"`
	PostureSource     string     `json:"posture_source,omitempty"`
	// RevokedBy is the principal that revoked the runner; RevokeReason is "" or
	// RunnerRevokeNotMine. Both are empty until it is revoked.
	RevokedBy    string `json:"-"`
	RevokeReason string `json:"-"`
	// MaxRuns is how many sandbox-holding runs placement may put on the runner at once: the runner's
	// "Runs at once". Its owner sets it; the stored value is the one placement holds.
	MaxRuns int `json:"max_runs"`
	// Limits is the runner's declared limit set (its RunsAtOnce is MaxRuns), nil while its owner has not
	// set them all (0174): such a runner takes no run.
	Limits *RunnerLimits `json:"-"`
	// MachineName is the host name the runner last reported (at registration and on each connect):
	// runner-reported, never an identity, "" until it reports one.
	MachineName string `json:"machine_name"`
}

// MachineNameMax bounds a reported machine name: the longest DNS host name.
const MachineNameMax = 253

// ValidMachineName reports whether s may be stored as a runner's machine name: at most MachineNameMax
// bytes of printable characters. "" (none reported) is valid.
func ValidMachineName(s string) bool {
	return len(s) <= MachineNameMax && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) })
}

// RunnerRevokeNotMine marks an owner's revoke of a runner they did not register.
const RunnerRevokeNotMine = "not_mine"

// RunnerActionKind is a pending action's verb.
type RunnerActionKind string

const (
	RunnerActionRetire             RunnerActionKind = "retire_run"
	RunnerActionKill               RunnerActionKind = "kill"
	RunnerActionEnd                RunnerActionKind = "end"
	RunnerActionStopProxy          RunnerActionKind = "stop_proxy"
	RunnerActionRecoverMutation    RunnerActionKind = "recover_mutation"
	RunnerActionQuarantine         RunnerActionKind = "quarantine_run"
	RunnerActionRecoverContainment RunnerActionKind = "recover_containment"
)

// RunnerQuarantine identifies an org-authorized isolation disposition, not an
// evidence acknowledgement or permission to reconstruct missing run authority.
// Asserted marks a quarantine of a run the org has no row for: Generation is
// then the runner's own assertion, never compared to org executions, and the
// runner deletes that run's local evidence after RunnerAssertedEvidenceTTL or
// at revocation.
type RunnerQuarantine struct {
	ID         uuid.UUID `json:"id"`
	Generation uuid.UUID `json:"generation"`
	Asserted   bool      `json:"asserted,omitempty"`
}

// RunnerAssertedEvidenceTTL is how long quarantined evidence of an asserted
// (org-erased) run is kept, on the runner and in the org's action rows, after
// its quarantine succeeded.
const RunnerAssertedEvidenceTTL = 90 * 24 * time.Hour

// RunnerAssertedSubjectCap bounds the asserted subjects one runner can have,
// settled or not. Past it the runner is held not-ready.
const RunnerAssertedSubjectCap = 100

// RunnerMutationFailure is an immutable failure identity. Contained becomes true
// only after proxy containment and lifecycle persistence both succeed.
type RunnerMutationFailure struct {
	ID         uuid.UUID `json:"id"`
	Generation uuid.UUID `json:"generation"`
	Operation  string    `json:"operation"`
	Contained  bool      `json:"contained"`
}

// RunnerPendingAction is applied before readiness when a runner reconnects.
type RunnerPendingAction struct {
	Retirement *RunnerRetirement      `json:"retirement,omitempty"`
	Quarantine *RunnerQuarantine      `json:"quarantine,omitempty"`
	Recovery   *RunnerMutationFailure `json:"recovery,omitempty"`
	ID         uuid.UUID              `json:"id"`
	RunnerID   uuid.UUID              `json:"runner_id"`
	RunID      uuid.UUID              `json:"run_id"`
	Kind       RunnerActionKind       `json:"kind"`
	Ref        string                 `json:"ref"`
	QueuedAt   time.Time              `json:"queued_at"`
	AppliedAt  *time.Time             `json:"applied_at,omitempty"`
	Outcome    string                 `json:"outcome,omitempty"`
	ObservedAt *time.Time             `json:"observed_at,omitempty"`
	// Bound, Owner, Generation and LostAt are the org identity persisted when
	// the action was queued. A row that is not Bound predates that binding and
	// grants nothing. On a Bound row a nil Generation means no execution had
	// been issued; LostAt is set for stop_proxy only.
	Bound      bool       `json:"-"`
	Owner      string     `json:"-"`
	Generation uuid.UUID  `json:"-"`
	LostAt     *time.Time `json:"-"`
	// Asserted marks a subject with no org run row: RunID is then the
	// runner's assertion, Owner is empty and Generation is nil.
	Asserted bool `json:"-"`
	// ObligationID is the chain a deferred action's successors continue.
	ObligationID uuid.UUID `json:"-"`
}

// Runner action outcomes an action_result may carry. A deferred receipt is
// final for its action ID but does not settle the obligation it carried.
const (
	RunnerActionSuccess  = "success"
	RunnerActionDeferred = "deferred"
	// RunnerActionRefused closes an action the runner can never apply.
	RunnerActionRefused = "refused"
)

// CredentialDeliveryPolicyDoc is the stored local_credential_delivery document.
// Policy is the JSON of placement.DeliveryPolicy, validated by the writer; the
// store keeps it opaque.
type CredentialDeliveryPolicyDoc struct {
	Policy    json.RawMessage `json:"policy"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

// GrantDelivery is how one grant reached a run (credential_grants.delivery).
type GrantDelivery string

const (
	GrantDeliveryNone     GrantDelivery = ""
	GrantDeliveryOwn      GrantDelivery = "own"
	GrantDeliveryViaOrg   GrantDelivery = "via_org"
	GrantDeliveryResident GrantDelivery = "runner_resident"
)
