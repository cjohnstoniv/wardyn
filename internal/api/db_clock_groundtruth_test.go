// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// clockedSensorStore is nodeSensorStore on a fake database clock: DatabaseNow answers at, or fails with err.
type clockedSensorStore struct {
	*nodeSensorStore
	at  time.Time
	err error
}

func (s clockedSensorStore) DatabaseNow(context.Context) (time.Time, error) { return s.at, s.err }

// A Kubernetes kernel event for a run that ended loses its run once the run has been gone groundtruthLateEvents on
// the database's clock, which stamps ended_at, whatever the server's clock says. A clock that cannot be read refuses
// the batch with a server error, as a run that cannot be read does, and an event with no pod never reads it.
// Catches: the late-event cut judged on the server's clock, which a skewed replica moves by its skew.
func TestGroundtruthPodEventAfterRunEnded_IsJudgedOnTheDatabaseClock(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "wardyn-runs")
	now := time.Now().UTC()
	for _, tc := range []struct {
		name       string
		endedAgo   time.Duration // on the server's clock
		dbOff      time.Duration
		clockErr   error
		pod        bool
		code       int
		wantReason string
	}{
		{"gone past the cut on the database's clock", groundtruthLateEvents - 2*time.Minute, 4 * time.Minute, nil, true, http.StatusAccepted, "run_ended"},
		{"inside the cut on the database's clock", groundtruthLateEvents + 2*time.Minute, -4 * time.Minute, nil, true, http.StatusAccepted, ""},
		{"the database's clock cannot be read", groundtruthLateEvents + 2*time.Minute, 0, errClockRead, true, http.StatusInternalServerError, ""},
		{"an event with no pod never reads the clock", groundtruthLateEvents + 2*time.Minute, 0, errClockRead, false, http.StatusAccepted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			ended := now.Add(-tc.endedAgo)
			st := &nodeSensorStore{run: types.AgentRun{ID: runID, State: types.RunCompleted, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, EndedAt: &ended}}
			srv := nodeSensorServer(h, clockedSensorStore{nodeSensorStore: st, at: now.Add(tc.dbOff), err: tc.clockErr}, now)
			ev := podExec(runID, "wardyn-runs/wardyn-agent-"+runID.String(), "/bin/late")
			if !tc.pod {
				ev.Data, _ = json.Marshal(groundtruth.EventData{Stream: groundtruth.Stream, Subtype: "process_exec",
					Correlation: groundtruth.CorrelationMapped, Node: "node-a"})
			}
			body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{ev}})
			w := do(t, srv, http.MethodPost, "/api/v1/internal/groundtruth", h.mintGroundtruthToken(t), string(body))
			if w.Code != tc.code {
				t.Fatalf("batch code = %d, want %d; body=%s", w.Code, tc.code, w.Body.String())
			}
			if tc.code != http.StatusAccepted {
				return
			}
			got, ed := recordedByTarget(t, h, "/bin/late")
			if kept := got.RunID != nil && ed.Reason == ""; kept != (tc.wantReason == "") || ed.Reason != tc.wantReason {
				t.Fatalf("run=%v reason=%q; want reason %q, the run kept %v", got.RunID, ed.Reason, tc.wantReason, tc.wantReason == "")
			}
		})
	}
}
