// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApplyRunRecording_UnknownFactsAreNotADecline: a runner whose facts cannot be read now is asked to
// record and the run is not marked; only a runner known not to record marks it runner_declined.
func TestApplyRunRecording_UnknownFactsAreNotADecline(t *testing.T) {
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	declines, records, unknown := uuid.New(), uuid.New(), uuid.New()
	s := &Server{cfg: Config{RecordingStore: fs, Runner: &poolRunner{fakeRunner: &fakeRunner{}, runnerFacts: map[string]runner.TargetFacts{
		declines.String(): {Records: false}, records.String(): {Records: true},
	}}}}
	for _, tc := range []struct {
		name   string
		runner uuid.UUID
		want   types.RunNotRecorded
	}{{"known not to record", declines, types.NotRecordedRunnerDeclined}, {"records", records, ""}, {"facts unknown", unknown, ""}} {
		run := types.AgentRun{ID: uuid.New(), RunnerID: &tc.runner}
		var spec runner.SandboxSpec
		if !s.applyRunRecording(context.Background(), &run, &spec) {
			t.Fatalf("%s: dispatch refused with no required setting", tc.name)
		}
		if spec.Record == nil || !*spec.Record || run.NotRecorded != tc.want {
			t.Fatalf("%s: record=%v not_recorded=%q, want true %q", tc.name, spec.Record, run.NotRecorded, tc.want)
		}
	}
}
