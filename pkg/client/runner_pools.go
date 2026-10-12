// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runner pool contract (0.9). A pool is a set of execution targets: an
// organisation's Remote Provided pool (administrators keep it) or one person's
// own Self-Hosted pool of their own claimed runners (only they keep or use it).
// Choosing one constrains where a run may go and grants nothing. A server that cannot honour pools yet answers every method below,
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

// RunnerPoolChoice is one pool a person may choose, as the preflight and the
// policy preview show it. Only pools the caller may use appear; an inaccessible
// pool's name, members and counts are never sent. Reason says why a pool that is
// not available is not: a pool refusal reason or one of
// runnerpool.AvailabilityReasons. The pool list answers the same five leading
// fields for the same pool and caller (RunnerPoolCatalogueEntry).
//
// Limits and Barriers are never filled on these doors: the list carries a
// pool's limits and barrier_options, and preflight carries this run's.
// MainDrive is filled on the list and on both doors.
type RunnerPoolChoice struct {
	ID           uuid.UUID                `json:"id"`
	Name         string                   `json:"name"`
	HostingType  RunnerPoolHosting        `json:"hosting_type"`
	Availability string                   `json:"availability"`
	Reason       RunnerPoolReason         `json:"reason,omitempty"`
	Limits       []EffectiveRunLimits     `json:"limits,omitempty"`
	Barriers     []types.ConfinementClass `json:"barriers,omitempty"`
	// MainDrive is what this pool's runners do with a run's main-drive size, for the caller's own eligible
	// targets. Absent on a pool the caller may only manage. The enforcement word reaches members only here.
	MainDrive *MainDriveFacts `json:"main_drive,omitempty"`
}

// MainDriveEnforcement is how a pool binds a run's main-drive size. Never request or external.
type MainDriveEnforcement string

// MainDriveCoverage is how much of the sandbox the enforced size covers.
type MainDriveCoverage string

const (
	MainDriveFilesystem MainDriveEnforcement = "filesystem"
	MainDriveEviction   MainDriveEnforcement = "eviction"
	MainDriveNone       MainDriveEnforcement = "none"
	MainDriveUnknown    MainDriveEnforcement = "unknown"

	// MainDriveCoverageAll is the sandbox's writable layer (Docker's /tmp is a separate in-memory tmpfs).
	MainDriveCoverageAll MainDriveCoverage = "all"
	// MainDriveCoverageScratch is /tmp, /home/agent/work and /home/agent/.cache only.
	MainDriveCoverageScratch MainDriveCoverage = "scratch_paths"
)

// MainDriveFacts is what a pool's runners do with the main drive, over the caller's eligible targets.
type MainDriveFacts struct {
	// Enforcement is the WEAKEST word across the targets; unknown when any target's is unknown (offline,
	// never reported, or no target at all).
	Enforcement MainDriveEnforcement `json:"enforcement"`
	// Basis is substrate (this deployment's executor) or runner_asserted (a self-hosted runner's own report).
	Basis types.RunEvidenceSource `json:"basis"`
	// Coverage is absent when enforcement is none or unknown.
	Coverage MainDriveCoverage `json:"coverage,omitempty"`
	// Keep is what happens to an Interactive run's sandbox at its end time.
	Keep MainDriveKeep `json:"keep"`
	// DriveMounts: the pool's runners can mount drives at all (false for a self-hosted pool).
	DriveMounts bool `json:"drive_mounts"`
}

// MainDriveKeep: every target keeps a stopped sandbox and the deployment's ended-run grace is above zero.
type MainDriveKeep struct {
	Supported bool `json:"supported"`
	GraceSec  int  `json:"grace_sec,omitempty"` // set only when Supported
}

// Where a pool's run types come from: its own limits, or none, which takes both.
const (
	RunTypesSourcePool = "pool"
	RunTypesSourceNone = "none"
)

// BarrierOption is one barrier as a pool offers it to the caller (runnerpool.BarrierOptions).
type BarrierOption = runnerpool.BarrierOption

// RunnerPoolCatalogueEntry is one row of GET /runner-pools. A pool the caller may
// use carries its run types, the effective limits of each (Limits, one per run
// type, or only the ?run_type= one) and its three barriers. A pool the caller may
// only manage (reason runner_pool_not_found) carries none of them: nothing is
// computed for a pool the caller may not use.
type RunnerPoolCatalogueEntry struct {
	RunnerPoolChoice
	RunTypes []RunnerPoolRunType `json:"run_types"`
	// RunTypesSource is RunTypesSourcePool or RunTypesSourceNone.
	RunTypesSource string          `json:"run_types_source,omitempty"`
	BarrierOptions []BarrierOption `json:"barrier_options"`
	// MyRunners is every one of the caller's OWN runners that is a member of a
	// self-hosted pool, whether or not it can take a run now; never another
	// person's. It is how a person reads which pools each of their runners is in.
	MyRunners []PoolRunner `json:"my_runners,omitempty"`
	// Slots is how busy the pool is; absent on a row the caller may only manage.
	Slots *PoolSlots `json:"slots,omitempty"`
	// Owner is a Self-Hosted pool's owner: the caller on their own pools, and the
	// person each other pool belongs to on an administrator's oversight rows.
	Owner string `json:"owner,omitempty"`
}

// The Verdict of PoolSlots: advisory, since the launch decides.
const (
	SlotsStartsNow = "starts_now"
	SlotsWaits     = "waits"
)

// PoolSlots is a pool's slot counts, everyone's and never saying whose. On the pool list MaxWaitSec
// is set only when ?run_type= is asked; on preflight and the policy preview it is the run's own.
type PoolSlots struct {
	// Used is the runs holding one of the pool's slots; Waiting the runs queued for one.
	Used    int `json:"used"`
	Waiting int `json:"waiting"`
	// Max is the pool's max_concurrent_runs; absent means no pool limit.
	Max *int `json:"max,omitempty"`
	// Verdict is SlotsWaits when the pool has a limit and is at it or has runs waiting, else SlotsStartsNow.
	Verdict string `json:"verdict"`
	// MaxWaitSec is how long a run of that type waits by default: 86400 Background, 3600 Interactive.
	MaxWaitSec int `json:"max_wait_sec,omitempty"`
}

// PoolRunner is one of the caller's own runners in a self-hosted pool.
type PoolRunner struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Online bool      `json:"online"`
}

// RunnerPoolList is the caller's pool catalogue, their allowance and the pool an
// untouched run resolves to.
type RunnerPoolList struct {
	Pools []RunnerPoolCatalogueEntry `json:"pools"`
	// Allowance is absent for a caller whose runs stand outside governance.
	Allowance *PersonAllowance `json:"allowance,omitempty"`
	// Default is absent when no default applies.
	Default *PoolDefault `json:"default,omitempty"`
}

// PersonAllowance is the caller's own concurrent runs against their governance
// limit: counts only.
type PersonAllowance struct {
	// Active is every non-terminal run the caller created, in any pool.
	Active int `json:"active"`
	// Max is the profile's max_concurrent_runs; absent means no personal limit.
	Max         *int   `json:"max,omitempty"`
	ProfileName string `json:"profile_name,omitempty"`
}

// PoolDefault is the pool a run that names none resolves to (runnerpool.Resolve),
// or, with Reason runner_pool_default_unavailable, the default that no longer
// applies and whose it was.
type PoolDefault struct {
	PoolID    *uuid.UUID          `json:"pool_id,omitempty"`
	Selection RunnerPoolSelection `json:"selection,omitempty"`
	Reason    RunnerPoolReason    `json:"reason,omitempty"`
}

// CreateRunnerPoolRequest names a new Remote Provided pool (admin); a server
// refuses self_hosted here, since each person creates their own
// (CreateMyRunnerPoolRequest). The hosting type is fixed for its life. Limits are validated (RunnerPoolLimits.Validate); a pool created without
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

// CreateMyRunnerPoolRequest names a new Self-Hosted pool of the caller's own.
// Limits are as CreateRunnerPoolRequest's; every run is still bounded by the
// caller's governance profile.
type CreateMyRunnerPoolRequest struct {
	Name   string            `json:"name"`
	Limits *RunnerPoolLimits `json:"limits,omitempty"`
}

// Validate refuses a bad name and limits a pool cannot keep.
func (r CreateMyRunnerPoolRequest) Validate() error {
	return CreateRunnerPoolRequest{Name: r.Name, HostingType: RunnerPoolSelfHosted, Limits: r.Limits}.Validate()
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
// runner_pool_stale. A present Limits replaces the pool's limits as a WHOLE
// block, so a client sends back the block it read with its change in it (an
// omitted max_concurrent_runs removes the cap); an absent Limits leaves the
// stored block unchanged.
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

// ListRunnerPools lists the pools the caller may use, judged for every run type each takes.
func (c *Client) ListRunnerPools(ctx context.Context) (RunnerPoolList, error) {
	return c.ListRunnerPoolsFor(ctx, "")
}

// ListRunnerPoolsFor lists the pools judged for one run type: a pool that refuses it is unavailable
// with runner_pool_run_type_not_allowed, and each pool's limits hold only that type. "" is every type;
// a value other than background or interactive is a 400 runner_pool_invalid.
func (c *Client) ListRunnerPoolsFor(ctx context.Context, runType RunnerPoolRunType) (RunnerPoolList, error) {
	path := "/api/v1/runner-pools"
	if runType != "" {
		path += "?run_type=" + url.QueryEscape(string(runType))
	}
	var out RunnerPoolList
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// GetRunnerPool reads one pool the caller may use; a pool they may not use is a 404.
func (c *Client) GetRunnerPool(ctx context.Context, id uuid.UUID) (RunnerPool, error) {
	var out RunnerPool
	err := c.do(ctx, http.MethodGet, "/api/v1/runner-pools/"+id.String(), nil, &out)
	return out, err
}

// CreateRunnerPool adds a Remote Provided pool to the organisation's catalogue (admin).
func (c *Client) CreateRunnerPool(ctx context.Context, req CreateRunnerPoolRequest) (RunnerPool, error) {
	var out RunnerPool
	err := c.do(ctx, http.MethodPost, "/api/v1/runner-pools", req, &out)
	return out, err
}

// UpdateRunnerPool renames, switches or re-limits a Remote Provided pool (admin).
func (c *Client) UpdateRunnerPool(ctx context.Context, id uuid.UUID, req UpdateRunnerPoolRequest) (RunnerPool, error) {
	var out RunnerPool
	err := c.do(ctx, http.MethodPut, "/api/v1/runner-pools/"+id.String(), req, &out)
	return out, err
}

// DeleteRunnerPool tombstones a Remote Provided pool (admin). Runs bound to it keep their history.
func (c *Client) DeleteRunnerPool(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/runner-pools/"+id.String(), nil, nil)
}

// CreateMyRunnerPool creates a Self-Hosted pool of the caller's own.
func (c *Client) CreateMyRunnerPool(ctx context.Context, req CreateMyRunnerPoolRequest) (RunnerPool, error) {
	var out RunnerPool
	err := c.do(ctx, http.MethodPost, "/api/v1/me/runner-pools", req, &out)
	return out, err
}

// UpdateMyRunnerPool renames, switches or re-limits one of the caller's own
// Self-Hosted pools; anyone else's answers as an unknown id does.
func (c *Client) UpdateMyRunnerPool(ctx context.Context, id uuid.UUID, req UpdateRunnerPoolRequest) (RunnerPool, error) {
	var out RunnerPool
	err := c.do(ctx, http.MethodPut, "/api/v1/me/runner-pools/"+id.String(), req, &out)
	return out, err
}

// DeleteMyRunnerPool tombstones one of the caller's own Self-Hosted pools.
func (c *Client) DeleteMyRunnerPool(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/runner-pools/"+id.String(), nil, nil)
}

// AddMyRunnerToPool puts the caller's own claimed runner into one of their own
// Self-Hosted pools. Nobody adds a runner on another person's behalf.
func (c *Client) AddMyRunnerToPool(ctx context.Context, poolID, runnerID uuid.UUID) (RunnerPoolMember, error) {
	var out RunnerPoolMember
	err := c.do(ctx, http.MethodPut, "/api/v1/me/runner-pools/"+poolID.String()+"/runners/"+runnerID.String(), nil, &out)
	return out, err
}

// RemoveMyRunnerFromPool takes the caller's own runner out of a pool.
func (c *Client) RemoveMyRunnerFromPool(ctx context.Context, poolID, runnerID uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/runner-pools/"+poolID.String()+"/runners/"+runnerID.String(), nil, nil)
}

// GetOrgRunnerPoolDefaults reads the organisation's default hosting type, pools and barrier.
func (c *Client) GetOrgRunnerPoolDefaults(ctx context.Context) (RunnerPoolDefaults, error) {
	var out RunnerPoolDefaults
	err := c.do(ctx, http.MethodGet, "/api/v1/runner-pool-defaults", nil, &out)
	return out, err
}

// SetOrgRunnerPoolDefaults replaces the whole organisation document (admin), its default barrier
// included: a d without Barrier clears it, so read the document first to keep what you do not change.
// It never rewrites a person's own.
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
