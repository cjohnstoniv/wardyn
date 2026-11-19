// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// windowReportTimeout bounds one mac_window_unexpected send.
const windowReportTimeout = 10 * time.Second

// windowReport sends one unexpected window; set once the session machinery is up. A runner has one.
var windowReport atomic.Pointer[func(uuid.UUID, string, time.Time)]

// unexpectedWindow is the driver's hearing of a window the helper opened with a grant this runner did not
// issue: someone used the helper's control socket on this Mac directly. It is reported to the organisation
// and never closed here. With no session to report on it is logged: the window is the person's own Mac.
func unexpectedWindow(runID uuid.UUID, grant string, at time.Time) {
	if report := windowReport.Load(); report != nil {
		(*report)(runID, grant, at)
		return
	}
	slog.Warn("macOS VM window opened without a grant from this runner; no session to report it on", "run", runID, "grant", grant)
}

// reportWindowsOn sends each unexpected window on the peer peer returns; nil is no session.
func reportWindowsOn(ctx context.Context, peer func() *runnerwire.Peer) {
	report := func(runID uuid.UUID, grant string, at time.Time) {
		p := peer()
		if p == nil {
			slog.WarnContext(ctx, "macOS VM window opened without a grant from this runner; no session to report it on", "run", runID, "grant", grant)
			return
		}
		if err := sendUnexpectedWindow(ctx, p, runID, grant, at); err != nil {
			slog.WarnContext(ctx, "macOS VM window opened without a grant from this runner; the report failed", "run", runID, "grant", grant, "error", err)
		}
	}
	windowReport.Store(&report)
}

// sendUnexpectedWindow sends one mac_window_unexpected event and waits for the organisation's answer.
func sendUnexpectedWindow(ctx context.Context, peer *runnerwire.Peer, runID uuid.UUID, grant string, at time.Time) error {
	data, err := runnerwire.Marshal(runnerwire.MacWindowUnexpected{Grant: grant, At: at.UTC()})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, windowReportTimeout)
	defer cancel()
	return peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventMacWindowUnexpected, RunID: &runID, Data: data})
}
