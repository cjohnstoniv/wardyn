// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
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

// A runner's report of decisions its spool lost (builtin:spool-lost) is, like the proxy's summary, a deny with no
// host, and it stands for decisions of requests that were sent. A confined replay whose trail holds one is a
// failed capture, never a clean replay: at the first capture, and when the report lands after a clean verdict
// was stamped.
func TestRecordReplayWithDecisionsTheSpoolLostIsNeverClean(t *testing.T) {
	ctx := context.Background()
	lost := func(runID uuid.UUID) types.AuditEvent {
		return types.AuditEvent{ID: uuid.New(), RunID: &runID, Action: "egress.deny", Outcome: "denied",
			Data: mustJSON(map[string]any{"host": "", "rule_source": "builtin:spool-lost"})}
	}
	failed := func(t *testing.T, fake *recordStore) {
		t.Helper()
		res := fake.savedResult(t, "verify:build")
		if res.Status != recordStatusFailed || res.Clean != nil || res.FailureHint != recordDecisionsLostHint {
			t.Fatalf("status %q, clean %v, hint %q; want record_failed, no verdict, the lost-decisions hint", res.Status, res.Clean, res.FailureHint)
		}
	}
	t.Run("on the trail at the capture", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		fake.events = append(fake.events, lost(runID))
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
		fake.events = append(fake.events, lost(runID))
		ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
		srv.repairStaleWorkspaceRuns(ctx, ws)
		failed(t, fake)
	})
}

// A runner's count of the records its full spool refused (builtin:spool-full) is a deny with no host. It stands
// for requests refused at admission, which the policy never judged, and for decisions of requests that were not
// sent: either could have been a deny. A confined replay whose trail holds one is a failed capture, never a clean
// replay: at the first capture, and when the count lands after a clean verdict was stamped.
func TestRecordReplayWhoseSpoolWasFullIsNeverClean(t *testing.T) {
	ctx := context.Background()
	full := func(runID uuid.UUID) types.AuditEvent {
		return types.AuditEvent{ID: uuid.New(), RunID: &runID, Action: "egress.deny", Outcome: "denied",
			Data: mustJSON(map[string]any{"host": "", "rule_source": "builtin:spool-full", "spooled": true})}
	}
	failed := func(t *testing.T, fake *recordStore) {
		t.Helper()
		res := fake.savedResult(t, "verify:build")
		if res.Status != recordStatusFailed || res.Clean != nil || res.FailureHint != recordSpoolFullHint {
			t.Fatalf("status %q, clean %v, hint %q; want record_failed, no verdict, the spool-full hint", res.Status, res.Clean, res.FailureHint)
		}
	}
	t.Run("on the trail at the capture", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		fake.events = append(fake.events, full(runID))
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
		fake.events = append(fake.events, full(runID))
		ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
		srv.repairStaleWorkspaceRuns(ctx, ws)
		failed(t, fake)
	})
}

// refusedDecisionPost is the row internalAuth writes on a run's trail when it refuses a post to the decision door.
func refusedDecisionPost(runID uuid.UUID) types.AuditEvent {
	return types.AuditEvent{ID: uuid.New(), RunID: &runID, ActorType: types.ActorAgent, Action: "authz.denied",
		Target: "/api/v1/internal/decisions", Outcome: "denied",
		Data: mustJSON(map[string]any{"reason": "run_terminal", "run_state": "COMPLETED", "method": "POST"})}
}

// A post the decision door refused (one sent after the run's tail grace, say) is not on the trail, and it could
// have been a deny: a confined replay whose trail holds the refusal is a failed capture, never a clean replay,
// at the first capture and when the refusal lands after a clean verdict was stamped. A refusal at any other
// door is itself on the trail and lost nothing.
func TestRecordReplayWithARefusedDecisionPostIsNeverClean(t *testing.T) {
	ctx := context.Background()
	failed := func(t *testing.T, fake *recordStore) {
		t.Helper()
		res := fake.savedResult(t, "verify:build")
		if res.Status != recordStatusFailed || res.Clean != nil || res.FailureHint != recordDecisionRefusedHint {
			t.Fatalf("status %q, clean %v, hint %q; want record_failed, no verdict, the refused-report hint", res.Status, res.Clean, res.FailureHint)
		}
	}
	t.Run("on the trail at the capture", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		fake.events = append(fake.events, refusedDecisionPost(runID))
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
		fake.events = append(fake.events, refusedDecisionPost(runID))
		ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
		srv.repairStaleWorkspaceRuns(ctx, ws)
		failed(t, fake)
	})
	t.Run("a refusal at another door loses nothing", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		mint := refusedDecisionPost(runID)
		mint.Target = InternalPathPrefix + "credentials/mint"
		fake.events = append(fake.events, mint)
		newTestSrv(t, fake).reconcileRecordRun(ctx, runID)
		if res := fake.savedResult(t, "verify:build"); res.Status != recordStatusRecorded || res.Clean == nil || !*res.Clean {
			t.Fatalf("status %q, clean %v, hint %q; want the replay recorded clean", res.Status, res.Clean, res.FailureHint)
		}
	})
}

// lostDecisionPost is the row the decision door writes on a run's trail for a post it could not take.
func lostDecisionPost(runID uuid.UUID) types.AuditEvent {
	return types.AuditEvent{ID: uuid.New(), RunID: &runID, ActorType: types.ActorSystem, Action: runReportLostAction,
		Target: "/api/v1/internal/decisions", Outcome: "failure", Data: mustJSON(map[string]any{"reason": "token_expired"})}
}

// A post the decision door could not take (a 503, or a 401 for the run's expired token) is not on the trail
// either: a confined replay whose trail notes one is a failed capture, at the first capture and when the note
// lands after a clean verdict, and a list read of the workspace takes the verdict again as a read of the one
// workspace does.
func TestRecordReplayWithALostDecisionPostIsNeverClean(t *testing.T) {
	ctx := context.Background()
	failed := func(t *testing.T, res RecordTaskResult) {
		t.Helper()
		if res.Status != recordStatusFailed || res.Clean != nil || res.FailureHint != recordDecisionRefusedHint {
			t.Fatalf("status %q, clean %v, hint %q; want record_failed, no verdict, the refused-report hint", res.Status, res.Clean, res.FailureHint)
		}
	}
	t.Run("on the trail at the capture", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		fake.events = append(fake.events, lostDecisionPost(runID))
		newTestSrv(t, fake).reconcileRecordRun(ctx, runID)
		failed(t, fake.savedResult(t, "verify:build"))
	})
	t.Run("landed after a clean verdict, read by the list", func(t *testing.T) {
		fake, runID := confinedRecording(t)
		srv := newTestSrv(t, fake)
		srv.reconcileRecordRun(ctx, runID)
		ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
		if listed := recordResultsMap(srv.rejudgedForList(ctx, ws))["verify:build"]; listed.Clean == nil || !*listed.Clean {
			t.Fatalf("listed before the note: clean %v, want the stamped true", listed.Clean)
		}
		fake.events = append(fake.events, lostDecisionPost(runID))
		failed(t, recordResultsMap(srv.rejudgedForList(ctx, ws))["verify:build"])
		failed(t, fake.savedResult(t, "verify:build"))
	})
}

// confinedReplayDoor is confinedRecording whose run ended `ended` ago, behind a server whose audit rows land on
// the fake's trail, with the run's own token for the decision door. revoked revokes the run's identity first, as
// the completion path does before it stops the sandbox (finalizeRunEffects).
func confinedReplayDoor(t *testing.T, ended time.Duration, revoked bool) (*recordStore, uuid.UUID, *Server, string) {
	t.Helper()
	fake, runID := confinedRecording(t)
	fake.run.UpdatedAt = time.Now().UTC().Add(-ended)
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.Audit = refusingDrain{s: fake}
	tok := h.mintRunToken(t, runID)
	if revoked {
		if err := h.idp.RevokeRun(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
	}
	return fake, runID, New(cfg), tok
}

// A deny the egress proxy posts just after its run ended, as it drains at teardown and so on the run's revoked
// token, is kept on the run's trail and turns the replay's clean verdict on the next workspace read; past the
// tail grace the door refuses it, and its refusal fails the replay. Either way the verdict is never left clean.
func TestRecordDenyPostedAfterTheRunEndedTurnsTheVerdict(t *testing.T) {
	ctx := context.Background()
	const deny = `{"request":{"host":"example.com","method":"GET"},"decision":"deny","rule_source":"policy"}`
	for name, tc := range map[string]struct {
		ended   time.Duration
		revoked bool
		status  string
		hint    string
	}{
		"before the revoke":   {time.Second, false, recordStatusRecorded, ""},
		"after the revoke":    {time.Second, true, recordStatusRecorded, ""},
		"past the tail grace": {terminalUploadGrace + time.Minute, true, recordStatusFailed, recordDecisionRefusedHint},
	} {
		t.Run(name, func(t *testing.T) {
			fake, runID, srv, tok := confinedReplayDoor(t, tc.ended, tc.revoked)
			srv.reconcileRecordRun(ctx, runID)
			if res := fake.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean {
				t.Fatalf("first capture: clean %v, want a stamped true", res.Clean)
			}
			w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, deny)
			ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
			srv.repairStaleWorkspaceRuns(ctx, ws)
			res := fake.savedResult(t, "verify:build")
			if res.Status != tc.status || res.FailureHint != tc.hint || (res.Clean != nil && *res.Clean) {
				t.Fatalf("after a deny posted %s after the run ended (answered %d): status %q, clean %v, caught %d, hint %q; want %q, not clean, hint %q",
					tc.ended, w.Code, res.Status, res.Clean, res.Caught, res.FailureHint, tc.status, tc.hint)
			}
			if tc.status == recordStatusRecorded && (w.Code != http.StatusAccepted || res.Clean == nil || res.Caught != 1) {
				t.Fatalf("inside the grace: answered %d, clean %v, caught %d; want 202 and the deny caught", w.Code, res.Clean, res.Caught)
			}
		})
	}
}

// Past the tail grace every post on the run's revoked token is refused, and only the first leaves a row on the
// run's trail (claimDecisionRefusal): many refused posts leave one, and that one fails the replay.
func TestRecordRefusedDecisionPostsLeaveOneRowThatFailsTheReplay(t *testing.T) {
	ctx := context.Background()
	const deny = `{"request":{"host":"example.com","method":"GET"},"decision":"deny","rule_source":"policy"}`
	fake, runID, srv, tok := confinedReplayDoor(t, terminalUploadGrace+time.Minute, true)
	srv.reconcileRecordRun(ctx, runID)
	for i := range 5 {
		if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, deny); w.Code != http.StatusForbidden {
			t.Fatalf("post %d past the grace answered %d, want 403; body=%s", i, w.Code, w.Body.String())
		}
	}
	rows := 0
	for _, ev := range fake.events {
		if ev.Action == "authz.denied" && ev.Target == decisionsDoor && ev.RunID != nil && *ev.RunID == runID {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("refusal rows on the run's trail = %d after 5 refused posts, want 1", rows)
	}
	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	srv.repairStaleWorkspaceRuns(ctx, ws)
	if res := fake.savedResult(t, "verify:build"); res.Status != recordStatusFailed || res.Clean != nil || res.FailureHint != recordDecisionRefusedHint {
		t.Fatalf("status %q, clean %v, hint %q; want record_failed, no verdict, the refused-report hint", res.Status, res.Clean, res.FailureHint)
	}
}

// A local run's token posting to the decision door off its runner's relay is refused on every post, inside the
// tail grace too, and leaves one refusal row on the run's trail, which fails the replay.
func TestRecordOffRelayDecisionPostsLeaveOneRowThatFailsTheReplay(t *testing.T) {
	ctx := context.Background()
	const deny = `{"request":{"host":"example.com","method":"GET"},"decision":"deny","rule_source":"policy"}`
	fake, runID, srv, tok := confinedReplayDoor(t, time.Second, true)
	srv.reconcileRecordRun(ctx, runID)
	runner := uuid.New()
	fake.run.Placement, fake.run.RunnerID = types.PlacementLocal, &runner
	for i := range 5 {
		w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, deny)
		if w.Code != http.StatusForbidden || reasonOf(t, w) != string(placement.ReasonRelayRequired) {
			t.Fatalf("post %d off the relay answered %d; body=%s", i, w.Code, w.Body.String())
		}
	}
	rows := 0
	for _, ev := range fake.events {
		if ev.Action == "authz.denied" && ev.Target == decisionsDoor && ev.RunID != nil && *ev.RunID == runID {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("refusal rows on the run's trail = %d after 5 posts off the relay, want 1", rows)
	}
	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	srv.repairStaleWorkspaceRuns(ctx, ws)
	if res := fake.savedResult(t, "verify:build"); res.Status != recordStatusFailed || res.Clean != nil {
		t.Fatalf("status %q, clean %v; want record_failed and no verdict", res.Status, res.Clean)
	}
}

// pendingReadFailsStore is recordStore for a run on a runner whose pending runner actions cannot be read.
type pendingReadFailsStore struct {
	*recordStore
	store.RunnerStore
}

func (pendingReadFailsStore) RunHasPendingRunnerAction(context.Context, uuid.UUID) (bool, error) {
	return false, errors.New("conn reset")
}

// A relayed decision post whose relay fence cannot read the run's pending runner actions is a retryable 503
// the proxy does not send again: the door notes it once on the run's trail, and the replay fails on the note.
func TestRecordRelayedPostTheFenceCannotReadFailsTheReplay(t *testing.T) {
	ctx := context.Background()
	const deny = `{"request":{"host":"example.com","method":"GET"},"decision":"deny","rule_source":"policy"}`
	fake, runID := confinedRecording(t)
	fake.run.UpdatedAt = time.Now().UTC().Add(-time.Second)
	runner := uuid.New()
	fake.run.Placement, fake.run.RunnerID = types.PlacementLocal, &runner
	h := newHarness(t)
	cfg := baseTestConfig(h, pendingReadFailsStore{recordStore: fake})
	cfg.Audit = refusingDrain{s: fake}
	tok := h.mintRunToken(t, runID)
	srv := New(cfg)
	srv.reconcileRecordRun(ctx, runID)
	if res := fake.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean {
		t.Fatalf("first capture: clean %v, want a stamped true", res.Clean)
	}
	relayed := func(tb testing.TB, next http.Handler) http.Handler {
		inner := panicFails(tb, next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner.ServeHTTP(w, r.WithContext(relayConnContext(r.Context(), relayedConn{runner: runner})))
		})
	}
	for range 3 {
		w := doVia(t, relayed, srv, http.MethodPost, "/api/v1/internal/decisions", tok, deny)
		if w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonInternalLivenessReadFailed {
			t.Fatalf("a relayed post the fence cannot read answered %d; want a retryable 503; body=%s", w.Code, w.Body.String())
		}
	}
	rows := 0
	for _, ev := range fake.events {
		if ev.Action == runReportLostAction && ev.Target == decisionsDoor && ev.RunID != nil && *ev.RunID == runID {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("run.report.drop rows on the run's trail = %d after 3 relayed posts the fence could not read, want 1", rows)
	}
	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	srv.repairStaleWorkspaceRuns(ctx, ws)
	if res := fake.savedResult(t, "verify:build"); res.Status != recordStatusFailed || res.FailureHint != recordDecisionRefusedHint {
		t.Fatalf("status %q, clean %v, hint %q; want record_failed on the refused-report hint", res.Status, res.Clean, res.FailureHint)
	}
}

// The proxy's summary of the decisions it dropped is posted through the same door as they were, and at teardown
// on the run's revoked token: inside the tail grace it is kept, and the replay fails on it.
func TestRecordDroppedSummaryOnARevokedTokenFailsTheReplay(t *testing.T) {
	ctx := context.Background()
	fake, runID, srv, tok := confinedReplayDoor(t, time.Second, true)
	srv.reconcileRecordRun(ctx, runID)
	const summary = `{"request":{},"decision":"deny","rule_source":"egress:dropped-decisions-2"}`
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, summary); w.Code != http.StatusAccepted {
		t.Fatalf("the dropped-decisions summary on a revoked token inside the grace answered %d, want 202; body=%s", w.Code, w.Body.String())
	}
	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	srv.repairStaleWorkspaceRuns(ctx, ws)
	if res := fake.savedResult(t, "verify:build"); res.Status != recordStatusFailed || res.Clean != nil || res.FailureHint != recordDecisionsDroppedHint {
		t.Fatalf("status %q, clean %v, hint %q; want record_failed on the dropped-decisions hint", res.Status, res.Clean, res.FailureHint)
	}
}

// A post on the run's expired token, mid-run or after it ended, is refused with a 401 and lost: the door notes
// it once on the run's trail, and the replay fails on the note. A token that expired long ago cannot fail the
// replay of a run that is over: its posts leave the clean verdict as it was.
func TestRecordPostOnAnExpiredTokenFailsTheReplay(t *testing.T) {
	ctx := context.Background()
	fake, runID := confinedRecording(t)
	cfg := baseTestConfig(newHarness(t), fake)
	tokens := &expiredIdentity{token: expiredIdentityToken, runID: runID, expiry: time.Now().Add(-2 * time.Hour)}
	cfg.Identity = tokens
	cfg.Audit = refusingDrain{s: fake}
	srv := New(cfg)
	srv.reconcileRecordRun(ctx, runID)
	if res := fake.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean {
		t.Fatalf("first capture: clean %v, want a stamped true", res.Clean)
	}
	const deny = `{"request":{"host":"example.com","method":"GET"},"decision":"deny","rule_source":"policy"}`
	post := func() {
		t.Helper()
		for range 3 {
			if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", expiredIdentityToken, deny); w.Code != http.StatusUnauthorized {
				t.Fatalf("a post on an expired token answered %d, want 401; body=%s", w.Code, w.Body.String())
			}
		}
	}
	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	post()
	srv.repairStaleWorkspaceRuns(ctx, ws)
	if res := fake.savedResult(t, "verify:build"); res.Status == recordStatusFailed || res.Clean == nil || !*res.Clean {
		t.Fatalf("status %q, clean %v after posts on a token that expired long ago; want the clean verdict kept", res.Status, res.Clean)
	}
	tokens.expiry = time.Now().Add(-2 * time.Minute)
	post()
	srv.repairStaleWorkspaceRuns(ctx, ws)
	if res := fake.savedResult(t, "verify:build"); res.Status != recordStatusFailed || res.FailureHint != recordDecisionRefusedHint {
		t.Fatalf("status %q, clean %v, hint %q; want record_failed on the refused-report hint", res.Status, res.Clean, res.FailureHint)
	}
}

// A decision post whose body does not decode is a 400 the proxy counts among the posts it dropped: the door
// notes it once on the run's trail, and the replay fails on the note.
func TestRecordDecisionPostThatDoesNotDecodeFailsTheReplay(t *testing.T) {
	ctx := context.Background()
	fake, runID, srv, tok := confinedReplayDoor(t, time.Second, false)
	srv.reconcileRecordRun(ctx, runID)
	if res := fake.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean {
		t.Fatalf("first capture: clean %v, want a stamped true", res.Clean)
	}
	for range 3 {
		w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, `{"request":`)
		if w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonInternalDecisionLogInvalid {
			t.Fatalf("a post that does not decode answered %d; body=%s", w.Code, w.Body.String())
		}
	}
	rows := 0
	for _, ev := range fake.events {
		if ev.Action == runReportLostAction && ev.Target == decisionsDoor && ev.RunID != nil && *ev.RunID == runID &&
			strings.Contains(string(ev.Data), `"reason":"`+reportLostInvalid+`"`) {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("run.report.drop rows = %d after 3 posts that do not decode, want 1", rows)
	}
	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	srv.repairStaleWorkspaceRuns(ctx, ws)
	if res := fake.savedResult(t, "verify:build"); res.Status != recordStatusFailed || res.FailureHint != recordDecisionRefusedHint {
		t.Fatalf("status %q, clean %v, hint %q; want record_failed on the refused-report hint", res.Status, res.Clean, res.FailureHint)
	}
}

// A decision whose egress row is neither written nor spooled is a retryable 503, not a 202: the proxy counts
// the post among those it dropped, and its summary of them goes through the door. A row that was kept is a 202.
func TestDecisionDoorAnswersA503ForADecisionItCouldNotKeep(t *testing.T) {
	fake, runID := confinedRecording(t)
	fake.run.UpdatedAt = time.Now().UTC().Add(-time.Second)
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.Audit = refusingDrain{s: fake, refuse: []string{"egress.deny"}}
	tok := h.mintRunToken(t, runID)
	srv := New(cfg)
	const deny = `{"request":{"host":"example.com","method":"GET"},"decision":"deny","rule_source":"policy"}`
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, deny); w.Code != http.StatusServiceUnavailable ||
		reasonOf(t, w) != reasonInternalDecisionUnrecorded {
		t.Fatalf("a decision whose row was not kept answered %d; want a retryable 503; body=%s", w.Code, w.Body.String())
	}
	const allow = `{"request":{"host":"pypi.org","method":"GET"},"decision":"allow","rule_source":"policy"}`
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, allow); w.Code != http.StatusAccepted {
		t.Fatalf("a decision whose row was kept answered %d, want 202; body=%s", w.Code, w.Body.String())
	}
}

// listedRecordStore lists the one workspace recordStore holds.
type listedRecordStore struct{ *recordStore }

func (s listedRecordStore) ListWorkspaces(ctx context.Context) ([]types.Workspace, error) {
	ws, err := s.GetWorkspace(ctx, uuid.Nil)
	return []types.Workspace{ws}, err
}

// GET /workspaces hands out each row as GET /workspaces/{id} does: a clean confined verdict whose run's trail
// moved is taken again before it is listed.
func TestListWorkspacesTakesACleanVerdictAgain(t *testing.T) {
	ctx := context.Background()
	fake, runID := confinedRecording(t)
	srv := newTestSrv(t, listedRecordStore{fake})
	srv.reconcileRecordRun(ctx, runID)
	if res := fake.savedResult(t, "verify:build"); res.Clean == nil || !*res.Clean {
		t.Fatalf("first capture: clean %v, want a stamped true", res.Clean)
	}
	fake.events = append(fake.events, refusedDecisionPost(runID))
	w := do(t, srv, http.MethodGet, "/api/v1/workspaces", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: code = %d; body=%s", w.Code, w.Body.String())
	}
	var got []types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != 1 {
		t.Fatalf("decode %d rows: %v", len(got), err)
	}
	if res := recordResultsMap(got[0])["verify:build"]; res.Status != recordStatusFailed || res.Clean != nil {
		t.Fatalf("listed status %q, clean %v; want the verdict taken again (record_failed)", res.Status, res.Clean)
	}
}

// What the proxy posts as it drains at teardown, on the run's revoked token, is ordinary: an allow of a host the
// replay already reached, a DNS row and a model call's token counts are kept inside the tail grace, and the
// replay stays clean. A refusal of each would have failed a replay that lost nothing.
func TestRecordReplayKeepsItsVerdictForTailReportsThatLostNothing(t *testing.T) {
	ctx := context.Background()
	fake, runID, srv, tok := confinedReplayDoor(t, time.Second, true)
	srv.reconcileRecordRun(ctx, runID)
	for _, body := range []string{
		`{"request":{"host":"pypi.org","method":"GET"},"decision":"allow","rule_source":"policy"}`,
		`{"request":{},"dns":{"name":"pypi.org","type":"A","outcome":"refused","via":"proxy"}}`,
		`{"request":{"host":"api.anthropic.com"},"rule_source":"llm:usage","usage":{}}`,
	} {
		if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, body); w.Code != http.StatusAccepted {
			t.Fatalf("a tail report just after the run ended answered %d, want 202: %s; body=%s", w.Code, body, w.Body.String())
		}
	}
	ws, _ := fake.GetWorkspace(ctx, uuid.Nil)
	srv.repairStaleWorkspaceRuns(ctx, ws)
	if res := fake.savedResult(t, "verify:build"); res.Status != recordStatusRecorded || res.Clean == nil || !*res.Clean {
		t.Fatalf("status %q, clean %v, hint %q; want the replay still recorded clean", res.Status, res.Clean, res.FailureHint)
	}
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
