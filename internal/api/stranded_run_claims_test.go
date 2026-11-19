// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// strandedClaimStore releases one workspace claim and serves the workspace with the stranded run's
// recording entry beside another run's.
type strandedClaimStore struct {
	store.Store
	ws     types.Workspace
	run    uuid.UUID
	writes map[string]RecordTaskResult
	fences []string
}

func (s *strandedClaimStore) ReleaseStrandedRunClaims(context.Context, time.Duration) ([]store.StrandedRunClaim, error) {
	return []store.StrandedRunClaim{{Kind: "workspace", ID: s.ws.ID, RunID: s.run}}, nil
}

func (s *strandedClaimStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	if id != s.ws.ID {
		return types.Workspace{}, store.ErrNotFound
	}
	return s.ws, nil
}

func (s *strandedClaimStore) SetWorkspaceRecordResult(_ context.Context, _ uuid.UUID, key string, raw json.RawMessage, onlyIf string) (types.Workspace, bool, error) {
	var res RecordTaskResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return types.Workspace{}, false, err
	}
	s.writes[key], s.fences = res, append(s.fences, onlyIf)
	return s.ws, true, nil
}

// TestReleaseStrandedRunClaims_FailsTheStrandedRecording: a released workspace claim fails that run's
// recording entry, fenced on it still recording, with the record card's fixed sentence, and leaves the
// other run's entry alone; the run watcher's slow pass runs the release.
func TestReleaseStrandedRunClaims_FailsTheStrandedRecording(t *testing.T) {
	stranded, other := uuid.New(), uuid.New()
	st := &strandedClaimStore{run: stranded, writes: map[string]RecordTaskResult{}}
	st.ws = types.Workspace{ID: uuid.New(), RecordResults: mustJSON(map[string]RecordTaskResult{
		"build":  {RunID: stranded, Label: "build", Status: recordStatusRecording},
		"verify": {RunID: other, Label: "verify", Status: recordStatusRecording},
	})}
	h := newHarness(t)
	h.srv.cfg.Store = st
	if err := h.srv.releaseStrandedRunClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok := st.writes["build"]
	if len(st.writes) != 1 || !ok {
		t.Fatalf("writes = %+v, want the build entry alone", st.writes)
	}
	if got.Status != recordStatusFailed || got.FinishedAt == nil || got.FailureHint != recordLaunchFailureHint(nil) || got.Label != "build" {
		t.Errorf("build entry = %+v, want record_failed with a finish time, the fixed hint and its label", got)
	}
	if len(st.fences) != 1 || st.fences[0] != recordStatusRecording {
		t.Errorf("fences = %v, want the write fenced on recording", st.fences)
	}
	src, err := os.ReadFile("run_watcher_tick.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "s.releaseStrandedRunClaims(ctx)") {
		t.Error("runWatcherTick no longer runs releaseStrandedRunClaims")
	}
}
