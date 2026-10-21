// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// RunnerFilter is the state filter of the administrators' runner list; an empty one is active.
type RunnerFilter string

const (
	RunnerFilterActive  RunnerFilter = "active"
	RunnerFilterRevoked RunnerFilter = "revoked"
	RunnerFilterAll     RunnerFilter = "all"
)

// RunnerUnclaimedTTL is how long an unclaimed runner waits for its owner.
const RunnerUnclaimedTTL = 24 * time.Hour

// RunnerCapacity is the per-run ceiling a runner advertised: the wire shape of its caps event.
type RunnerCapacity struct {
	CPUMillisMax int64 `json:"cpu_millis_max"`
	MemoryMiBMax int64 `json:"memory_mib_max"`
}

// RunnerView is a runner as the management routes show it to a person or an administrator. It is
// never sent to a runner. Every posture fact is what the runner said about itself.
type RunnerView struct {
	ID    uuid.UUID   `json:"id"`
	Name  string      `json:"name"`
	State RunnerState `json:"state"`
	// Owner is set on the administrators' routes only; the caller's own list omits it.
	Owner string `json:"owner,omitempty"`
	// MintedBy names who minted the registration token this runner redeemed. MintedByEmail is that
	// person's email where Wardyn holds one, set on a person's own routes when someone else minted it,
	// so the console can say who without printing an opaque sign-in subject.
	MintedBy      string `json:"minted_by,omitempty"`
	MintedByEmail string `json:"minted_by_email,omitempty"`
	// Online is the hub's live view of a claimed runner and false for any other.
	Online bool `json:"online"`
	// KeyFingerprint is abbreviated for an unclaimed runner on its owner's own routes, so it cannot
	// be copied from there into the claim; KeyFingerprintAbbreviated says so.
	KeyFingerprint            string    `json:"key_fingerprint"`
	KeyFingerprintAbbreviated bool      `json:"key_fingerprint_abbreviated"`
	Version                   string    `json:"version,omitempty"`
	CreatedAt                 time.Time `json:"created_at"`
	// ClaimExpiresAt is when an unclaimed runner lapses; absent for any other state.
	ClaimExpiresAt *time.Time `json:"claim_expires_at,omitempty"`
	ClaimedAt      *time.Time `json:"claimed_at,omitempty"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	// RevokedBy names who revoked the runner; RevokeReason is "" or RunnerRevokeNotMine. Both are
	// absent until it is revoked.
	RevokedBy    string `json:"revoked_by,omitempty"`
	RevokeReason string `json:"revoke_reason,omitempty"`
	// RevokedByEmail is that person's email where Wardyn holds one, set on a person's own list only.
	RevokedByEmail string `json:"revoked_by_email,omitempty"`
	// Posture is absent until the runner has reported; PostureSource is then runner_asserted.
	Posture           *RunnerPosture `json:"posture,omitempty"`
	PostureReportedAt *time.Time     `json:"posture_reported_at,omitempty"`
	PostureSource     string         `json:"posture_source,omitempty"`
	// Capacity, Classes and AllowedRoots are the claimed runner's last caps event, all runner_asserted.
	// They are read from this process's live endpoint, so like Online they are absent when the runner
	// has not connected to this replica, never a stored or guessed value.
	Capacity     *RunnerCapacity    `json:"capacity,omitempty"`
	Classes      []ConfinementClass `json:"classes,omitempty"`
	AllowedRoots []string           `json:"allowed_roots,omitempty"`
	// Folders says the runner shares folders on its computer with runs elsewhere (a folder-only
	// runner, 0.10 on), read like Capacity.
	Folders bool `json:"folders,omitempty"`
	// Kind is always RunnerKindSelfHosted: the one shape both kinds' reads share.
	Kind RunnerKind `json:"kind"`
	// Limits is what the runner's owner declared, which only they change; nil when not all of them are
	// set, and then the runner takes no run. Usage is what its runs hold now, the figures placement
	// counts, shown to its owner and to operators only.
	Limits *RunnerLimits `json:"limits"`
	Usage  RunnerUsage   `json:"usage"`
	// MachineName is the host name the runner last reported at registration or on connect. It is what
	// the runner says, never verified and never an identity; absent until it reports one.
	MachineName string `json:"machine_name,omitempty"`
	// Arch is the CPU architecture of the runner's Docker engine ("amd64" or "arm64") as last reported;
	// runner-reported, absent until it reports one.
	Arch string `json:"arch,omitempty"`
	// OwnerKind and TrustClass are the stored values (migration 0181), on every route. TrustSetBy and
	// TrustSetAt name the last writer, on the administrators' routes only.
	OwnerKind  RunnerOwnerKind  `json:"owner_kind"`
	TrustClass RunnerTrustClass `json:"trust_class"`
	TrustSetBy string           `json:"trust_set_by,omitempty"`
	TrustSetAt *time.Time       `json:"trust_set_at,omitempty"`
	// Pools are the owner's live Self-Hosted pools the runner is a member of, by name; [] for none.
	Pools []RunnerPoolRef `json:"pools"`
	// OfflineSummary is the runner's last report of why it could not reach the organisation, and
	// RelayStatus its last report on its run relay; both runner_asserted, set on a one-runner read
	// only, and absent until a runner that sends them has.
	OfflineSummary *RunnerOfflineSummary `json:"offline_summary,omitempty"`
	RelayStatus    *RunnerRelayStatus    `json:"relay_status,omitempty"`
}

// MaxRunnerReason bounds the reason of an offline summary or a relay status, in bytes, and
// MaxRunnerOfflineSummaryBytes one encoded summary.
const (
	MaxRunnerReason              = 256
	MaxRunnerOfflineSummaryBytes = 1024
)

// The stages an offline summary names: the `wardyn-runnerd doctor` stage names of the network stages a
// connection fails at and of the engine stages that refuse serve's start.
const (
	RunnerStageDNS        = "dns"
	RunnerStageProxy      = "proxy"
	RunnerStageTLS        = "tls"
	RunnerStageHealthz    = "healthz"
	RunnerStageIdentity   = "identity"
	RunnerStageEngine     = "engine"
	RunnerStageProxyImage = "proxy-image"
	RunnerStageRelay      = "relay"
)

// RunnerStages is the closed list of those names.
var RunnerStages = []string{RunnerStageDNS, RunnerStageProxy, RunnerStageTLS, RunnerStageHealthz, RunnerStageIdentity,
	RunnerStageEngine, RunnerStageProxyImage, RunnerStageRelay}

// RunnerOfflineSummary is what a runner sends once it connects again after it could not: the first and
// last failure since its last session, the stage that failed (a `wardyn-runnerd doctor` stage name) and
// a short reason with any credential removed. ReportedAt is the organisation's receipt time.
type RunnerOfflineSummary struct {
	Stage          string    `json:"stage"`
	Reason         string    `json:"reason"`
	FirstFailureAt time.Time `json:"first_failure_at"`
	LastFailureAt  time.Time `json:"last_failure_at"`
	ReportedAt     time.Time `json:"reported_at,omitzero"`
}

// ClipRunnerReason is s on one line, printable, cut to MaxRunnerReason bytes at a character boundary. It
// removes no credential: the caller scrubs first.
func ClipRunnerReason(s string) string {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "?")), " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, s)
	for len(s) > MaxRunnerReason {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// Relay states a runner reports.
const (
	RelayReachable   = "reachable"
	RelayUnreachable = "unreachable"
)

// RunnerRelayStatus is a Docker runner's last check of its run relay: State is RelayReachable or
// RelayUnreachable, Since when it became so, Reason why it is unreachable ("" when reachable).
// ReportedAt is the organisation's receipt time.
type RunnerRelayStatus struct {
	State      string    `json:"state"`
	Since      time.Time `json:"since"`
	Reason     string    `json:"reason,omitempty"`
	ReportedAt time.Time `json:"reported_at,omitzero"`
}

// RunnerPoolRef names one pool a runner is a member of.
type RunnerPoolRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// RunnerSettingsRequest turns runners on or off. Enabled is required, so an empty body never
// switches anything.
type RunnerSettingsRequest struct {
	Enabled *bool `json:"enabled"`
}

// RunnerUpdateRequest is an owner's change to their own runner (PUT /me/runners/{id}): the whole limit
// set, required, so an empty body changes nothing.
type RunnerUpdateRequest struct {
	Limits *RunnerLimits `json:"limits"`
}
