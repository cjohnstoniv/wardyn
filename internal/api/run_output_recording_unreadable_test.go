// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// endedRunOutputs is the test's run output store with the database's answer to the give-up set by ended
// (runEndedBeforeReader).
type endedRunOutputs struct {
	*memRunOutputs
	ended bool
}

func (s endedRunOutputs) RunEndedBefore(context.Context, uuid.UUID, time.Duration) (bool, error) {
	return s.ended, nil
}

// A stale pending row whose recording has a part that cannot be read stays pending while the run ended recently,
// and becomes a capture gap, reason recording_unavailable, on a try made once the run ended longer ago than the
// give-up, whether the unreadable part is the first or a later one. A try the pass's own end cut leaves it
// pending. Catches: a recording that stays unreadable for good holding the row pending forever.
func TestRecordingOutput_AStaleRowOnAnUnreadableRecordingSettlesToAGapAfterTheGiveUp(t *testing.T) {
	for _, c := range []struct {
		name      string
		part      string // the suffix of the part that cannot be read; "" is the first part
		ended     bool
		cancelled bool
		gap       bool
	}{
		{"a later part, the run ended recently", recording.PartSuffix(2), false, false, false},
		{"a later part, past the give-up", recording.PartSuffix(2), true, false, true},
		{"the first part, past the give-up", "", true, false, true},
		{"a later part, past the give-up, the pass cut", recording.PartSuffix(2), true, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, rs := newRecordingOutputFixture(t)
			saveOutputCast(t, rs, f.run.ID, partsHeader+`[0,"o","part one marker\n"]`+"\n")
			unreadable := recording.CastKey(f.run.ID.String(), c.part)
			rs.open = func(ctx context.Context, key string) (io.ReadCloser, error) {
				if key == unreadable {
					return io.NopCloser(iotest.ErrReader(errors.New("input/output error"))), nil
				}
				return rs.Store.OpenCast(ctx, key)
			}
			if err := f.mem.InsertPendingRunOutput(t.Context(), f.run.ID); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if c.cancelled {
				cut, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cut
			}
			f.srv.settleStaleRecordingOutput(ctx, endedRunOutputs{f.mem, c.ended}, f.run)
			r, found := f.mem.row(f.run.ID)
			fin := f.audit.eventsFor(f.run.ID, "run.output.finalize")
			if !c.gap {
				if !found || r.CapturedAt != nil || len(fin) != 0 {
					t.Fatalf("row %+v (found %v), %d finalize rows: want the row still pending and no finalize row", r, found, len(fin))
				}
				return
			}
			if !found || r.CapturedAt == nil || !r.CaptureGap || len(fin) != 1 {
				t.Fatalf("row %+v (found %v), %d finalize rows: want a capture gap and one finalize row", r, found, len(fin))
			}
			var d map[string]any
			if err := json.Unmarshal(fin[0].Data, &d); err != nil {
				t.Fatal(err)
			}
			if fin[0].Outcome != "failure" || d["reason"] != "recording_unavailable" || d["capture_gap"] != true {
				t.Errorf("finalize row %s %s: want a failure with reason recording_unavailable and capture_gap true", fin[0].Outcome, fin[0].Data)
			}
		})
	}
}
