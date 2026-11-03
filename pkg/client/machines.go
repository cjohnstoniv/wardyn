// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunMachine is CreateRunRequest.Machine: a macOS VM run on the person's own Mac that keeps its machine
// when it stops, or starts from one kept earlier. Keep and Resume are exclusive.
type RunMachine struct {
	// Keep keeps this run's machine when the run stops. Name is its name (the caller's own, unique);
	// empty lets the server pick one.
	Keep bool   `json:"keep,omitempty"`
	Name string `json:"name,omitempty"`
	// Resume is the kept machine this run starts from, by id or name.
	Resume string `json:"resume,omitempty"`
	// Fresh, with Resume, resets the machine to the base before the run starts.
	Fresh bool `json:"fresh,omitempty"`
}

// KeptMachine is one kept machine as GET /api/v1/me/machines answers it: the organisation's record, never
// the machine's content.
type KeptMachine struct {
	ID             uuid.UUID              `json:"id"`
	Name           string                 `json:"name"`
	OwnerSubject   string                 `json:"owner_subject"`
	RunnerID       uuid.UUID              `json:"runner_id"`
	Base           string                 `json:"base"`
	BasePreparedAt time.Time              `json:"base_prepared_at"`
	State          types.KeptMachineState `json:"state"`
	RunID          *uuid.UUID             `json:"run_id,omitempty"`
	Scrubbed       bool                   `json:"scrubbed"`
	DiskBytes      int64                  `json:"disk_bytes"`
	CreatedAt      time.Time              `json:"created_at"`
	LastUsedAt     time.Time              `json:"last_used_at"`
	ExpiresAt      time.Time              `json:"expires_at"`
	// DeleteStartedAt is set once a delete has started and is still finishing.
	DeleteStartedAt *time.Time `json:"delete_started_at,omitempty"`
}

// ListMyMachines lists the caller's own kept machines (GET /api/v1/me/machines).
func (c *Client) ListMyMachines(ctx context.Context) ([]KeptMachine, error) {
	var out []KeptMachine
	err := c.do(ctx, http.MethodGet, "/api/v1/me/machines", nil, &out)
	return out, err
}

// DeleteMyMachine deletes one of the caller's own kept machines from its Mac
// (DELETE /api/v1/me/machines/{id}).
func (c *Client) DeleteMyMachine(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/machines/"+id.String(), nil, nil)
}
