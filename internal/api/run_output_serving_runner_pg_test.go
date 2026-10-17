// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run's output source is decided by the runner that served it, never by this deployment's own runner.
// One deployment whose runner is Kubernetes under the recorder, with recording on: a laptop run on
// Docker keeps and serves its tail (source stdout); a cluster run, on this deployment's runner or on an
// organisation's Kubernetes runner under the recorder, keeps no tail and serves its recording, also
// when that runner drops before the run ends; a run that is not recorded keeps its tail, unless its
// runner predates per-run recording and records it anyway; and when the run's runner cannot be asked,
// this deployment's runner answers.
func TestRunOutputPG_SourceFollowsTheServingRunner(t *testing.T) {
	l := newMaskLab(t)
	rp := l.recordingReplica()
	st := store.NewPG(l.pool)
	ctx := context.Background()

	// tailed runs a run that must keep a tail: it prints, ends, and reads back from source stdout.
	// drop, when set, runs once the run has printed or uploaded and before it ends.
	tailed := func(t *testing.T, run types.AgentRun, drop func()) {
		t.Helper()
		rp.dispatch(t, run)
		w := rp.srv.openExecOutput(run, false)
		if w == nil {
			t.Fatal("dispatch kept no tail: the run's output would be read from a recording it never made")
		}
		writeExecOutput(t, w, "printed on its own runner\n")
		if drop != nil {
			drop()
		}
		run = l.recordingRunEnded(run)
		rp.srv.FinishRunOutput(ctx, run.ID)
		code, got, body := l.readOutput(rp, run.ID)
		if code != http.StatusOK || got.Source != "stdout" || got.Output != "printed on its own runner\n" || got.CaptureGap || got.Incomplete {
			t.Fatalf("read = %d %s, want 200 with the tail from source stdout", code, body)
		}
	}
	// recorded runs a run whose runner keeps no tail: its recording is its output.
	recorded := func(t *testing.T, run types.AgentRun, drop func()) {
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
		if drop != nil {
			drop()
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
		tailed(t, localRunOn(t, st, rn.ID), nil)
	})
	t.Run("laptop run whose runner drops before the end reads its tail", func(t *testing.T) {
		rn := claimedLocalRunner(t, st)
		_, link := connectRunner(t, rp.srv, rn.ID, dockerRunnerSupport)
		tailed(t, localRunOn(t, st, rn.ID), func() { link.online = false })
	})
	t.Run("cluster run on this deployment's runner reads its recording", func(t *testing.T) {
		recorded(t, l.run(), nil)
	})
	t.Run("run on an organisation's Kubernetes runner under the recorder reads its recording", func(t *testing.T) {
		rn := claimedLocalRunner(t, st)
		connectRunner(t, rp.srv, rn.ID, recordedK8sRunnerSupport)
		recorded(t, localRunOn(t, st, rn.ID), nil)
	})
	t.Run("org Kubernetes runner that drops before the end still reads its recording", func(t *testing.T) {
		rn := claimedLocalRunner(t, st)
		_, link := connectRunner(t, rp.srv, rn.ID, recordedK8sRunnerSupport)
		recorded(t, localRunOn(t, st, rn.ID), func() { link.online = false })
	})
	t.Run("run that is not recorded reads its tail", func(t *testing.T) {
		// Dispatched while the organisation kept no recordings: the recorder wrapped nothing.
		run := l.run()
		if err := st.SetRunNotRecorded(ctx, run.ID, types.NotRecordedOrgOff); err != nil {
			t.Fatal(err)
		}
		run.NotRecorded = types.NotRecordedOrgOff
		tailed(t, run, nil)
	})
	t.Run("run that is not recorded on a runner that cannot be asked reads its tail", func(t *testing.T) {
		rn := claimedLocalRunner(t, st) // registered, never connected to this replica
		run := localRunOn(t, st, rn.ID)
		if err := st.SetRunNotRecorded(ctx, run.ID, types.NotRecordedOrgOff); err != nil {
			t.Fatal(err)
		}
		run.NotRecorded = types.NotRecordedOrgOff
		tailed(t, run, nil)
	})
	t.Run("run marked not recorded on an older runner that records anyway reads its recording", func(t *testing.T) {
		// The organisation reads an older runner's per-run facts only through its inspector.
		prev := rp.srv.cfg.Runner
		rp.srv.cfg.Runner = streamFactsRunner{Runner: prev, srv: rp.srv}
		t.Cleanup(func() { rp.srv.cfg.Runner = prev })
		rn := claimedLocalRunner(t, st)
		connectRunner(t, rp.srv, rn.ID, olderK8sRunnerSupport)
		run := localRunOn(t, st, rn.ID)
		if err := st.SetRunNotRecorded(ctx, run.ID, types.NotRecordedRunnerDeclined); err != nil {
			t.Fatal(err)
		}
		run.NotRecorded = types.NotRecordedRunnerDeclined
		recorded(t, run, nil)
	})
}

// streamFactsRunner is this deployment's runner with the orchestrator's per-runner facts: a connected
// runner's own advertisement (remote.Substrate.Advertised), as runner.TargetInspector answers them.
type streamFactsRunner struct {
	runner.Runner
	srv *Server
}

func (r streamFactsRunner) RunnerFacts(id string) (runner.TargetFacts, bool) {
	r.srv.runnerStreams.subsMu.Lock()
	ep, ok := r.srv.runnerStreams.subs[uuid.MustParse(id)]
	r.srv.runnerStreams.subsMu.Unlock()
	if !ok {
		return runner.TargetFacts{}, false
	}
	return ep.sub.Advertised()
}

func (r streamFactsRunner) ExecutorFacts(context.Context, string) (runner.TargetFacts, bool) {
	return runner.TargetFacts{}, false
}
