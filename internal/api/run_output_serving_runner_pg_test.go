// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run's output source is decided by the runner that served it, never by this deployment's own runner.
// One deployment whose runner is Kubernetes under the recorder, with recording on: a laptop run on
// Docker keeps and serves its tail (source stdout); a cluster run, on this deployment's runner or on an
// organisation's Kubernetes runner under the recorder, keeps no tail and serves its recording; a run
// that is not recorded keeps its tail whatever its runner; and a laptop run whose runner cannot be
// asked keeps its tail.
func TestRunOutputPG_SourceFollowsTheServingRunner(t *testing.T) {
	l := newMaskLab(t)
	rp := l.recordingReplica()
	st := store.NewPG(l.pool)
	ctx := context.Background()

	// tailed runs a run that must keep a tail: it prints, ends, and reads back from source stdout.
	tailed := func(t *testing.T, run types.AgentRun) {
		t.Helper()
		rp.dispatch(t, run)
		w := rp.srv.openExecOutput(run, false)
		if w == nil {
			t.Fatal("dispatch kept no tail: the run's output would be read from a recording it never made")
		}
		writeExecOutput(t, w, "printed on its own runner\n")
		run = l.recordingRunEnded(run)
		rp.srv.FinishRunOutput(ctx, run.ID)
		code, got, body := l.readOutput(rp, run.ID)
		if code != http.StatusOK || got.Source != "stdout" || got.Output != "printed on its own runner\n" || got.CaptureGap || got.Incomplete {
			t.Fatalf("read = %d %s, want 200 with the tail from source stdout", code, body)
		}
	}
	// recorded runs a run whose runner keeps no tail: its recording is its output.
	recorded := func(t *testing.T, run types.AgentRun) {
		t.Helper()
		rp.dispatch(t, run)
		if rp.srv.openExecOutput(run, false) != nil {
			t.Fatal("dispatch kept a tail for a run whose runner keeps none")
		}
		cast := partsHeader + `[0,"o","printed under the recorder\n"]` + "\n"
		var w *httptest.ResponseRecorder
		if run.RunnerID != nil { // a runner's upload comes through its own relay
			w = relayedPut(t, rp.srv, *run.RunnerID, "/api/v1/internal/recordings/"+run.ID.String(), mintRunTokenAs(t, l.h, run.ID, maskOwner), cast)
		} else {
			w = l.upload(rp, run, cast)
		}
		if w.Code != http.StatusNoContent {
			t.Fatalf("upload = %d %s", w.Code, w.Body)
		}
		run = l.recordingRunEnded(run)
		rp.srv.FinishRunOutput(ctx, run.ID)
		code, got, body := l.readOutput(rp, run.ID)
		if code != http.StatusOK || got.Source != recordingOutputSource || got.Output != "printed under the recorder\n" || got.CaptureGap {
			t.Fatalf("read = %d %s, want 200 with the recording's bytes from source recording", code, body)
		}
	}

	t.Run("laptop run on Docker reads its tail", func(t *testing.T) {
		rn := claimedLocalRunner(t, st)
		connectRunner(t, rp.srv, rn.ID, dockerRunnerSupport)
		tailed(t, localRunOn(t, st, rn.ID))
	})
	t.Run("cluster run on this deployment's runner reads its recording", func(t *testing.T) {
		recorded(t, l.run())
	})
	t.Run("run on an organisation's Kubernetes runner under the recorder reads its recording", func(t *testing.T) {
		rn := claimedLocalRunner(t, st)
		connectRunner(t, rp.srv, rn.ID, recordedK8sRunnerSupport)
		recorded(t, localRunOn(t, st, rn.ID))
	})
	t.Run("run that is not recorded reads its tail", func(t *testing.T) {
		// Dispatched while the organisation kept no recordings: the recorder wrapped nothing.
		run := l.run()
		if err := st.SetRunNotRecorded(ctx, run.ID, types.NotRecordedOrgOff); err != nil {
			t.Fatal(err)
		}
		run.NotRecorded = types.NotRecordedOrgOff
		tailed(t, run)
	})
	t.Run("laptop run whose runner cannot be asked reads its tail", func(t *testing.T) {
		rn := claimedLocalRunner(t, st) // registered, never connected to this replica
		tailed(t, localRunOn(t, st, rn.ID))
	})
}
