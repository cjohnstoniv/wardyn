// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// Device is one enrolled laptop daemon: an organisation-side inventory row
// binding a revocable bearer credential to that device's own audit-federation
// progress (docs/design/0.8/PLAN.md, "Hybrid enrolment and audit
// federation"). CredentialSHA256 is hex(sha256(raw token)) — the raw device
// bearer never reaches a row (see store.hashToken); GetDeviceByRaw hashes the
// presented bearer before every lookup, and its WHERE clause requires
// RevokedAt IS NULL so a revoked credential answers exactly like an unknown
// one — no oracle.
//
// LastSeq/LastRowHash are this device's chain cursor AS INGESTED by the
// organisation so far — never the device's own local table, which the
// organisation never reads directly. IngestDeviceAudit verifies each new
// batch's first row's claimed PrevHash against LastRowHash before accepting
// anything, and advances both atomically with the insert.
type Device struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Never serialized: the inventory is an admin read, and a credential hash
	// has no business leaving the process any more than api_tokens' does.
	CredentialSHA256 string     `json:"-"`
	EnrolledBy       string     `json:"enrolled_by"`
	CreatedAt        time.Time  `json:"created_at"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	LastSeq          int64      `json:"last_seq"`
	LastRowHash      string     `json:"last_row_hash,omitempty"`
}

// DeviceEnrolmentToken is a single-use admin-minted token a laptop's first
// boot exchanges for a Device credential. TokenSHA256 follows the same
// hash-at-rest rule as every other bearer this tree mints (attach_tickets,
// api_tokens). ConsumedAt is set by ConsumeEnrolmentToken's conditional
// UPDATE ... RETURNING, which is what makes a second redemption impossible:
// its WHERE clause requires ConsumedAt IS NULL.
type DeviceEnrolmentToken struct {
	ID          uuid.UUID  `json:"id"`
	TokenSHA256 string     `json:"-"`
	DeviceName  string     `json:"device_name"`
	MintedBy    string     `json:"minted_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ConsumedAt  *time.Time `json:"consumed_at,omitempty"`
	Token       string     `json:"token,omitempty"` // plaintext, mint response ONLY
}

// FederatedAuditEvent is one audit row as a device's forwarder submits it
// upward: the device's own AuditEvent — already chained on the DEVICE's own
// local audit_events table, so PrevHash/RowHash are the link ITS trigger
// computed, not the organisation's — plus Seq, that same local table's own
// seq for the row.
//
// Seq is what idempotency rides on (store.PG.IngestDeviceAudit and
// ListAuditEventsAfterSeq): a device's audit_events.id is not unique across a
// retried push (the forwarder re-reads and re-sends whatever is at or after
// its durable cursor), but its local seq is monotonic and gapless, so it is
// the only field a replayed batch can be deduplicated against.
type FederatedAuditEvent struct {
	AuditEvent
	Seq int64 `json:"seq"`
}
