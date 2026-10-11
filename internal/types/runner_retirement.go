// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "github.com/google/uuid"

// RunnerSpoolReceipt identifies the durable accepted prefix including overflow.
type RunnerSpoolReceipt struct {
	SpoolID       uuid.UUID `json:"spool_id"`
	Seq           int64     `json:"seq"`
	Hash          string    `json:"hash"`
	ReportedDrops uint64    `json:"reported_drops"`
}

// RunnerRetirement is an org-issued settlement receipt, never authority inferred
// from a runner's staged exit ACK. Its action separately binds runner/run/ref.
type RunnerRetirement struct {
	ID         uuid.UUID          `json:"id"`
	Generation uuid.UUID          `json:"generation"`
	ExecID     string             `json:"exec_id"`
	ExitHash   string             `json:"exit_hash"`
	OutputEnd  int64              `json:"output_end"`
	Spool      RunnerSpoolReceipt `json:"spool"`
}
