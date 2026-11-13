// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunAuditSeq, HasRunAuditEventAfter and SwapWorkspaceRecordResult give recordStore the audit positions of
// store.RecordEvidence: an event's seq is its index in events, plus one.
func (s *recordStore) RunAuditSeq(_ context.Context, runID uuid.UUID) (int64, error) {
	var seq int64
	for i, ev := range s.events {
		if ev.RunID != nil && *ev.RunID == runID {
			seq = int64(i + 1)
		}
	}
	return seq, nil
}

func (s *recordStore) HasRunAuditEventAfter(_ context.Context, runID uuid.UUID, afterSeq int64, f store.AuditFilter) (bool, error) {
	for i, ev := range s.events {
		if int64(i+1) > afterSeq && ev.RunID != nil && *ev.RunID == runID && f.Matches(ev) {
			return true, nil
		}
	}
	return false, nil
}

func (s *recordStore) SwapWorkspaceRecordResult(_ context.Context, _ uuid.UUID, task string, old, result json.RawMessage) (bool, error) {
	cur := s.saved
	if cur == nil {
		cur = s.ws.RecordResults
	}
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(cur, &m)
	var stored, want any
	if json.Unmarshal(m[task], &stored) != nil || json.Unmarshal(old, &want) != nil || !reflect.DeepEqual(stored, want) {
		return false, nil
	}
	m[task] = result
	s.saved = mustJSON(m)
	return true, nil
}

// refusingDrain lands drained events where the record fake's audit reads find them, except the actions in
// refuse, which it refuses as a store refuses a row it will never take.
type refusingDrain struct {
	s      *recordStore
	refuse []string
}

func (d refusingDrain) Record(_ context.Context, ev types.AuditEvent) error {
	for _, a := range d.refuse {
		if ev.Action == a {
			return errors.New("ERROR: new row for relation \"audit_events\" violates check constraint (SQLSTATE 23514)")
		}
	}
	d.s.events = append(d.s.events, ev)
	return nil
}

// positionlessStore is a store with no audit positions to give: only store.Store's methods show through.
type positionlessStore struct{ store.Store }

// positionErrStore is a recordStore whose audit position read fails.
type positionErrStore struct{ *recordStore }

func (positionErrStore) RunAuditSeq(context.Context, uuid.UUID) (int64, error) {
	return 0, errors.New("store: read run audit position: connection reset")
}

func confinedRecording(t *testing.T) (*recordStore, uuid.UUID) {
	t.Helper()
	runID, wsID := uuid.New(), uuid.New()
	ws := recordingWorkspace(wsID, runID, "verify:build")
	ws.RecordResults = mustJSON(map[string]RecordTaskResult{
		"verify:build": {RunID: runID, Label: "build", Mode: recordModeInteractive, Confined: true, Status: recordStatusRecording},
	})
	return &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunCompleted},
		importStateFake: importStateFake{ws: ws},
		events:          []types.AuditEvent{egressAllowEvent(runID, "pypi.org")},
	}, runID
}

func denyEvent(runID uuid.UUID, host string) types.AuditEvent {
	return types.AuditEvent{ID: uuid.New(), RunID: &runID, Action: "egress.deny", Outcome: "denied", Target: host,
		Data: mustJSON(map[string]any{"host": host, "method": "GET", "rule_source": "approval:denied"})}
}

// A deny that lands on the run's trail after its confined replay was stamped clean (its insert was still in
// flight, or it waited in another replica's audit spool) turns the verdict on the next workspace read.
func TestRecordCleanVerdictIsTakenAgainWhenADenyLandsAfterTheCapture(t *testing.T) {
	fake, runID := confinedRecording(t)
	srv := newTestSrv(t, fake)
	ctx := context.Background()

	srv.reconcileRecordRun(ctx, runID)
	if res := fake.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean {
		t.Fatalf("first capture: clean %v, want a stamped true", res.Clean)
	}
	fake.events = append(fake.events, denyEvent(runID, "example.com"))

	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	srv.repairStaleWorkspaceRuns(ctx, ws)
	res := fake.savedResult(t, "verify:build")
	if res.Status != recordStatusRecorded || res.Clean == nil || *res.Clean || res.Caught != 1 {
		t.Fatalf("after the late deny: status %q, clean %v, caught %d; want recorded, not clean, 1 caught", res.Status, res.Clean, res.Caught)
	}
	if res.Label != "build" || res.FinishedAt == nil {
		t.Fatalf("the verdict taken again lost the entry's own fields: label %q, finished_at %v", res.Label, res.FinishedAt)
	}
}

// The verdict taken again is written only over the entry it was read from: a re-record that replaced the
// entry meanwhile is kept.
func TestRecordCleanVerdictTakenAgainNeverOverwritesANewerEntry(t *testing.T) {
	fake, runID := confinedRecording(t)
	srv := newTestSrv(t, fake)
	ctx := context.Background()

	srv.reconcileRecordRun(ctx, runID)
	fake.events = append(fake.events, denyEvent(runID, "example.com"))
	stale, _ := fake.GetWorkspace(ctx, uuid.Nil)
	next := RecordTaskResult{RunID: uuid.New(), Label: "build", Mode: recordModeInteractive, Confined: true, Status: recordStatusRecording}
	if _, applied, err := fake.SetWorkspaceRecordResult(ctx, uuid.Nil, "verify:build", mustJSON(next), ""); err != nil || !applied {
		t.Fatalf("re-record: applied %v, err %v", applied, err)
	}

	stale.ActiveRunID = nil // nothing else for the repair to settle: only the missed swap says the row moved
	read := srv.repairStaleWorkspaceRuns(ctx, stale)
	if res := fake.savedResult(t, "verify:build"); res.RunID != next.RunID || res.Status != recordStatusRecording {
		t.Fatalf("a read of the old entry overwrote the re-record: run %s, status %q", res.RunID, res.Status)
	}
	if res := recordResultsMap(read)["verify:build"]; res.RunID != next.RunID {
		t.Fatalf("the read answered with the entry it found replaced: run %s, clean %v; want the re-record", res.RunID, res.Clean)
	}
}

// trailRaceStore lands land on the run's trail while a capture reads it, after that read took its rows: a
// row that commits between the capture's position read and the end of its trail read.
type trailRaceStore struct {
	*recordStore
	land []types.AuditEvent
}

func (s *trailRaceStore) QueryAuditEvents(ctx context.Context, id uuid.UUID, n int) ([]types.AuditEvent, error) {
	events, err := s.recordStore.QueryAuditEvents(ctx, id, n)
	events = append([]types.AuditEvent(nil), events...)
	s.events, s.land = append(s.events, s.land...), nil
	return events, err
}

// The position is read before the trail, in the first capture and in one taken again: a deny that lands
// while the trail is being read is past the position stored, so the next workspace read finds it. Read
// the other way round, the position would cover a row the capture never saw, and the verdict would stay.
func TestRecordAuditPositionIsReadBeforeTheTrail(t *testing.T) {
	ctx := context.Background()
	read := func(t *testing.T, srv *Server, st *trailRaceStore) RecordTaskResult {
		t.Helper()
		ws, _ := st.GetWorkspace(ctx, uuid.Nil)
		srv.repairStaleWorkspaceRuns(ctx, ws)
		return st.savedResult(t, "verify:build")
	}
	t.Run("first capture", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		st := &trailRaceStore{recordStore: fake, land: []types.AuditEvent{denyEvent(runID, "example.com")}}
		srv := newTestSrv(t, st)
		srv.reconcileRecordRun(ctx, runID)
		if res := st.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean || res.EvidenceSeq != 1 {
			t.Fatalf("first capture: clean %v, evidence_seq %d; want clean, read through position 1", res.Clean, res.EvidenceSeq)
		}
		if res := read(t, srv, st); res.Clean == nil || *res.Clean || res.Caught != 1 {
			t.Fatalf("the deny that landed during the capture's read was never counted: clean %v, caught %d", res.Clean, res.Caught)
		}
	})
	t.Run("taken again", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		st := &trailRaceStore{recordStore: fake}
		srv := newTestSrv(t, st)
		srv.reconcileRecordRun(ctx, runID)
		st.events = append(st.events, egressAllowEvent(runID, "pypi.org"))
		st.land = []types.AuditEvent{denyEvent(runID, "example.com")}
		if res := read(t, srv, st); res.Clean == nil || !*res.Clean || res.EvidenceSeq != 2 {
			t.Fatalf("taken again over the late allow: clean %v, evidence_seq %d; want clean, read through position 2", res.Clean, res.EvidenceSeq)
		}
		if res := read(t, srv, st); res.Clean == nil || *res.Clean || res.Caught != 1 {
			t.Fatalf("the deny that landed while the verdict was taken again was never counted: clean %v, caught %d", res.Clean, res.Caught)
		}
	})
}

// The egress proxy's summary of decisions it dropped is a deny with no host, which the capture leaves out.
// A confined replay whose trail holds one is missing decisions, so it is a failed capture, never a clean
// replay: at the first capture, and when the summary lands after a clean verdict was stamped.
func TestRecordReplayWhoseProxyDroppedDecisionsIsNeverClean(t *testing.T) {
	ctx := context.Background()
	dropped := func(runID uuid.UUID) types.AuditEvent {
		return types.AuditEvent{ID: uuid.New(), RunID: &runID, Action: "egress.deny", Outcome: "denied",
			Data: mustJSON(map[string]any{"host": "", "rule_source": "egress:dropped-decisions-3"})}
	}
	failed := func(t *testing.T, fake *recordStore) {
		t.Helper()
		res := fake.savedResult(t, "verify:build")
		if res.Status != recordStatusFailed || res.Clean != nil || res.FailureHint != recordDecisionsDroppedHint {
			t.Fatalf("status %q, clean %v, hint %q; want record_failed, no verdict, the dropped-decisions hint", res.Status, res.Clean, res.FailureHint)
		}
	}
	t.Run("on the trail at the capture", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		fake.events = append(fake.events, dropped(runID))
		newTestSrv(t, fake).reconcileRecordRun(ctx, runID)
		failed(t, fake)
	})
	t.Run("landed after a clean verdict", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		srv := newTestSrv(t, fake)
		srv.reconcileRecordRun(ctx, runID)
		if res := fake.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean {
			t.Fatalf("first capture: clean %v, want a stamped true", res.Clean)
		}
		fake.events = append(fake.events, dropped(runID))
		ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
		srv.repairStaleWorkspaceRuns(ctx, ws)
		failed(t, fake)
	})
}

// drainUntilQuarantined runs the drain passes that set a refused line aside (spoolPoisonAttempts strikes,
// then a line behind it lands).
func drainUntilQuarantined(t *testing.T, spool *AuditSpool, rec refusingDrain) {
	t.Helper()
	for i := 0; i < spoolPoisonAttempts; i++ {
		_, _ = spool.Drain(context.Background(), rec, 100)
	}
	if spool.Quarantined() != 1 {
		t.Fatalf("quarantined %d lines, want the refused deny set aside", spool.Quarantined())
	}
}

func spoolWithRefusedDeny(t *testing.T, runID uuid.UUID) (*AuditSpool, types.AuditEvent) {
	t.Helper()
	spool, err := NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	deny := denyEvent(runID, "example.com")
	for _, ev := range []types.AuditEvent{deny, newTestEvent("good.one")} {
		if err := spool.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	return spool, deny
}

func assertSetAside(t *testing.T, fake *recordStore, runID uuid.UUID, deny types.AuditEvent) {
	t.Helper()
	res := fake.savedResult(t, "verify:build")
	if res.Status != recordStatusFailed || res.Clean != nil || !strings.Contains(res.FailureHint, "quarantine") {
		t.Fatalf("status %q, clean %v, hint %q; want record_failed, no verdict, the quarantine hint", res.Status, res.Clean, res.FailureHint)
	}
	for _, ev := range fake.events {
		if ev.Action == "audit.spool.quarantine" && ev.RunID != nil && *ev.RunID == runID && ev.Target == deny.ID.String() {
			return
		}
	}
	t.Fatalf("no note on the run's trail names the deny set aside: %+v", fake.events)
}

// A deny the store refuses for good is set aside by the spool, and the spool then reads empty: the capture
// it no longer holds back is missing the deny, so it is a failed capture, never a clean replay.
func TestRecordReplayWithAnEventSetAsideByThisSpoolIsNeverClean(t *testing.T) {
	fake, runID := confinedRecording(t)
	spool, deny := spoolWithRefusedDeny(t, runID)
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.AuditSpool = spool
	srv := New(cfg)

	srv.reconcileRecordRun(context.Background(), runID)
	if fake.saved != nil {
		t.Fatalf("finalized with the deny still in the spool: %s", fake.saved)
	}
	drainUntilQuarantined(t, spool, refusingDrain{s: fake, refuse: []string{"egress.deny"}})
	srv.reconcileRecordRun(context.Background(), runID)
	assertSetAside(t, fake, runID, deny)
}

// The same, when the capture was taken on another replica before this spool set the deny aside: the note
// on the run's trail turns the stamped verdict on the next workspace read.
func TestRecordCleanVerdictFailsWhenAnotherSpoolSetsTheRunsEventAside(t *testing.T) {
	fake, runID := confinedRecording(t)
	srv := newTestSrv(t, fake)
	ctx := context.Background()

	srv.reconcileRecordRun(ctx, runID)
	if res := fake.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean {
		t.Fatalf("first capture: clean %v, want a stamped true", res.Clean)
	}
	spool, deny := spoolWithRefusedDeny(t, runID)
	drainUntilQuarantined(t, spool, refusingDrain{s: fake, refuse: []string{"egress.deny"}})

	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	srv.repairStaleWorkspaceRuns(ctx, ws)
	assertSetAside(t, fake, runID, deny)
}

// A spool that cannot write the note keeps the line, as it does through an outage: an event is never set
// aside without its run's trail saying so.
func TestAuditSpoolKeepsARunsRefusedLineWhenItsNoteIsRefused(t *testing.T) {
	fake, runID := confinedRecording(t)
	spool, _ := spoolWithRefusedDeny(t, runID)
	rec := refusingDrain{s: fake, refuse: []string{"egress.deny", "audit.spool.quarantine"}}
	for i := 0; i < 2*spoolPoisonAttempts; i++ {
		_, _ = spool.Drain(context.Background(), rec, 100)
	}
	if spool.Quarantined() != 0 || spool.Lines() != 1 {
		t.Fatalf("quarantined %d, %d lines left; want the deny kept in the spool, nothing set aside", spool.Quarantined(), spool.Lines())
	}
}

// unstorableDrain is a store that refuses a row for a NUL in its action or in its data, as PostgreSQL
// does (22021 in a text column, 22P05 in jsonb), and takes every other row.
type unstorableDrain struct{ s *recordStore }

func (d unstorableDrain) Record(_ context.Context, ev types.AuditEvent) error {
	if strings.Contains(ev.Action, "\x00") || bytes.Contains(ev.Data, []byte(`\u0000`)) {
		return errors.New("ERROR: invalid byte sequence for encoding \"UTF8\": 0x00 (SQLSTATE 22021)")
	}
	d.s.events = append(d.s.events, ev)
	return nil
}

// An event the store refuses for its own action would have its note refused for the same action, and the
// line would then hold the head of the spool for good. The note is written again without the action, so
// the line still moves, a second refused line behind it is still found, and the run's replay still fails.
func TestAuditSpoolNotesARunsRefusedLineWithoutAnActionTheStoreRefuses(t *testing.T) {
	fake, runID := confinedRecording(t)
	spool, err := NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	deny := denyEvent(runID, "example.com")
	deny.Action = "egress.deny\x00"
	second := newTestEvent("second.bad\x00")
	for _, ev := range []types.AuditEvent{deny, newTestEvent("good.one"), second, newTestEvent("good.two"), newTestEvent("good.three")} {
		if err := spool.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3*spoolPoisonAttempts; i++ {
		_, _ = spool.Drain(context.Background(), unstorableDrain{fake}, 100)
	}
	if spool.Quarantined() != 2 || spool.Lines() != 0 {
		t.Fatalf("quarantined %d, %d lines left; want both refused lines set aside and the spool drained", spool.Quarantined(), spool.Lines())
	}
	srv := newTestSrv(t, fake)
	srv.reconcileRecordRun(context.Background(), runID)
	assertSetAside(t, fake, runID, deny)
}

// A line whose move keeps failing is noted once, not on every drain pass.
func TestAuditSpoolNotesALineItCannotMoveOnce(t *testing.T) {
	fake, runID := confinedRecording(t)
	spool, _ := spoolWithRefusedDeny(t, runID)
	if err := os.Mkdir(spool.quarantinePath(), 0o700); err != nil { // the sidecar cannot be opened for append
		t.Fatal(err)
	}
	rec := refusingDrain{s: fake, refuse: []string{"egress.deny"}}
	for i := 0; i < 3*spoolPoisonAttempts; i++ {
		_, _ = spool.Drain(context.Background(), rec, 100)
	}
	notes := 0
	for _, ev := range fake.events {
		if ev.Action == "audit.spool.quarantine" {
			notes++
		}
	}
	if notes != 1 || spool.Quarantined() != 0 || spool.Lines() != 1 {
		t.Fatalf("%d notes, quarantined %d, %d lines left; want one note for the line that could not move", notes, spool.Quarantined(), spool.Lines())
	}
}

// A store that has no audit position to give leaves a confined replay recording rather than stamp a
// verdict nothing could check again. An open recording needs none and is captured.
func TestRecordConfinedReplayWaitsForAnAuditPosition(t *testing.T) {
	fake, runID := confinedRecording(t)
	srv := newTestSrv(t, positionlessStore{fake})
	srv.reconcileRecordRun(context.Background(), runID)
	if fake.saved != nil {
		t.Fatalf("stamped a confined verdict with no audit position: %s", fake.saved)
	}
	newTestSrv(t, positionErrStore{fake}).reconcileRecordRun(context.Background(), runID)
	if fake.saved != nil {
		t.Fatalf("stamped a confined verdict over a failed position read: %s", fake.saved)
	}

	runID, wsID := uuid.New(), uuid.New()
	open := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunCompleted},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
		events:          []types.AuditEvent{egressAllowEvent(runID, "pypi.org")},
	}
	newTestSrv(t, positionlessStore{open}).reconcileRecordRun(context.Background(), runID)
	if res := open.savedResult(t, "build"); res.Status != recordStatusRecorded {
		t.Fatalf("open recording: status %q, want recorded", res.Status)
	}
}
