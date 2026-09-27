// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// Device is one enrolled laptop daemon: an inventory row binding a revocable
// bearer credential to that device's own audit-federation progress.
// CredentialSHA256 never reaches a row unhashed; a revoked credential's WHERE
// RevokedAt IS NULL makes it answer exactly like an unknown one (no oracle).
// LastSeq/LastRowHash are the chain cursor as ingested so far, verified
// against each new batch's claimed PrevHash before advancing atomically.
type Device struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Never serialized: a credential hash has no business leaving the process.
	CredentialSHA256 string     `json:"-"`
	EnrolledBy       string     `json:"enrolled_by"`
	CreatedAt        time.Time  `json:"created_at"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	LastSeq          int64      `json:"last_seq"`
	LastRowHash      string     `json:"last_row_hash,omitempty"`
}

// DeviceEnrolmentToken is a single-use admin-minted token a laptop's first
// boot exchanges for a Device credential. ConsumeEnrolmentToken's conditional
// UPDATE ... RETURNING (WHERE ConsumedAt IS NULL) makes redemption single-use.
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

// FederatedAuditEvent is one audit row plus Seq, its local table's own
// sequence for the row. Seq alone is not identity — a table reset can
// restart it — so a replay is recognised by seq together with RowHash.
type FederatedAuditEvent struct {
	AuditEvent
	Seq int64 `json:"seq"`
}

// DeviceEnrolRequest/DeviceEnrolResponse are POST /devices/enrol's wire
// shapes; DeviceAck is the ingest/heartbeat routes' recorded-cursor answer.
type DeviceEnrolRequest struct {
	Token string `json:"token"`
}

type DeviceEnrolResponse struct {
	DeviceID uuid.UUID `json:"device_id"`
	Name     string    `json:"name"`
	Token    string    `json:"token"`
}

type DeviceAck struct {
	AckedSeq int64 `json:"acked_seq"`
}
