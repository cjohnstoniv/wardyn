// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RunEvent is one entry of a run's lifecycle feed, GET /api/v1/runs/{id}/events.
// ID is monotonic per run. Reason is set on RunEventFailed only, State on
// RunEventEnded only. It never carries log or secret content.
type RunEvent struct {
	ID     uint64    `json:"id"`
	Type   string    `json:"type"`
	Reason string    `json:"reason,omitempty"`
	State  RunState  `json:"state,omitempty"`
	At     time.Time `json:"at"`
}

// The closed RunEvent.Type vocabulary, in the order a run can reach them.
// Every stream finishes with RunEventEnded.
const (
	RunEventProvisioning = "provisioning" // PENDING -> STARTING: sandbox being created
	RunEventPulling      = "pulling"      // the substrate reported an image pull
	RunEventReady        = "ready"        // STARTING -> RUNNING
	RunEventIdleStopped  = "idle_stopped" // the idle reaper stopped the run
	RunEventFailed       = "failed"       // -> FAILED; Reason names the phase
	RunEventEnded        = "ended"        // terminal; State is the terminal state
)

// RunEventFailed reasons: which phase the run failed in.
const (
	RunFailedNotStarted  = "not_started"  // failed before provisioning began
	RunFailedStartFailed = "start_failed" // failed while provisioning
	RunFailedRunFailed   = "run_failed"   // failed after it was ready
)

// runEventsReconnectDelay spaces the reconnects RunEvents makes when the server
// closes a held stream before the run ended.
const runEventsReconnectDelay = time.Second

// RunEvents follows a run's lifecycle feed, calling fn with each event in order,
// starting after afterID (0 = from the first). The server closes a held stream
// every few minutes; RunEvents reconnects with Last-Event-ID, so fn sees every
// event exactly once. It returns nil after fn has seen RunEventEnded, fn's own
// error if fn returns one, an *APIError for a non-2xx answer (404 for a run the
// caller cannot read), or the transport error — pass the last ID fn saw back as
// afterID to resume.
//
// Resume is within the daemon's lifetime: the feed is kept in memory, so after
// a wardynd restart a run that is still live replays only what happens next,
// and a run that had already ended answers RunEventEnded alone.
func (c *Client) RunEvents(ctx context.Context, runID uuid.UUID, afterID uint64, fn func(RunEvent) error) error {
	for {
		ended, err := c.runEventsOnce(ctx, runID, &afterID, fn)
		if err != nil || ended {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(runEventsReconnectDelay):
		}
	}
}

// runEventsOnce reads one connection's worth of the feed. Only the data: lines
// are read — each carries the whole event, id included — so the id:/event:
// lines and the heartbeat comments need no parsing.
func (c *Client) runEventsOnce(ctx context.Context, runID uuid.UUID, afterID *uint64, fn func(RunEvent) error) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/runs/"+runID.String()+"/events", nil, false)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if *afterID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(*afterID, 10))
	}
	resp, err := c.streamClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return false, NewAPIError(resp.StatusCode, raw)
	}
	// A 2xx that is not a stream (a captive portal, an SPA fallback) carries
	// no events and never ends; reconnecting to it would spin until ctx.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return false, fmt.Errorf("run events: the server answered %q, not text/event-stream", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev RunEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return false, fmt.Errorf("decode run event: %w", err)
		}
		*afterID = ev.ID
		if err := fn(ev); err != nil {
			return false, err
		}
		if ev.Type == RunEventEnded {
			return true, nil
		}
	}
	return false, sc.Err()
}
