// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// caveatNaming returns the first caveat containing every fragment, or "".
func caveatNaming(caveats []string, fragments ...string) string {
	for _, c := range caveats {
		all := true
		for _, f := range fragments {
			if !strings.Contains(c, f) {
				all = false
			}
		}
		if all {
			return c
		}
	}
	return ""
}

// kernelEventsRun builds a record run whose capture carries real kernel
// evidence: one exec and one connect the sensor observed and correlated to
// THIS run.
func kernelEventsRun(t *testing.T, hb *types.AuditEvent, created, updated time.Time) (*recordStore, *Server, uuid.UUID) {
	t.Helper()
	runID, wsID := uuid.New(), uuid.New()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record",
			State: types.RunCompleted, ConfinementClass: types.CC1,
			CreatedAt: created, UpdatedAt: updated},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
		events: []types.AuditEvent{
			egressAllowEvent(runID, "registry.npmjs.org"),
			kernelExecEvent(runID, "/usr/bin/node"),
		},
		heartbeat: hb,
	}
	return fake, newTestSrv(t, fake), runID
}

func kernelExecEvent(runID uuid.UUID, argv0 string) types.AuditEvent {
	return types.AuditEvent{RunID: &runID, Action: groundtruth.ActionProcessExec, Outcome: "success",
		Target: argv0, Data: mustJSON(groundtruth.EventData{
			Stream: groundtruth.Stream, Subtype: "exec",
			Correlation: groundtruth.CorrelationMapped, Argv: []string{argv0},
		})}
}

// TestReconcileRecordRun_CorroborationIsPerCapture pins the first half of
// B11b-F7. The kernel-ground-truth caveat read the newest GLOBAL heartbeat as
// of NOW and then said "this capture" about it. So a capture with real kernel
// evidence of its own, reviewed while the host sensor happens to be idle,
// claimed to have "no kernel-level corroboration" — about a capture that is
// visibly corroborated. The claim about the capture must come from the
// capture's own evidence.
func TestReconcileRecordRun_CorroborationIsPerCapture(t *testing.T) {
	idle := groundtruth.HeartbeatEventWithDropped(0, 0, 0, nil) // sensor alive, mapped nothing
	idle.Time = time.Now()
	now := time.Now()
	fake, srv, runID := kernelEventsRun(t, &idle, now.Add(-time.Hour), now)

	srv.reconcileRecordRun(context.Background(), runID)
	res := fake.savedResult(t, "build")

	if c := caveatNaming(res.Caveats, captureNoCorroborationFragment); c != "" {
		t.Errorf("caveats = %v\nmust not claim %q: this capture carries kernel events of its own",
			res.Caveats, c)
	}
	// The host's own state is still reported — as review-time state, about the
	// host, not as a claim about this capture.
	if c := caveatNaming(res.Caveats, "kernel ground truth", "idle"); c == "" {
		t.Errorf("caveats = %v, want the host sensor's own state still reported", res.Caveats)
	}
}

// TestReconcileRecordRun_NoKernelEvidenceSaysSo is the other direction: a
// capture with NO exec/connect/file-write of its own says so from its own
// evidence, whatever the host sensor reports now.
func TestReconcileRecordRun_NoKernelEvidenceSaysSo(t *testing.T) {
	healthy := groundtruth.HeartbeatEventWithDropped(0, 9, 0, map[string]uint64{
		groundtruth.ActionProcessExec:    3,
		groundtruth.ActionNetworkConnect: 3,
		groundtruth.ActionFileWrite:      3,
	})
	healthy.Time = time.Now()
	runID, wsID := uuid.New(), uuid.New()
	now := time.Now()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record",
			State: types.RunCompleted, ConfinementClass: types.CC1,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
		importStateFake: importStateFake{ws: recordingWorkspace(wsID, runID, "build")},
		events:          []types.AuditEvent{egressAllowEvent(runID, "registry.npmjs.org")},
		heartbeat:       &healthy,
	}
	srv := newTestSrv(t, fake)

	srv.reconcileRecordRun(context.Background(), runID)
	res := fake.savedResult(t, "build")

	if c := caveatNaming(res.Caveats, captureNoCorroborationFragment); c == "" {
		t.Errorf("caveats = %v, want one saying this capture has no kernel corroboration of its own "+
			"(a healthy host sensor is not evidence about THIS capture)", res.Caveats)
	}
}

// TestReconcileRecordRun_OrdinaryHostDropsAreNotAnAnomaly is the SR-1 pin, and
// it is the one that matters most: dropped_unmapped is the sensor's CUMULATIVE,
// process-lifetime count of every kernel event on the host that bound to no
// Wardyn run — the daemon itself, sshd, cron. It is non-zero within seconds of
// the sensor starting and never resets. Firing on the count alone therefore
// stamped "a possible proxy bypass" on EVERY clean capture on a host that does
// anything, and the window check cannot save it: at the terminal transition the
// newest heartbeat is at most one interval old, so it is always inside.
//
// The only thing the counter actually proves is the case /healthz already names:
// drops with observed_total == 0 means the sensor saw kernel events and bound
// NONE of them to any run — correlation broken, not a busy host.
func TestReconcileRecordRun_OrdinaryHostDropsAreNotAnAnomaly(t *testing.T) {
	now := time.Now()
	// A healthy sensor on a busy host: 12 events correlated, 7 host events
	// dropped as unmapped. This is what every ordinary deployment looks like.
	hb := groundtruth.HeartbeatEventWithDropped(0, 12, 7, map[string]uint64{
		groundtruth.ActionProcessExec:    4,
		groundtruth.ActionNetworkConnect: 4,
		groundtruth.ActionFileWrite:      4,
	})
	hb.Time = now.Add(-30 * time.Second) // as fresh as the production path ever sees
	fake, srv, runID := kernelEventsRun(t, &hb, now.Add(-time.Hour), now)

	srv.reconcileRecordRun(context.Background(), runID)
	res := fake.savedResult(t, "build")
	if res.Observations == nil {
		t.Fatal("no observations captured")
	}
	if a := caveatNaming(res.Observations.Anomalies, "unmapped"); a != "" {
		t.Errorf("anomaly %q on a clean capture: the sensor is correlating fine (observed_total=12) "+
			"and the drops are ordinary host activity outside any sandbox", a)
	}
}

// TestReconcileRecordRun_DroppedUnmappedInWindowIsAnAnomaly pins B11b-F4. The
// "possible proxy bypass" anomaly could never fire: an unmapped kernel event
// carries a nil run_id, the gated mapper drops it, and Capture reads only
// events already scoped to this run. The signal comes from the sensor's own
// counters instead, on the broken-correlation shape (drops with NOTHING
// correlated), and only from a heartbeat that beat DURING the capture — a count
// reported days later says nothing about this run.
func TestReconcileRecordRun_DroppedUnmappedInWindowIsAnAnomaly(t *testing.T) {
	now := time.Now()
	// SR-1 fixture: observed_total 0 with drops — the sensor saw kernel events
	// and bound NONE of them. observed_by_kind is empty for the same reason.
	hb := groundtruth.HeartbeatEventWithDropped(0, 0, 7, nil)
	hb.Time = now.Add(-30 * time.Minute) // inside the capture window
	fake, srv, runID := kernelEventsRun(t, &hb, now.Add(-time.Hour), now)

	srv.reconcileRecordRun(context.Background(), runID)
	res := fake.savedResult(t, "build")
	if res.Observations == nil {
		t.Fatal("no observations captured")
	}
	if a := caveatNaming(res.Observations.Anomalies, "unmapped"); a == "" {
		t.Errorf("anomalies = %v, want one naming the sensor's unmapped drops during this capture",
			res.Observations.Anomalies)
	}

	// Same counter, reported by a heartbeat OUTSIDE the window: says nothing
	// about this capture and must not become its anomaly.
	late := hb
	late.Time = now.Add(48 * time.Hour)
	fake2, srv2, runID2 := kernelEventsRun(t, &late, now.Add(-time.Hour), now)
	srv2.reconcileRecordRun(context.Background(), runID2)
	res2 := fake2.savedResult(t, "build")
	if res2.Observations == nil {
		t.Fatal("no observations captured")
	}
	if a := caveatNaming(res2.Observations.Anomalies, "unmapped"); a != "" {
		t.Errorf("anomaly %q was sourced from a heartbeat outside the capture window", a)
	}
}
