// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The management routes read online from the production Hub, not a nil hook.
func TestRunnerHubWiringPG_InventoryReadsTheLiveHub(t *testing.T) {
	f := newAdmissionFixture(t)
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("session never reached READY: %v", s.result())
	}
	if !f.srv.runnerView(f.rn, types.RunnerTally{}, true).Online {
		t.Fatal("a READY runner reads offline on the runners routes")
	}
}

// The runners switch's hook defaults to ending this process's runner sessions.
func TestRunnerHubWiringPG_SwitchOffHookEndsTheSession(t *testing.T) {
	f := newAdmissionFixture(t)
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("session never reached READY: %v", s.result())
	}
	if f.srv.cfg.RunnersDisabled == nil {
		t.Fatal("Config.RunnersDisabled has no default: switching runners off ends no session")
	}
	f.srv.cfg.RunnersDisabled(f.ctx)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived the switch-off hook")
	}
}

// slowDisconnectAudit holds each runner.disconnect row before recording it, so a
// disconnect audit written off the session slot lands after the next connect.
type slowDisconnectAudit struct{ syncAudit }

func (a *slowDisconnectAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == placement.ActionRunnerDisconnect {
		time.Sleep(300 * time.Millisecond)
	}
	return a.syncAudit.Record(ctx, ev)
}

// A replaced session writes runner.disconnect before the new session's
// runner.connect, and each connect carries the stored version, not resumed.
func TestRunnerHubWiringPG_PresenceAuditOrder(t *testing.T) {
	f := newAdmissionFixture(t)
	rec := &slowDisconnectAudit{}
	f.srv.cfg.Audit = rec
	first := f.connect(t, nil, f.liveRow())
	if !f.waitReady(first) {
		t.Fatalf("first session never reached READY: %v", first.result())
	}
	first.cancel()
	<-first.done
	second := f.connect(t, nil, f.liveRow())
	if !f.waitReady(second) {
		t.Fatalf("second session never reached READY: %v", second.result())
	}
	rec.mu.Lock()
	var got []types.AuditEvent
	for _, ev := range rec.events {
		if ev.Target == f.rn.ID.String() && (ev.Action == placement.ActionRunnerConnect || ev.Action == placement.ActionRunnerDisconnect) {
			got = append(got, ev)
		}
	}
	rec.mu.Unlock()
	want := []string{placement.ActionRunnerConnect, placement.ActionRunnerDisconnect, placement.ActionRunnerConnect}
	if len(got) != len(want) {
		t.Fatalf("presence rows = %d, want %v", len(got), want)
	}
	for i, ev := range got {
		if ev.Action != want[i] || ev.Actor != "runner:"+f.rn.ID.String() {
			t.Fatalf("presence row %d = %s by %s, want %s by the runner", i, ev.Action, ev.Actor, want[i])
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if ev.Action == placement.ActionRunnerConnect && (data["version"] != f.rn.Version || data["resumed"] != false) {
			t.Fatalf("runner.connect data = %s, want the stored version and resumed false", ev.Data)
		}
		if _, ok := data["last_seen_at"]; ev.Action == placement.ActionRunnerDisconnect && !ok {
			t.Fatalf("runner.disconnect data = %s, want last_seen_at", ev.Data)
		}
	}
}

// failEveryFourthGetRunner fails the fourth runner read of each session: Serve's two Lookup reads and the
// admission re-read in Hub.start come first, so the fourth is the one the production Connected callback makes.
type failEveryFourthGetRunner struct {
	store.PG
	calls atomic.Int32
}

func (s *failEveryFourthGetRunner) GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error) {
	if s.calls.Add(1)%4 == 0 {
		return types.Runner{}, errors.New("runner read failed")
	}
	return s.PG.GetRunner(ctx, id)
}

// A failing runner read in the production Connected callback (the one runner.connect is written
// from) refuses the session: the runner never reaches READY and no runner.connect row is written.
func TestRunnerHubWiringPG_ConnectedReadFailureRefusesTheSession(t *testing.T) {
	f := newAdmissionFixture(t)
	failing := &failEveryFourthGetRunner{PG: f.st}
	f.srv.runnerHub(failing) // the hub is built once, from this store
	s := f.connect(t, nil, f.liveRow())
	deadline := time.Now().Add(10 * time.Second)
	for failing.calls.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if failing.calls.Load() < 4 {
		t.Fatalf("the Connected read was never made (%d runner reads)", failing.calls.Load())
	}
	time.Sleep(300 * time.Millisecond)
	if f.srv.runnerEndpoint(f.rn.ID).link.Online() {
		t.Fatal("a session whose Connected read failed reached READY")
	}
	rec := f.srv.cfg.Audit.(*syncAudit)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.events {
		if ev.Action == placement.ActionRunnerConnect {
			t.Fatalf("runner.connect written for a refused session: %+v", ev)
		}
	}
	_ = s
}
