// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// The doors a support grant may name, as the server spells them.
const (
	SupportDoorTerminal = "terminal"
	SupportDoorCommands = "commands"
	SupportDoorFiles    = "files"
	SupportDoorForward  = "forward"
	SupportDoorWebApps  = "web_apps"
	SupportDoorDesktop  = "desktop"
)

// SupportGrant is a help request or session as the support routes answer it (mirrors the server's
// supportGrantView). Reason is the helper's own words: it reaches the run's owner, and the helper while the
// grant is open, and is empty for everyone else and once the grant has ended.
type SupportGrant struct {
	Grant       uuid.UUID  `json:"grant"`
	Helper      string     `json:"helper"`
	Reason      string     `json:"reason,omitempty"`
	DoorsAsked  []string   `json:"doors_asked"`
	Doors       []string   `json:"doors,omitempty"`
	Minutes     int        `json:"minutes,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	// State is requested (waiting for the owner), active (accepted, not ended) or ended.
	State     string `json:"state"`
	EndReason string `json:"end_reason,omitempty"`
	// RecordedDoors, on a grant that is not ended, is the doors Wardyn records on this run.
	RecordedDoors []string `json:"recorded_doors,omitempty"`
}

// SupportStatus is GET /api/v1/runs/{id}/support: the open grant, the last one that ended, the doors a
// request may ask for and the doors this run records. Open and Ended are nil when there is none.
type SupportStatus struct {
	Open          *SupportGrant `json:"open"`
	Ended         *SupportGrant `json:"ended"`
	DoorsOffered  []string      `json:"doors_offered"`
	RecordedDoors []string      `json:"recorded_doors"`
}

// SupportRequest is the body of POST /api/v1/runs/{id}/support: the doors asked for and the reason the run's
// owner reads. It names no lifetime; the owner picks it.
type SupportRequest struct {
	Doors  []string `json:"doors"`
	Reason string   `json:"reason"`
}

// SupportAccept is the body of POST /api/v1/runs/{id}/support/{grant}/accept: a subset of the doors asked and
// the minutes (15, 30 or 60) the help lasts.
type SupportAccept struct {
	Doors   []string `json:"doors"`
	Minutes int      `json:"minutes"`
}

func supportPath(run uuid.UUID, tail ...string) string {
	p := "/api/v1/runs/" + run.String() + "/support"
	for _, t := range tail {
		p += "/" + t
	}
	return p
}

// RequestSupport asks the run's owner to let the caller help inside the run (a super admin signed in as a
// person). 409 support_grant_open: the run already has a request or session; 409 support_disabled: the switch
// is off; 409 support_run_ineligible: the run cannot take a helper.
func (c *Client) RequestSupport(ctx context.Context, run uuid.UUID, req SupportRequest) (SupportGrant, error) {
	var out SupportGrant
	err := c.do(ctx, http.MethodPost, supportPath(run), req, &out)
	return out, err
}

// SupportStatus reads a run's help request or session and the last one that ended.
func (c *Client) SupportStatus(ctx context.Context, run uuid.UUID) (SupportStatus, error) {
	var out SupportStatus
	err := c.do(ctx, http.MethodGet, supportPath(run), nil, &out)
	return out, err
}

// AcceptSupport is the run owner's yes. 404 support_grant_not_found: the request was answered, ended or lapsed.
func (c *Client) AcceptSupport(ctx context.Context, run, grant uuid.UUID, req SupportAccept) (SupportGrant, error) {
	var out SupportGrant
	err := c.do(ctx, http.MethodPost, supportPath(run, grant.String(), "accept"), req, &out)
	return out, err
}

// DeclineSupport is the run owner's no, on a request not yet accepted.
func (c *Client) DeclineSupport(ctx context.Context, run, grant uuid.UUID) (SupportGrant, error) {
	var out SupportGrant
	err := c.do(ctx, http.MethodPost, supportPath(run, grant.String(), "decline"), nil, &out)
	return out, err
}

// EndSupport ends an open request or session: as the run's owner, the grant's helper or a security operator.
func (c *Client) EndSupport(ctx context.Context, run, grant uuid.UUID) (SupportGrant, error) {
	var out SupportGrant
	err := c.do(ctx, http.MethodPost, supportPath(run, grant.String(), "end"), nil, &out)
	return out, err
}
