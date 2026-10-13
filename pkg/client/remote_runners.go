// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Remote-hosted runners (0.9): the Docker host or Kubernetes cluster this server is set up with, as
// runners with a name and an "Available to people's pools" policy. Administrators and security admins
// read them all; an administrator renames one; the people policy is held for a second human where the
// server asks for one. A person lists only the ones they may add to their own user pools.

// The remote-hosted runner shapes, re-exported so SDK callers can name them.
type (
	RemoteRunner            = types.RemoteRunner
	RemoteRunnerPeoplePools = types.RemoteRunnerPeoplePools
	RemoteRunnerPoolRef     = types.RemoteRunnerPoolRef
)

// The people policy values.
const (
	PeoplePoolsOff      = types.PeoplePoolsOff
	PeoplePoolsEveryone = types.PeoplePoolsEveryone
	PeoplePoolsListed   = types.PeoplePoolsListed
)

// RemoteRunnerView is a remote-hosted runner as the server reads it. Configured says this server runs
// it; a runner that is not configured stays listed and never takes a run. Online is whether it answered
// its last health read. Pools are the live pools it is in: every pool on the operators' reads, the
// caller's own user pools on ListMyRemoteRunners (which also leaves out the people list).
type RemoteRunnerView struct {
	RemoteRunner
	Configured bool                  `json:"configured"`
	Online     bool                  `json:"online"`
	Pools      []RemoteRunnerPoolRef `json:"pools"`
}

// UpdateRemoteRunnerRequest renames a remote-hosted runner at the revision the caller read.
type UpdateRemoteRunnerRequest struct {
	Revision int64   `json:"revision"`
	Name     *string `json:"name,omitempty"`
}

// Validate refuses a revision that was never read and a missing or bad name.
func (r UpdateRemoteRunnerRequest) Validate() error {
	switch {
	case r.Revision < 1:
		return errors.New("revision is the revision you read")
	case r.Name == nil:
		return errors.New("change the name")
	}
	return types.ValidateRemoteRunnerName(*r.Name)
}

// RemoteRunnerPeoplePolicyRequest makes a remote-hosted runner available to people's pools: everyone
// (no subjects), or the listed people, groups and roles (1 to 64). Revision is optional optimistic
// concurrency against the runner's revision. Turning it off is ClearRemoteRunnerPeoplePolicy.
type RemoteRunnerPeoplePolicyRequest struct {
	Revision    *int64                  `json:"revision,omitempty"`
	PeoplePools RemoteRunnerPeoplePools `json:"people_pools"`
	Subjects    []RunnerPoolSubject     `json:"subjects,omitempty"`
}

// RunnerPoolMemberView is one runner of a pool, either kind. ID is the self-hosted runner's id or the
// remote-hosted runner's. Runner is a self-hosted member's runner view, its owner named on an
// operator's read; Configured is set for a remote-hosted member this server runs.
type RunnerPoolMemberView struct {
	Kind       RunnerKind        `json:"kind"`
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Online     bool              `json:"online"`
	Configured bool              `json:"configured,omitempty"`
	Runner     *types.RunnerView `json:"runner,omitempty"`
}

// ListRemoteRunners lists every remote-hosted runner (admin or security admin).
func (c *Client) ListRemoteRunners(ctx context.Context) ([]RemoteRunnerView, error) {
	var out []RemoteRunnerView
	err := c.do(ctx, http.MethodGet, "/api/v1/remote-runners", nil, &out)
	return out, err
}

// GetRemoteRunner reads one remote-hosted runner (admin or security admin).
func (c *Client) GetRemoteRunner(ctx context.Context, id string) (RemoteRunnerView, error) {
	var out RemoteRunnerView
	err := c.do(ctx, http.MethodGet, "/api/v1/remote-runners/"+url.PathEscape(id), nil, &out)
	return out, err
}

// UpdateRemoteRunner renames a remote-hosted runner (admin).
func (c *Client) UpdateRemoteRunner(ctx context.Context, id string, req UpdateRemoteRunnerRequest) (RemoteRunnerView, error) {
	var out RemoteRunnerView
	err := c.do(ctx, http.MethodPut, "/api/v1/remote-runners/"+url.PathEscape(id), req, &out)
	return out, err
}

// SetRemoteRunnerPeoplePolicy sets who may add a remote-hosted runner to their own user pools (admin or
// security admin). A held write returns a *PendingApprovalError naming the change.
func (c *Client) SetRemoteRunnerPeoplePolicy(ctx context.Context, id string, req RemoteRunnerPeoplePolicyRequest) (RemoteRunner, error) {
	var out RemoteRunner
	err := c.do(ctx, http.MethodPut, "/api/v1/remote-runners/"+url.PathEscape(id)+"/people-policy", req, &out)
	return out, err
}

// ClearRemoteRunnerPeoplePolicy turns the people policy off: nobody may add the runner to a user pool
// (those already in one stop being offered to people at launch). Held like a set.
func (c *Client) ClearRemoteRunnerPeoplePolicy(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/remote-runners/"+url.PathEscape(id)+"/people-policy", nil, nil)
}

// ListMyRemoteRunners lists the remote-hosted runners the caller may add to their own user pools.
func (c *Client) ListMyRemoteRunners(ctx context.Context) ([]RemoteRunnerView, error) {
	var out []RemoteRunnerView
	err := c.do(ctx, http.MethodGet, "/api/v1/me/remote-runners", nil, &out)
	return out, err
}
