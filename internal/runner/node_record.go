// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// NodeRecordAcceptor is an OPTIONAL capability: check an operator's node record against the run's
// own record and accept it as an infrastructure fence, or refuse it with the reason.
type NodeRecordAcceptor interface {
	AcceptNodeRecord(ctx context.Context, ref string, rec NodeRecord) error
}

// Node-record attestations (design K1 §3.1 (b)).
const (
	AttestNodePoweredOff = "node_powered_off"
	AttestNoPriorHolder  = "no_prior_holder"
)

// NodeRecordApprover is one approver of a node record as the org recorded it.
type NodeRecordApprover struct {
	Principal string `json:"principal"`
	Email     string `json:"email,omitempty"`
	// Human is false for an admin token, which can never make the record.
	Human bool `json:"human"`
}

// ErrNodeRecordRefused marks a node record the runner refused for good: its reason is final for
// that record, which a new record (with fresh approvals) can replace.
var ErrNodeRecordRefused = errors.New("node record refused")

// NodeRecord is a security operator's audited record that the machines which may have run a run's
// pods are powered off, or that no other machine held the name.
type NodeRecord struct {
	ID          uuid.UUID            `json:"id"`
	Attestation string               `json:"attestation"`
	Tuple       NodeTuple            `json:"tuple"`
	Approvers   []NodeRecordApprover `json:"approvers"`
	// FourEyes is the org's four-eyes setting for security-operator writes when the record was made.
	FourEyes bool   `json:"four_eyes"`
	Source   string `json:"source"`
	SourceID string `json:"source_id"`
}
