// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordingRowStore keeps a run row's recording_level and not_recorded. The separate marker write
// always fails; offErr fails the single off write, which otherwise lands both columns together.
type recordingRowStore struct {
	store.Store
	level  map[uuid.UUID]types.RecordingLevel
	marker map[uuid.UUID]types.RunNotRecorded
	offErr error
	cas    []types.RunState
}

func newRecordingRowStore(offErr error) *recordingRowStore {
	return &recordingRowStore{level: map[uuid.UUID]types.RecordingLevel{}, marker: map[uuid.UUID]types.RunNotRecorded{}, offErr: offErr}
}

func (s *recordingRowStore) SetRunRecordingLevel(_ context.Context, id uuid.UUID, l types.RecordingLevel) error {
	if s.offErr != nil {
		return s.offErr
	}
	s.level[id] = l
	return nil
}

func (s *recordingRowStore) SetRunNotRecorded(context.Context, uuid.UUID, types.RunNotRecorded) error {
	return errors.New("store: set run not recorded: connection reset by peer")
}

func (s *recordingRowStore) SetRunRecordingOff(_ context.Context, id uuid.UUID) error {
	if s.offErr != nil {
		return s.offErr
	}
	s.level[id], s.marker[id] = types.RecordingOff, types.NotRecordedPolicyOff
	return nil
}

func (s *recordingRowStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	s.cas = append(s.cas, from, to)
	return false, nil
}

func offDispatch(t *testing.T, st *recordingRowStore) (*Server, recording.Store, types.AgentRun, runner.SandboxSpec, bool) {
	t.Helper()
	records := uuid.New()
	rs := fsRecordingStore(t)
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken, RecordingStore: rs, Store: st,
		Runner: &poolRunner{fakeRunner: &fakeRunner{}, runnerFacts: map[string]runner.TargetFacts{records.String(): {Records: true}}}})
	run := types.AgentRun{ID: uuid.New(), RunnerID: &records}
	var spec runner.SandboxSpec
	ok := srv.applyRunRecording(context.Background(), &run, &spec, types.RecordingChoice{Policy: types.RecordingOff})
	return srv, rs, run, spec, ok
}

// TestApplyRunRecordingOff_OneWrite: the off decision is stored in one write, so a failing marker write
// on its own cannot leave a row at level off with no mark, and the reloaded run's console records nothing.
func TestApplyRunRecordingOff_OneWrite(t *testing.T) {
	st := newRecordingRowStore(nil)
	srv, rs, run, spec, ok := offDispatch(t, st)
	if !ok || spec.Record == nil || *spec.Record {
		t.Fatalf("dispatch ok=%v record=%v, want started and asked not to record", ok, spec.Record)
	}
	if st.level[run.ID] != types.RecordingOff || st.marker[run.ID] != types.NotRecordedPolicyOff {
		t.Fatalf("stored level %q not_recorded %q, want off policy_off together", st.level[run.ID], st.marker[run.ID])
	}
	reload := types.AgentRun{ID: run.ID, RecordingLevel: st.level[run.ID], NotRecorded: st.marker[run.ID]}
	if tee, finish := srv.newSessionRecorder(reload, "console-1", runner.AttachOptions{Cols: 80, Rows: 24}); tee != nil {
		_, _ = tee.Write([]byte("console output on an off run\r\n"))
		finish(context.Background(), types.ActorHuman, "owner@example.com")
		t.Error("the reloaded off run got a session recorder")
	}
	if rc, err := rs.OpenCast(context.Background(), recording.CastKey(run.ID.String(), "console-1")); err == nil {
		_ = rc.Close()
		t.Error("a cast was stored for the off run")
	}
}

// TestApplyRunRecordingOff_StoreFailsRefusesDispatch: an off decision that cannot be stored fails the
// run before it starts; nothing is written and nothing records it.
func TestApplyRunRecordingOff_StoreFailsRefusesDispatch(t *testing.T) {
	st := newRecordingRowStore(errors.New("store: set run recording off: connection reset by peer"))
	_, _, run, _, ok := offDispatch(t, st)
	if ok {
		t.Fatal("dispatch went on with its off decision unstored")
	}
	if len(st.cas) != 2 || st.cas[0] != types.RunStarting || st.cas[1] != types.RunFailed {
		t.Fatalf("run transitions %v, want starting->failed", st.cas)
	}
	if _, has := st.level[run.ID]; has {
		t.Fatalf("a level was stored: %q", st.level[run.ID])
	}
}

// TestRecordingOffLevelWithoutMark: a row at level off whose mark is empty gets no session recorder and
// its upload is refused; a legacy row (empty level, empty mark) is recorded and uploads as before.
func TestRecordingOffLevelWithoutMark(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken, RecordingStore: fsRecordingStore(t)})
	off := types.AgentRun{ID: uuid.New(), RecordingLevel: types.RecordingOff}
	if tee, _ := srv.newSessionRecorder(off, "s", runner.AttachOptions{}); tee != nil {
		t.Error("a level-off run with no mark got a session recorder")
	}
	w := httptest.NewRecorder()
	if !srv.refuseDeclinedUpload(w, off) || w.Code != http.StatusConflict {
		t.Errorf("level-off upload = %d, want 409 refused", w.Code)
	}
	legacy := types.AgentRun{ID: uuid.New()}
	if tee, _ := srv.newSessionRecorder(legacy, "s", runner.AttachOptions{}); tee == nil {
		t.Error("a legacy run got no session recorder")
	}
	if srv.refuseDeclinedUpload(httptest.NewRecorder(), legacy) {
		t.Error("a legacy run's upload was refused")
	}
}
