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
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
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
// runner.connect, and each connect carries the version the session reported, not resumed.
func TestRunnerHubWiringPG_PresenceAuditOrder(t *testing.T) {
	f := newAdmissionFixture(t)
	f.capsVersion = f.rn.Version
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

// presenceRows is the runner.connect and runner.disconnect rows of f's runner, in order.
func presenceRows(f *admissionFixture) []types.AuditEvent {
	rec := f.srv.cfg.Audit.(*syncAudit)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var got []types.AuditEvent
	for _, ev := range rec.events {
		if ev.Target == f.rn.ID.String() && (ev.Action == placement.ActionRunnerConnect || ev.Action == placement.ActionRunnerDisconnect) {
			got = append(got, ev)
		}
	}
	return got
}

func connectRowVersion(t *testing.T, ev types.AuditEvent) any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data["version"]
}

// A runner that reports version 0.9.3-test has it stored with a last_seen_at, and its runner.connect
// row carries it, not the version the row held before.
func TestRunnerHubWiringPG_ReportedVersionIsRecorded(t *testing.T) {
	f := newAdmissionFixture(t)
	const reported = "0.9.3-test"
	f.capsVersion = reported
	if before, err := f.st.GetRunner(f.ctx, f.rn.ID); err != nil || before.Version == reported || before.LastSeenAt != nil {
		t.Fatalf("precondition: version %q, last seen %v, err %v", before.Version, before.LastSeenAt, err)
	}
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("session never reached READY: %v", s.result())
	}
	got, err := f.st.GetRunner(f.ctx, f.rn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != reported || got.LastSeenAt == nil {
		t.Fatalf("runner row = version %q, last_seen_at %v; want %q and a time", got.Version, got.LastSeenAt, reported)
	}
	rows := presenceRows(f)
	if len(rows) != 1 || rows[0].Action != placement.ActionRunnerConnect || connectRowVersion(t, rows[0]) != reported {
		t.Fatalf("presence rows = %+v, want one runner.connect carrying %q", rows, reported)
	}
}

// A session that ends before it reports caps still writes its runner.connect, ahead of its
// runner.disconnect (which the next fresh session writes), with the version the row held, and records
// last_seen_at.
func TestRunnerHubWiringPG_SessionWithoutCapsKeepsConnectBeforeDisconnect(t *testing.T) {
	f := newAdmissionFixture(t)
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
	rows := presenceRows(f)
	if len(rows) < 2 || rows[0].Action != placement.ActionRunnerConnect || rows[1].Action != placement.ActionRunnerDisconnect {
		t.Fatalf("presence rows = %+v, want connect then disconnect", rows)
	}
	if v := connectRowVersion(t, rows[0]); v != f.rn.Version {
		t.Fatalf("runner.connect version = %v, want the held %q", v, f.rn.Version)
	}
	if got, err := f.st.GetRunner(f.ctx, f.rn.ID); err != nil || got.LastSeenAt == nil || got.Version != f.rn.Version {
		t.Fatalf("runner row = %+v, err %v; want last_seen_at set and the version kept", got, err)
	}
}

// A superseded session's caps, handled after the runner's next session reported its own, write
// nothing over them: the version, machine name, architecture and retire capability stay the current
// session's.
func TestRunnerCapsPG_ASupersededSessionWritesNothing(t *testing.T) {
	pool, cfg, rn, _, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	srv := New(cfg)
	old, cur := uuid.NewString(), uuid.NewString()
	caps := func(session, version, arch string, retire bool) {
		data := mustJSON(runnerwire.Caps{Version: version, MachineName: "host-" + version, Arch: arch, Retire: retire})
		srv.capsFromRunner(ctx, rn.ID, session, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	}
	if err := st.BeginRunnerInventory(ctx, rn.ID, old); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginRunnerInventory(ctx, rn.ID, cur); err != nil {
		t.Fatal(err)
	}
	caps(cur, "0.9.4", "arm64", true)
	caps(old, "0.9.2", "amd64", false)
	got, err := st.GetRunner(ctx, rn.ID)
	if err != nil {
		t.Fatal(err)
	}
	var retire bool
	if err := pool.QueryRow(ctx, `SELECT retire_capable FROM runners WHERE id = $1`, rn.ID).Scan(&retire); err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.9.4" || got.MachineName != "host-0.9.4" || got.Arch != "arm64" || !retire {
		t.Fatalf("runner = version %q, machine %q, arch %q, retire %v; want the current session's 0.9.4, host-0.9.4, arm64, true",
			got.Version, got.MachineName, got.Arch, retire)
	}
}
