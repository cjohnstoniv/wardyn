// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
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

// recordingReadFake answers the two recording-policy reads from fields, so a read can fail.
type recordingReadFake struct {
	store.Store
	store.RunnerPoolStore
	org     types.RunnerPoolDefaults
	orgErr  error
	pool    types.RunnerPool
	poolErr error
}

func (f *recordingReadFake) GetRunnerPoolOrgDefaults(context.Context) (types.RunnerPoolDefaults, error) {
	return f.org, f.orgErr
}

func (f *recordingReadFake) GetRunnerPool(context.Context, uuid.UUID) (types.RunnerPool, error) {
	return f.pool, f.poolErr
}

// TestRecordingRequired_AnUnreadableSettingIsRequired: a run known not to record is held when the
// organisation's or its pool's recording requirement cannot be read, never started unrecorded.
func TestRecordingRequired_AnUnreadableSettingIsRequired(t *testing.T) {
	boom := errors.New("connection reset")
	poolID := uuid.New()
	for _, tc := range []struct {
		name     string
		fake     recordingReadFake
		pooled   bool
		wantPool string
		want     bool
	}{
		{"org read fails", recordingReadFake{orgErr: boom}, false, "", true},
		{"org read fails, pooled run", recordingReadFake{orgErr: boom}, true, "", true},
		{"pool read fails", recordingReadFake{poolErr: boom}, true, "", true},
		{"pool is gone", recordingReadFake{poolErr: store.ErrNotFound}, true, "", false},
		{"nothing required", recordingReadFake{pool: types.RunnerPool{Name: "ci"}}, true, "ci", false},
		{"pool requires", recordingReadFake{pool: types.RunnerPool{Name: "ci", Recording: types.RecordingRequired}}, true, "ci", true},
	} {
		s := &Server{cfg: Config{Store: &tc.fake}}
		run := types.AgentRun{ID: uuid.New()}
		if tc.pooled {
			run.RunnerPoolID = &poolID
		}
		pool, required := s.recordingRequired(context.Background(), run)
		if required != tc.want || pool != tc.wantPool {
			t.Errorf("%s: recordingRequired = (%q, %v), want (%q, %v)", tc.name, pool, required, tc.wantPool, tc.want)
		}
	}
}
