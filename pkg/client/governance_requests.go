// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GovernanceRequest is a held request (an elevation today) as GET and POST /api/v1/governance/requests
// answer it: the held change, the kind's fields and who decides it.
type GovernanceRequest struct {
	GovernanceChange
	Kind      string                     `json:"kind"` // elevation | image
	UnitID    *uuid.UUID                 `json:"unit_id,omitempty"`
	UnitName  string                     `json:"unit_name,omitempty"`
	Duration  string                     `json:"duration,omitempty"` // as max_elevation is stored: "4h", "1h30m"
	EndsAt    *time.Time                 `json:"ends_at,omitempty"`  // an applied elevation's end
	Image     string                     `json:"image,omitempty"`
	Mine      bool                       `json:"mine"`
	Decidable bool                       `json:"decidable"`
	Deciders  []GovernanceChangeApprover `json:"deciders"`
}

// ElevationOffer is one unit the caller may ask an elevation in.
type ElevationOffer struct {
	UnitID         uuid.UUID `json:"unit_id"`
	UnitName       string    `json:"unit_name"`
	MaxElevation   string    `json:"max_elevation"`
	TicketRequired bool      `json:"ticket_required,omitempty"`
}

// GovernanceRequestList is GET /api/v1/governance/requests: the requests the caller may see (pending, and
// decided in the last day) and the units they may ask an elevation in.
type GovernanceRequestList struct {
	Requests []GovernanceRequest `json:"requests"`
	Offered  []ElevationOffer    `json:"offered"`
}

// ListGovernanceRequests lists the requests the caller may see and the elevations offered to them. GET
// /api/v1/governance/requests. A server older than requests answers 404, returned as the *APIError.
func (c *Client) ListGovernanceRequests(ctx context.Context) (GovernanceRequestList, error) {
	var out GovernanceRequestList
	err := c.do(ctx, http.MethodGet, "/api/v1/governance/requests", nil, &out)
	return out, err
}

// RequestElevation asks to be a member of unitID for duration, with a reason and, where the unit asks
// for one, a ticket ("" omits it). The request is held until a decider approves it. POST
// /api/v1/governance/requests.
func (c *Client) RequestElevation(ctx context.Context, unitID uuid.UUID, duration time.Duration, reason, ticket string) (GovernanceRequest, error) {
	body := struct {
		Kind     string `json:"kind"`
		UnitID   string `json:"unit_id"`
		Duration string `json:"duration"`
		Reason   string `json:"reason"`
		Ticket   string `json:"ticket,omitempty"`
	}{"elevation", unitID.String(), types.CanonicalDuration(duration), reason, ticket}
	b, err := json.Marshal(body)
	if err != nil {
		return GovernanceRequest{}, fmt.Errorf("marshal request body: %w", err)
	}
	// Not c.do: every request's 202 carries its pending_change, which do reads as a held write; the answer
	// here is the request beside it.
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/governance/requests", bytes.NewReader(b), true)
	if err != nil {
		return GovernanceRequest{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return GovernanceRequest{}, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPendingErrBody))
	if err != nil {
		return GovernanceRequest{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := NewAPIError(resp.StatusCode, raw)
		if len(apiErr.Body) > maxErrBody {
			apiErr.Body = apiErr.Body[:maxErrBody]
		}
		return GovernanceRequest{}, apiErr
	}
	var out struct {
		Request GovernanceRequest `json:"request"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return GovernanceRequest{}, fmt.Errorf("decode response: %w", err)
	}
	return out.Request, nil
}

// ApproveGovernanceRequest approves a request the caller may decide. POST
// /api/v1/governance/requests/{id}/approve.
func (c *Client) ApproveGovernanceRequest(ctx context.Context, id uuid.UUID) (GovernanceChange, error) {
	var out GovernanceChange
	err := c.do(ctx, http.MethodPost, "/api/v1/governance/requests/"+id.String()+"/approve", nil, &out)
	return out, err
}

// RejectGovernanceRequest rejects a request; the requester's own reject withdraws it. reason is optional
// ("" omits it). POST /api/v1/governance/requests/{id}/reject.
func (c *Client) RejectGovernanceRequest(ctx context.Context, id uuid.UUID, reason string) (GovernanceChange, error) {
	var out GovernanceChange
	body := struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: reason}
	err := c.do(ctx, http.MethodPost, "/api/v1/governance/requests/"+id.String()+"/reject", body, &out)
	return out, err
}
