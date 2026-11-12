// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A window the helper opened with a grant this runner did not issue reaches the organisation as a
// mac_window_unexpected event for that run, carrying the grant as seen and when; with no session nothing
// is sent and nothing panics.
func TestUnexpectedWindowIsReportedToTheOrganisation(t *testing.T) {
	orgConn, runConn := runnerwire.Loopback()
	got := make(chan runnerwire.Event, 4)
	org := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnEventChecked: func(ev runnerwire.Event) error {
		got <- ev
		return nil
	}})
	run := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = org.Run(ctx) }()
	go func() { _ = run.Run(ctx) }()
	t.Cleanup(func() { org.Close(); run.Close() })

	t.Cleanup(func() { windowReport.Store(nil) })
	runID, at := uuid.New(), time.Now().UTC().Truncate(time.Second)

	unexpectedWindow(runID, "before", at) // no reporter yet: logged only
	reportWindowsOn(ctx, func() *runnerwire.Peer { return nil })
	unexpectedWindow(runID, "offline", at) // no session: logged only
	select {
	case ev := <-got:
		t.Fatalf("an event went out with no session: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}

	reportWindowsOn(ctx, func() *runnerwire.Peer { return run })
	unexpectedWindow(runID, "someone-elses", at)
	select {
	case ev := <-got:
		var w runnerwire.MacWindowUnexpected
		if ev.Kind != runnerwire.EventMacWindowUnexpected || ev.RunID == nil || *ev.RunID != runID || runnerwire.Unmarshal(ev.Data, &w) != nil {
			t.Fatalf("event %+v", ev)
		}
		if w.Grant != "someone-elses" || !w.At.Equal(at) {
			t.Fatalf("payload %+v", w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the unexpected window was never reported")
	}
}

// A runner that serves mac_window says so in its macOS VM caps; the organisation sends the call only to
// one that does.
func TestMacOSVMOffersWindow(t *testing.T) {
	m := newMacLedgerRig(t).open(t)
	m.recompute()
	if c := m.current(); !c.Window {
		t.Fatal("caps: window=false, want true")
	}
}
