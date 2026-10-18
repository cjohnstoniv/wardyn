// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// RunContainment is whether a run's Kubernetes runner holds it as containment pending, and its node records.
type RunContainment = types.RunContainment

// RunNodeRecord is one node record and what the runner answered.
type RunNodeRecord = types.RunNodeRecord

// RunContainment reads a run's containment: the pending report (its tuple for security operators only)
// and the node records made for it.
func (c *Client) RunContainment(ctx context.Context, runID uuid.UUID) (RunContainment, error) {
	var out RunContainment
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID.String()+"/containment", nil, &out)
	return out, err
}

// RecordRunNode makes a security operator's node record for a run held as containment pending. A record
// that needs a second person returns *PendingApprovalError.
func (c *Client) RecordRunNode(ctx context.Context, runID uuid.UUID, req types.RunNodeRecordRequest) (RunNodeRecord, error) {
	var out RunNodeRecord
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+runID.String()+"/node-record", req, &out)
	return out, err
}
