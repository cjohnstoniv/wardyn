// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The decision door's batch wire (#4063). A control plane that takes batches says so on every answer of its
// decision door; a proxy posts a batch only after reading that, and never infers it from a status.
const (
	DecisionBatchContentType = "application/vnd.wardyn.decision-batch+json"
	// DecisionBatchHeader is on every answer of a control plane's decision door that takes batches; its value
	// is the most records one batch may carry.
	DecisionBatchHeader = "Wardyn-Decision-Batch"
	// DecisionFinalHeader marks the last attempt at a batch: the proxy will not send it again.
	DecisionFinalHeader = "Wardyn-Decision-Final"
	MaxDecisionBatch    = 128
)

// DecisionRecord is one entry of a batch post.
type DecisionRecord struct {
	ID          uuid.UUID          `json:"id"`
	Time        time.Time          `json:"time"` // when the proxy queued it
	Decision    egress.DecisionLog `json:"decision"`
	CPContact   string             `json:"cp_contact,omitempty"` // "lost", or empty
	ContactAgeS int64              `json:"contact_age_s,omitempty"`
}

// DecisionBatch is the body of a batch post.
type DecisionBatch struct {
	Records []DecisionRecord `json:"records"`
}
