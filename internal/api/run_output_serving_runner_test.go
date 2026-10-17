// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dockerRunnerSupport is a laptop runner on Docker: it records each run as asked and keeps a tail.
var dockerRunnerSupport = substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}, SessionRecording: true, RecordPerRun: true}

// recordedK8sRunnerSupport is a runner on Kubernetes under the session recorder: its runs keep no tail.
var recordedK8sRunnerSupport = substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC2}, SessionRecording: true, RecordPerRun: true, ExecOutputUncaptured: true}

// connectRunner puts runner id's stream on srv, online and advertising support, as the hub does once the
// runner has connected and sent its `caps` event.
func connectRunner(t *testing.T, srv *Server, id uuid.UUID, support substrate.ClassSupport) *remote.Substrate {
	t.Helper()
	sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
	advertiseRunner(t, sub, support)
	srv.runnerStreams.subsMu.Lock()
	defer srv.runnerStreams.subsMu.Unlock()
	if srv.runnerStreams.subs == nil {
		srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
	return sub
}

// advertiseRunner is the runner's `caps` event.
func advertiseRunner(t *testing.T, sub *remote.Substrate, support substrate.ClassSupport) {
	t.Helper()
	data, err := json.Marshal(runnerwire.Caps{Support: support})
	if err != nil {
		t.Fatal(err)
	}
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
}

// laptopRunFixture is the recording fixture (this deployment's runner is Kubernetes under the
// recorder, recording on) with its run bound to a connected Docker laptop runner, whose tail is open
// and holds what the run printed.
func laptopRunFixture(t *testing.T, printed string) (*outputFixture, *remote.Substrate) {
	t.Helper()
	f, _ := newRecordingOutputFixture(t)
	id := uuid.New()
	f.st.mu.Lock()
	f.st.run.RunnerID, f.st.run.Placement = &id, types.PlacementLocal
	f.st.mu.Unlock()
	f.run.RunnerID, f.run.Placement = &id, types.PlacementLocal
	sub := connectRunner(t, f.srv, id, dockerRunnerSupport)
	writeExecOutput(t, f.open(t), printed)
	return f, sub
}

// A recording that yielded nothing does not hide the tail this process holds: the read serves the
// tail, not an empty capture gap.
func TestRunOutput_RecordingGapDoesNotHideAHeldTail(t *testing.T) {
	f, _ := laptopRunFixture(t, "laptop printed this\n")
	now := time.Now()
	f.mem.mu.Lock()
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: recordingOutputSource, Incomplete: true, CaptureGap: true, CapturedAt: &now, ClaimedAt: now}
	f.mem.mu.Unlock()
	code, got := f.get(t)
	if code != http.StatusOK || got.Source != "stdout" || got.Output != "laptop printed this\n" || got.CaptureGap {
		t.Fatalf("read = %d %+v, want 200 with the tail's bytes from source stdout", code, got)
	}
}

// A runner that starts reporting the recorder after its run's tail was opened (it reconnected with
// recording on) does not move the run to the recording path: no recording gap row is written over
// the owed stdout row, so the run's end keeps the tail.
func TestRunOutput_HeldTailIsNotReplacedByAnEmptyRecording(t *testing.T) {
	f, sub := laptopRunFixture(t, "laptop printed this\n")
	advertiseRunner(t, sub, recordedK8sRunnerSupport)
	if code, got := f.get(t); code != http.StatusOK || got.Output != "laptop printed this\n" {
		t.Fatalf("read before the end = %d %+v, want the tail", code, got)
	}
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	row := f.finalRow(t)
	if row.Source != "stdout" || string(row.Output) != "laptop printed this\n" || row.CaptureGap {
		t.Fatalf("final row = %+v output %q, want the tail kept as source stdout", row, row.Output)
	}
}
