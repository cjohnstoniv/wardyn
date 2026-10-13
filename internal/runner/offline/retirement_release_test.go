// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// retainedOutput gives run id four bytes of finished output bound to its
// fixture ref, acknowledged up to ack, and returns the run's output directory.
func retainedOutput(t *testing.T, id uuid.UUID, ack int64) (*runnerio.Server, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := runnerio.New(runnertest.NewFake(), uuid.NewString(), dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindOutput(id, id.String()); err != nil {
		t.Fatal(err)
	}
	end := runner.BeginOutputDrain(w)
	_, err = io.WriteString(w, "tail")
	end(err)
	raw, _ := json.Marshal(runnerwire.OutputAckArgs{RunID: id, Offset: ack})
	if _, err = s.Call(t.Context(), &runnerwire.Peer{}, 0, runnerwire.MethodOutputAck, raw); err != nil {
		t.Fatal(err)
	}
	return s, filepath.Join(dir, "output", id.String())
}

// retire applies a retirement for a run with four bytes of output. settled
// reports settlement to the manager; release, when set, is the wired hook.
func retire(t *testing.T, ack int64, settledAnyway, wire bool) (outputKept bool) {
	t.Helper()
	m, _, id, _, a := retirementFixture(t)
	s, dir := retainedOutput(t, id, ack)
	m.outputSettled = s.OutputSettled
	if settledAnyway {
		m.outputSettled = func(context.Context, uuid.UUID, string) (int64, bool, error) { return 4, true, nil }
	}
	if wire {
		m.ReleaseSettledOutput(s.ReleaseOutput)
	}
	a.Retirement.OutputEnd = 4
	got, err := m.apply(t.Context(), a)
	if err != nil || got.Outcome != "success" || !m.state.Runs[id].Retired {
		t.Fatalf("retire: %+v %v", got, err)
	}
	_, err = os.Stat(dir)
	return err == nil
}

// With no release wired, retirement keeps settled output as it always has.
func TestRetirementWithoutReleaseKeepsOutput(t *testing.T) {
	if !retire(t, 4, false, false) {
		t.Fatal("retirement with no release hook removed output")
	}
}

func TestRetirementReleasesSettledOutput(t *testing.T) {
	if retire(t, 4, false, true) {
		t.Fatal("retired run kept settled output")
	}
}

// Output the runner has not seen acknowledged to its end is kept even when
// the run retires, and the failed release does not fail the retirement.
func TestRetirementKeepsUnsettledOutput(t *testing.T) {
	if !retire(t, 2, true, true) {
		t.Fatal("retirement released unacknowledged output")
	}
}
