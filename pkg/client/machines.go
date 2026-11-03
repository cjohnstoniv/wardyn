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

// RunMachineView is GET /api/v1/runs/{id}'s machine: the kept machine the run kept or resumed, read now.
// Every field but Mode is the machine's row as it is now, shared by every run that used it; HolderRunID
// names the run that holds it now or held it last (a stop does not clear it). A machine whose row is gone
// is {ID, Mode, Deleted}.
type RunMachineView struct {
	ID          uuid.UUID              `json:"id"`
	Name        string                 `json:"name,omitempty"`
	Mode        string                 `json:"mode,omitempty"` // new, resumed or fresh
	KeepAtEnd   bool                   `json:"keep_at_end,omitempty"`
	State       types.KeptMachineState `json:"state,omitempty"`
	Scrubbed    bool                   `json:"scrubbed"`
	RunnerName  string                 `json:"runner_name,omitempty"`
	HolderRunID *uuid.UUID             `json:"holder_run_id,omitempty"`
	Base        string                 `json:"base,omitempty"`
	DiskBytes   int64                  `json:"disk_bytes,omitempty"`
	ExpiresAt   *time.Time             `json:"expires_at,omitempty"`
	Deleted     bool                   `json:"deleted"`
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
	// DeleteReason says why a machine being deleted is: KeptMachineDeletedBy*. Empty while it is not.
	DeleteReason string `json:"delete_reason,omitempty"`
	// RunnerName is the name of the Mac the machine is kept on, filled by the server.
	RunnerName string `json:"runner_name,omitempty"`
}

// KeptMachine.DeleteReason values.
const (
	KeptMachineDeletedByPerson  = "person"
	KeptMachineDeletedByExpired = "expired"
	KeptMachineDeletedByAdmin   = "admin"
	KeptMachineDeletedByErasure = "erasure"
)

// MyMachines is GET /api/v1/me/machines: the caller's own kept machines, and whether they may keep another.
// Reason is the refusal code when Allowed is false: the site switch (kept_machine_site_off), their
// governance profile (governance_unit), no Mac of their own outside other people's pools
// (kept_machine_not_your_mac), or Count at Limit (kept_machine_person_limit, when resuming one still
// works). WorstBytes is the most disk one new kept machine can take.
type MyMachines struct {
	Machines   []KeptMachine `json:"machines"`
	Allowed    bool          `json:"allowed"`
	Reason     string        `json:"reason,omitempty"`
	Count      int           `json:"count"`
	Limit      int           `json:"limit"`
	WorstBytes int64         `json:"worst_bytes"`
	// IdleDays is the days a machine may sit unused before Wardyn deletes it: the site's setting, or its default.
	// Absent from a server that does not say.
	IdleDays int `json:"idle_days,omitempty"`
}

// KeptMachineList is GET /api/v1/machines/kept: one page of every person's kept machines, for an admin.
type KeptMachineList struct {
	Machines []KeptMachine `json:"machines"`
	Total    int           `json:"total"`
}

// KeptMachineDeleteResult is DELETE /api/v1/machines/kept/{id}'s answer: the machine and what its Mac
// did (deleted, already_absent, deferred while the Mac is offline, or record_only when the Mac is no
// longer registered).
type KeptMachineDeleteResult struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Owner    string    `json:"owner"`
	RunnerID uuid.UUID `json:"runner_id"`
	Outcome  string    `json:"outcome"`
}

// ListMyMachines lists the caller's own kept machines (GET /api/v1/me/machines).
func (c *Client) ListMyMachines(ctx context.Context) (MyMachines, error) {
	var out MyMachines
	err := c.do(ctx, http.MethodGet, "/api/v1/me/machines", nil, &out)
	return out, err
}

// DeleteMyMachine deletes one of the caller's own kept machines from its Mac
// (DELETE /api/v1/me/machines/{id}).
func (c *Client) DeleteMyMachine(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/machines/"+id.String(), nil, nil)
}

// GetRunMachine reads the kept machine a run kept or resumed from GET /api/v1/runs/{id}: nil for a run
// that keeps none.
func (c *Client) GetRunMachine(ctx context.Context, runID uuid.UUID) (*RunMachineView, error) {
	var out struct {
		Machine *RunMachineView `json:"machine"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID.String(), nil, &out)
	return out.Machine, err
}

// ListKeptMachines returns one page of every person's kept machines
// (GET /api/v1/machines/kept, super admin).
func (c *Client) ListKeptMachines(ctx context.Context, opts ListOpts) (KeptMachineList, error) {
	var out KeptMachineList
	err := c.do(ctx, http.MethodGet, appendListOpts("/api/v1/machines/kept", []ListOpts{opts}), nil, &out)
	return out, err
}

// DeleteKeptMachine deletes one person's kept machine and its files (DELETE /api/v1/machines/kept/{id},
// super admin). Outcome is deferred while the Mac is offline (a 202).
func (c *Client) DeleteKeptMachine(ctx context.Context, id uuid.UUID) (KeptMachineDeleteResult, error) {
	var out KeptMachineDeleteResult
	err := c.do(ctx, http.MethodDelete, "/api/v1/machines/kept/"+id.String(), nil, &out)
	return out, err
}
