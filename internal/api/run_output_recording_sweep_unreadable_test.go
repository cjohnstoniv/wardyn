// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// twoRecordingRuns is the recording-output fixture's store holding two ended runs whose output waits on their
// recordings. It lists unreadable first, so a pass meets that run before the other, and answers the give-up with
// ended (runEndedBeforeReader).
type twoRecordingRuns struct {
	*memRunOutputs
	runs       map[uuid.UUID]types.AgentRun
	unreadable uuid.UUID
	ended      bool
}

func (s twoRecordingRuns) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if r, ok := s.runs[id]; ok {
		return r, nil
	}
	return types.AgentRun{}, store.ErrNotFound
}

func (s twoRecordingRuns) ListPendingRecordingRunOutputs(ctx context.Context, staleAfter time.Duration, limit int) ([]uuid.UUID, error) {
	ids, err := s.memRunOutputs.ListPendingRecordingRunOutputs(ctx, staleAfter, limit)
	slices.SortStableFunc(ids, func(a, b uuid.UUID) int {
		switch {
		case a == s.unreadable:
			return -1
		case b == s.unreadable:
			return 1
		}
		return 0
	})
	return ids, err
}

func (s twoRecordingRuns) RunEndedBefore(context.Context, uuid.UUID, time.Duration) (bool, error) {
	return s.ended, nil
}

// One recording that cannot be read holds back no other run in the recovery pass: the pass goes on and recovers
// the next run's output from its recording, and the unreadable run's work stays pending. While that run is inside
// its give-up the pass still answers the error, as a failing recording store is; past it the recording is that
// run's outcome alone and the pass succeeds. Catches: the pass ending at the first unreadable recording, which
// holds back every run after it and fails the sweep each time that run comes up again.
func TestRecordingOutput_OneUnreadableRecordingDoesNotStopTheSweep(t *testing.T) {
	for _, c := range []struct {
		name    string
		ended   bool
		passErr bool
	}{
		{"inside the give-up", false, true},
		{"past the give-up", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, rs := newRecordingOutputFixture(t)
			bad, good := f.run, f.run
			bad.ID, good.ID = uuid.New(), uuid.New()
			saveOutputCast(t, rs, good.ID, partsHeader+`[0,"o","readable marker\n"]`+"\n")
			rs.open = func(ctx context.Context, key string) (io.ReadCloser, error) {
				if key == bad.ID.String() {
					return io.NopCloser(iotest.ErrReader(errors.New("input/output error"))), nil
				}
				return rs.Store.OpenCast(ctx, key)
			}
			st := twoRecordingRuns{memRunOutputs: f.mem, runs: map[uuid.UUID]types.AgentRun{bad.ID: bad, good.ID: good}, unreadable: bad.ID, ended: c.ended}
			f.srv.cfg.Store = st
			for _, id := range []uuid.UUID{bad.ID, good.ID} {
				if err := f.mem.QueueRecordingRunOutput(t.Context(), id, 0, runOutputPendingStale, false); err != nil {
					t.Fatal(err)
				}
			}
			err := f.srv.sweepRecordingOutputs(t.Context(), st)
			if c.passErr != errors.Is(err, errRecordingOutputUnavailable) || !c.passErr && err != nil {
				t.Errorf("pass = %v; want the unreadable recording as its error: %v", err, c.passErr)
			}
			if r, found := f.mem.row(good.ID); !found || string(r.Output) != "readable marker\n" || r.CaptureGap || r.Source != recordingOutputSource {
				t.Fatalf("the next run's row = %+v (found %v), want its output recovered from its recording", r, found)
			}
			f.mem.mu.Lock()
			p := f.mem.recording[bad.ID]
			f.mem.mu.Unlock()
			if _, found := f.mem.row(bad.ID); found || p.requested <= p.completed {
				t.Fatalf("the unreadable run has a row (%v) or its work %+v is done; want no row and the work pending", found, p)
			}
		})
	}
}
