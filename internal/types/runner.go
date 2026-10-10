// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"time"

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
}

// Runner is one registered wardyn-runnerd. Owner is a person's principal, so the
// row is personal data.
type Runner struct {
	ID           uuid.UUID `json:"id"`
	Owner        string    `json:"owner"`
	Name         string    `json:"name"`
	OrgURLSHA256 string    `json:"-"`
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
}

// RunnerActionKind is a pending action's verb.
type RunnerActionKind string

const (
	RunnerActionKill      RunnerActionKind = "kill"
	RunnerActionEnd       RunnerActionKind = "end"
	RunnerActionStopProxy RunnerActionKind = "stop_proxy"
)

// RunnerPendingAction is an action queued for a runner that is offline, applied
// first when it reconnects.
type RunnerPendingAction struct {
	ID         uuid.UUID        `json:"id"`
	RunnerID   uuid.UUID        `json:"runner_id"`
	RunID      uuid.UUID        `json:"run_id"`
	Kind       RunnerActionKind `json:"kind"`
	Ref        string           `json:"ref"`
	QueuedAt   time.Time        `json:"queued_at"`
	AppliedAt  *time.Time       `json:"applied_at,omitempty"`
	Outcome    string           `json:"outcome,omitempty"`
	ObservedAt *time.Time       `json:"observed_at,omitempty"`
}

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
