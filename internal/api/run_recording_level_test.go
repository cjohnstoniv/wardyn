// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func fsRecordingStore(t *testing.T) recording.Store {
	t.Helper()
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// TestApplyRunRecordingLevel is the dispatch decision per level. Its first row is the golden: nothing
// chooses a level, and the run is asked to record, is not marked and keeps terminal, exactly as before
// levels. The store rows pin WARDYN_RECORDING_STORE=off as the hard off whatever the policy says.
func TestApplyRunRecordingLevel(t *testing.T) {
	locked := &types.SiteRecording{Level: types.RecordingTerminal, Locked: true}
	for _, tc := range []struct {
		name      string
		store     bool
		choice    types.RecordingChoice
		record    bool
		notRec    types.RunNotRecorded
		wantLevel types.RecordingLevel
	}{
		{"golden: nothing set", true, types.RecordingChoice{}, true, "", types.RecordingTerminal},
		{"policy screen", true, types.RecordingChoice{Policy: types.RecordingScreenActions}, true, "", types.RecordingScreenActions},
		{"policy off", true, types.RecordingChoice{Policy: types.RecordingOff}, false, types.NotRecordedPolicyOff, types.RecordingOff},
		{"policy off under a locked minimum", true, types.RecordingChoice{Policy: types.RecordingOff, Org: locked}, true, "", types.RecordingTerminal},
		{"store off, nothing set", false, types.RecordingChoice{}, false, types.NotRecordedOrgOff, types.RecordingTerminal},
		{"store off, policy screen", false, types.RecordingChoice{Policy: types.RecordingScreenActions}, false, types.NotRecordedOrgOff, types.RecordingScreenActions},
		{"store off, policy off", false, types.RecordingChoice{Policy: types.RecordingOff}, false, types.NotRecordedOrgOff, types.RecordingOff},
	} {
		records := uuid.New()
		s := &Server{cfg: Config{Runner: &poolRunner{fakeRunner: &fakeRunner{}, runnerFacts: map[string]runner.TargetFacts{
			records.String(): {Records: true},
		}}}}
		if tc.store {
			s.cfg.RecordingStore = fsRecordingStore(t)
		}
		run := types.AgentRun{ID: uuid.New(), RunnerID: &records}
		var spec runner.SandboxSpec
		if !s.applyRunRecording(context.Background(), &run, &spec, tc.choice) {
			t.Fatalf("%s: dispatch refused with no required setting", tc.name)
		}
		if spec.Record == nil || *spec.Record != tc.record || run.NotRecorded != tc.notRec || run.RecordingLevel != tc.wantLevel {
			t.Errorf("%s: record=%v not_recorded=%q level=%q, want %v %q %q", tc.name, spec.Record, run.NotRecorded,
				run.RecordingLevel, tc.record, tc.notRec, tc.wantLevel)
		}
	}
}

// TestRecordingChoiceAtDispatch: a member's inline policy may not go under the organisation's level
// while their ceiling chooses none, a stored policy's level stands, and an organisation document that
// cannot be read is a locked terminal minimum.
func TestRecordingChoiceAtDispatch(t *testing.T) {
	off := types.RunPolicySpec{Recording: types.RecordingOff}
	member := dispatchCeiling{recordingBound: true}
	stored := uuid.New()
	for _, tc := range []struct {
		name    string
		run     types.AgentRun
		c       dispatchCeiling
		siteErr error
		want    types.RecordingLevel
	}{
		{"operator inline off", types.AgentRun{}, dispatchCeiling{}, nil, types.RecordingOff},
		{"member inline off", types.AgentRun{}, member, nil, types.RecordingTerminal},
		{"member stored off", types.AgentRun{PolicyID: &stored}, member, nil, types.RecordingOff},
		{"member inline off, ceiling chose off", types.AgentRun{}, dispatchCeiling{recordingBound: true, recording: types.RecordingOff}, nil, types.RecordingOff},
		{"site unreadable", types.AgentRun{}, dispatchCeiling{}, context.DeadlineExceeded, types.RecordingTerminal},
	} {
		if got := recordingChoice(tc.run, off, tc.c, types.SiteConfig{}, tc.siteErr).Resolve(); got != tc.want {
			t.Errorf("%s: level %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestNewSessionRecorder_PolicyOffRecordsNothing: a run whose level resolved to off gets no console or
// SSH cast, while the same run unmarked is recorded.
func TestNewSessionRecorder_PolicyOffRecordsNothing(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken, RecordingStore: fsRecordingStore(t)})
	if tee, _ := srv.newSessionRecorder(types.AgentRun{ID: uuid.New(), NotRecorded: types.NotRecordedPolicyOff}, "s", runner.AttachOptions{}); tee != nil {
		t.Error("a policy_off run got a session recorder")
	}
	if tee, _ := srv.newSessionRecorder(types.AgentRun{ID: uuid.New()}, "s", runner.AttachOptions{}); tee == nil {
		t.Error("an unmarked run got no session recorder")
	}
}

// TestRefuseDeclinedUpload_PolicyOff: a cast for a run resolved to off is refused, not kept.
func TestRefuseDeclinedUpload_PolicyOff(t *testing.T) {
	w := httptest.NewRecorder()
	if !(&Server{}).refuseDeclinedUpload(w, types.AgentRun{NotRecorded: types.NotRecordedPolicyOff}) || w.Code != http.StatusConflict {
		t.Fatalf("policy_off upload = %d, want 409 refused", w.Code)
	}
}

// TestSidecarPolicyDropsRecording: the proxy is never sent the level (a previous proxy refuses it).
func TestSidecarPolicyDropsRecording(t *testing.T) {
	if got := sidecarPolicy(types.RunPolicySpec{Recording: types.RecordingScreenActions}, dispatchParams{}); got.Recording != "" {
		t.Fatalf("sidecar policy carries recording %q", got.Recording)
	}
}

// TestSiteConfigRecording_Put: a level that is not one is refused; locking a minimum while the
// organisation keeps no recordings is refused with the board's sentence; with a store it saves.
func TestSiteConfigRecording_Put(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"recording":{"level":"keystrokes"}}`); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "recording.level") {
		t.Errorf("keystrokes level = %d %s, want 400 naming recording.level", w.Code, w.Body.String())
	}
	srv.cfg.RecordingStore = nil
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"recording":{"level":"terminal","locked":true}}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), runnerpool.RecordingSaveStoreOffMsg()) {
		t.Errorf("locked minimum with no store = %d %s, want 422 with the save refusal", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Errorf("refused saves wrote %+v", fake.putSeen)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"recording":{"level":"off","locked":true}}`); w.Code != http.StatusOK {
		t.Errorf("locking off with no store = %d %s, want 200", w.Code, w.Body.String())
	}
	srv.cfg.RecordingStore = fsRecordingStore(t)
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"recording":{"level":"screen_actions","locked":true}}`); w.Code != http.StatusOK {
		t.Errorf("locked minimum with a store = %d %s, want 200", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.Recording == nil || *fake.putSeen.Recording != (types.SiteRecording{Level: types.RecordingScreenActions, Locked: true}) {
		t.Errorf("saved %+v", fake.putSeen)
	}
}
