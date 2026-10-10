// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runner pool contract (0.9). A pool is an organisation-defined set of
// execution targets; choosing one constrains where a run may go and grants
// nothing. A server that cannot honour pools yet answers every method below,
// and a CreateRunRequest.RunnerPoolID, with a refusal rather than ignoring it.

// The canonical pool shapes, re-exported so SDK callers can name them.
type (
	RunnerPool          = types.RunnerPool
	RunnerPoolHosting   = types.RunnerPoolHosting
	RunnerPoolState     = types.RunnerPoolState
	RunnerPoolMember    = types.RunnerPoolMember
	RunnerPoolDefaults  = types.RunnerPoolDefaults
	RunnerPoolSelection = types.RunnerPoolSelection
	ResolvedRunnerPool  = types.ResolvedRunnerPool
	RunnerPoolSubject   = types.RunnerPoolSubject
	RunnerPoolUsePolicy = types.RunnerPoolUsePolicy
	// The pool's own limits and what a run of each type effectively gets.
	RunnerPoolLimits            = types.RunnerPoolLimits
	RunnerPoolBackgroundLimits  = types.RunnerPoolBackgroundLimits
	RunnerPoolInteractiveLimits = types.RunnerPoolInteractiveLimits
	RunnerPoolAmount            = types.RunnerPoolAmount
	RunnerPoolDuration          = types.RunnerPoolDuration
	RunnerPoolRunType           = types.RunnerPoolRunType
	EffectiveRunLimits          = types.EffectiveRunLimits
	EffectiveLimit              = types.EffectiveLimit
	LimitAmount                 = types.LimitAmount
	LimitSource                 = types.LimitSource
	ConcurrencyBound            = types.ConcurrencyBound
	RunEndReason                = types.RunEndReason
	// RunnerPoolReason is a pool selection refusal reason; it matches APIError.Reason.
	RunnerPoolReason = runnerpool.Reason
)

// The hosting types, lifecycle states and selection provenance values.
const (
	RunnerPoolRemoteProvided = types.RunnerPoolRemoteProvided
	RunnerPoolSelfHosted     = types.RunnerPoolSelfHosted

	RunnerPoolActive   = types.RunnerPoolActive
	RunnerPoolDisabled = types.RunnerPoolDisabled
	RunnerPoolDeleted  = types.RunnerPoolDeleted

	RunnerPoolSelectedExplicit = types.RunnerPoolSelectedExplicit
	RunnerPoolSelectedPersonal = types.RunnerPoolSelectedPersonal
	RunnerPoolSelectedOrg      = types.RunnerPoolSelectedOrg

	RunnerPoolRunBackground  = types.RunnerPoolRunBackground
	RunnerPoolRunInteractive = types.RunnerPoolRunInteractive

	LimitSourcePool           = types.LimitSourcePool
	LimitSourceGovernance     = types.LimitSourceGovernance
	LimitSourceDeployment     = types.LimitSourceDeployment
	LimitSourceRunnerCapacity = types.LimitSourceRunnerCapacity

	// RunEndMaxLifetimeReached is the reason a run is KILLED at its maximum lifetime.
	RunEndMaxLifetimeReached = types.RunEndMaxLifetimeReached
)

// Whether a permitted pool can take a run right now.
const (
	RunnerPoolAvailable   = "available"
	RunnerPoolUnavailable = "unavailable"
	RunnerPoolUnknown     = "unknown" // the answer could not be read; never read as available
)

// RunnerPoolChoice is one pool a person may choose, as the preview and the pool
// list show it. Only pools the caller may use appear; an inaccessible pool's
// name, members and counts are never sent. Reason says why a pool that is not
// available is not, in the pool refusal vocabulary.
//
// Limits has one entry per run type the pool allows, each the effective
// defaults and caps for this caller with the source that bound every value
// (runnerpool.Effective); a run type absent from it is one the pool refuses.
// Barriers are the confinement classes the pool allows. Both are absent for a
// pool that adds no limits of its own.
type RunnerPoolChoice struct {
	ID           uuid.UUID                `json:"id"`
	Name         string                   `json:"name"`
	HostingType  RunnerPoolHosting        `json:"hosting_type"`
	Availability string                   `json:"availability"`
	Reason       RunnerPoolReason         `json:"reason,omitempty"`
	Limits       []EffectiveRunLimits     `json:"limits,omitempty"`
	Barriers     []types.ConfinementClass `json:"barriers,omitempty"`
}

// RunnerPoolList is the caller's permitted pool catalogue.
type RunnerPoolList struct {
	Pools []RunnerPoolChoice `json:"pools"`
}

// CreateRunnerPoolRequest names a new pool. The hosting type is fixed for its
// life. Limits are validated (RunnerPoolLimits.Validate); a pool created without
// them adds no bound of its own.
type CreateRunnerPoolRequest struct {
	Name        string            `json:"name"`
	HostingType RunnerPoolHosting `json:"hosting_type"`
	Limits      *RunnerPoolLimits `json:"limits,omitempty"`
}

// Validate refuses a bad name, a hosting type outside the two and limits a pool
// cannot keep.
func (r CreateRunnerPoolRequest) Validate() error {
	if !r.HostingType.Valid() {
		return errors.New("hosting_type is remote_provided or self_hosted")
	}
	if err := types.ValidateRunnerPoolName(r.Name); err != nil {
		return err
	}
	if r.Limits != nil {
		return r.Limits.Validate()
	}
	return nil
}

// RunnerPoolSwitch is the state an update may put a pool in: active or disabled.
// Deleting a pool is DeleteRunnerPool alone, so it has one audit action.
type RunnerPoolSwitch string

const (
	RunnerPoolSwitchActive   RunnerPoolSwitch = "active"
	RunnerPoolSwitchDisabled RunnerPoolSwitch = "disabled"
)

// UpdateRunnerPoolRequest renames, switches off/on or re-limits one pool.
// Revision is the revision the caller read: a stale one is refused with
// runner_pool_stale. Limits replace the pool's limits as a whole.
type UpdateRunnerPoolRequest struct {
	Revision int64             `json:"revision"`
	Name     *string           `json:"name,omitempty"`
	State    *RunnerPoolSwitch `json:"state,omitempty"`
	Limits   *RunnerPoolLimits `json:"limits,omitempty"`
}

// Validate refuses an update that changes nothing, a revision that was never
// read, a bad name, limits a pool cannot keep, and any state but active or
// disabled (never deleted).
func (r UpdateRunnerPoolRequest) Validate() error {
	switch {
	case r.Revision < 1:
		return errors.New("revision is the revision you read")
	case r.Name == nil && r.State == nil && r.Limits == nil:
		return errors.New("change the name, the state or the limits")
	case r.State != nil && *r.State != RunnerPoolSwitchActive && *r.State != RunnerPoolSwitchDisabled:
		return errors.New("state is active or disabled; delete a pool with DELETE")
	}
	var nameErr, limitsErr error
	if r.Name != nil {
		nameErr = types.ValidateRunnerPoolName(*r.Name)
	}
	if r.Limits != nil {
		limitsErr = r.Limits.Validate()
	}
	return errors.Join(nameErr, limitsErr)
}

// ListRunnerPools lists the pools the caller may use.
func (c *Client) ListRunnerPools(ctx context.Context) (RunnerPoolList, error) {
	var out RunnerPoolList
	err := c.do(ctx, http.MethodGet, "/api/v1/runner-pools", nil, &out)
	return out, err
}

// GetRunnerPool reads one pool the caller may use; a pool they may not use is a 404.
func (c *Client) GetRunnerPool(ctx context.Context, id uuid.UUID) (RunnerPool, error) {
	var out RunnerPool
	err := c.do(ctx, http.MethodGet, "/api/v1/runner-pools/"+id.String(), nil, &out)
	return out, err
}

// CreateRunnerPool adds a pool to the organisation's catalogue (admin).
func (c *Client) CreateRunnerPool(ctx context.Context, req CreateRunnerPoolRequest) (RunnerPool, error) {
	var out RunnerPool
	err := c.do(ctx, http.MethodPost, "/api/v1/runner-pools", req, &out)
	return out, err
}

// UpdateRunnerPool renames or switches a pool off or on (admin).
func (c *Client) UpdateRunnerPool(ctx context.Context, id uuid.UUID, req UpdateRunnerPoolRequest) (RunnerPool, error) {
	var out RunnerPool
	err := c.do(ctx, http.MethodPut, "/api/v1/runner-pools/"+id.String(), req, &out)
	return out, err
}

// DeleteRunnerPool tombstones a pool (admin). Runs bound to it keep their history.
func (c *Client) DeleteRunnerPool(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/runner-pools/"+id.String(), nil, nil)
}

// AddMyRunnerToPool puts the caller's own claimed runner into a self-hosted
// pool. Nobody adds a runner on another person's behalf.
func (c *Client) AddMyRunnerToPool(ctx context.Context, poolID, runnerID uuid.UUID) (RunnerPoolMember, error) {
	var out RunnerPoolMember
	err := c.do(ctx, http.MethodPut, "/api/v1/me/runner-pools/"+poolID.String()+"/runners/"+runnerID.String(), nil, &out)
	return out, err
}

// RemoveMyRunnerFromPool takes the caller's own runner out of a pool.
func (c *Client) RemoveMyRunnerFromPool(ctx context.Context, poolID, runnerID uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/runner-pools/"+poolID.String()+"/runners/"+runnerID.String(), nil, nil)
}

// GetOrgRunnerPoolDefaults reads the organisation's default hosting type and pools.
func (c *Client) GetOrgRunnerPoolDefaults(ctx context.Context) (RunnerPoolDefaults, error) {
	var out RunnerPoolDefaults
	err := c.do(ctx, http.MethodGet, "/api/v1/runner-pool-defaults", nil, &out)
	return out, err
}

// SetOrgRunnerPoolDefaults replaces the organisation's defaults (admin). It
// never rewrites a person's own.
func (c *Client) SetOrgRunnerPoolDefaults(ctx context.Context, d RunnerPoolDefaults) (RunnerPoolDefaults, error) {
	var out RunnerPoolDefaults
	err := c.do(ctx, http.MethodPut, "/api/v1/runner-pool-defaults", d, &out)
	return out, err
}

// GetMyRunnerPoolDefaults reads the caller's own defaults; an unset field
// inherits the organisation's.
func (c *Client) GetMyRunnerPoolDefaults(ctx context.Context) (RunnerPoolDefaults, error) {
	var out RunnerPoolDefaults
	err := c.do(ctx, http.MethodGet, "/api/v1/me/runner-pool-defaults", nil, &out)
	return out, err
}

// SetMyRunnerPoolDefaults replaces the caller's own defaults. A default only
// seeds a choice: it never grants a pool the caller could not choose.
func (c *Client) SetMyRunnerPoolDefaults(ctx context.Context, d RunnerPoolDefaults) (RunnerPoolDefaults, error) {
	var out RunnerPoolDefaults
	err := c.do(ctx, http.MethodPut, "/api/v1/me/runner-pool-defaults", d, &out)
	return out, err
}

// ClearMyRunnerPoolDefaults removes the caller's own defaults, so every field
// inherits the organisation's again.
func (c *Client) ClearMyRunnerPoolDefaults(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/runner-pool-defaults", nil, nil)
}
