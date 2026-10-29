// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// A retirement whose receipt was saved but whose release never ran (a crash between the two) releases the
// output when the org replays the action.
func TestRetirementReplayReleasesOutputTheCrashLeft(t *testing.T) {
	m, _, id, _, a := retirementFixture(t)
	s, dir := retainedOutput(t, id, 4)
	m.outputSettled = s.OutputSettled
	a.Retirement.OutputEnd = 4
	if got, err := m.apply(t.Context(), a); err != nil || got.Outcome != "success" {
		t.Fatalf("retire: %+v %v", got, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("output gone before the hook was wired: %v", err)
	}
	m.ReleaseSettledOutput(s.ReleaseOutput)
	if got, err := m.apply(t.Context(), a); err != nil || got.Outcome != "success" {
		t.Fatalf("replay: %+v %v", got, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the replayed retirement left the output on disk")
	}
}

// ackRetirement runs a through Pending against a loopback org that acknowledges it, which compacts the
// retired run to its tombstone.
func ackRetirement(t *testing.T, m *Manager, a runnerwire.PendingAction) *runnerwire.Peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	orgConn, runConn := runnerwire.Loopback()
	org := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnEventChecked: func(runnerwire.Event) error { return nil }})
	peer := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{})
	org.Start(ctx, 0)
	peer.Start(ctx, 0)
	t.Cleanup(func() { org.Close(); peer.Close() })
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{a}); err != nil {
		t.Fatal(err)
	}
	return peer
}

// A release that fails once the retirement is stored must still happen after the org acknowledges the
// receipt and the run compacts: on a reconnect, and again at the next runner start.
func TestRetiredOutputReleaseRetriesAfterReceiptAck(t *testing.T) {
	m, sub, id, _, a := retirementFixture(t)
	s, dir := retainedOutput(t, id, 4)
	m.outputSettled = s.OutputSettled
	m.ReleaseSettledOutput(func(uuid.UUID, string) error { return errors.New("disk busy") })
	a.Retirement.OutputEnd = 4
	peer := ackRetirement(t, m, a)
	if r := m.state.Runs[id]; !r.Tombstone || !r.OutputPending {
		t.Fatalf("receipt acknowledged but run %+v lost its release obligation", r)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("output gone though the release failed: %v", err)
	}
	// Reconnect with the release still failing: the obligation stays.
	if err := m.Pending(t.Context(), peer, nil); err != nil {
		t.Fatal(err)
	}
	if !m.state.Runs[id].OutputPending {
		t.Fatal("a failed retry dropped the release obligation")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// The next runner start wires a working release, as production does.
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.ReleaseSettledOutput(s.ReleaseOutput)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("restart left a retired run's output on disk")
	}
	if r := reopened.state.Runs[id]; !r.Retired || !r.Tombstone || r.OutputPending {
		t.Fatalf("after the release the run is %+v, want a settled tombstone", r)
	}
}

// A reconnect also pays the obligation, and it never releases output that is not settled.
func TestRetiredOutputReleaseOnReconnectKeepsUnsettledOutput(t *testing.T) {
	m, _, id, _, a := retirementFixture(t)
	s, dir := retainedOutput(t, id, 2)
	m.outputSettled = func(context.Context, uuid.UUID, string) (int64, bool, error) { return 4, true, nil }
	m.ReleaseSettledOutput(s.ReleaseOutput)
	a.Retirement.OutputEnd = 4
	peer := ackRetirement(t, m, a)
	if err := m.Pending(t.Context(), peer, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("retry released unacknowledged output: %v", err)
	}
	if !m.state.Runs[id].OutputPending {
		t.Fatal("unsettled output lost its release obligation")
	}
	raw, _ := json.Marshal(runnerwire.OutputAckArgs{RunID: id, Offset: 4})
	if _, err := s.Call(t.Context(), &runnerwire.Peer{}, 0, runnerwire.MethodOutputAck, raw); err != nil {
		t.Fatal(err)
	}
	if err := m.Pending(t.Context(), peer, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("reconnect left settled output on disk")
	}
}
