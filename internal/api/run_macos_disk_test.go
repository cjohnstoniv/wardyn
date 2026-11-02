// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macOSDiskStore keeps reports in a map. A write whose context has ended fails, as a pool's would.
type macOSDiskStore struct {
	ownedRunners
	reports map[uuid.UUID]store.MacOSVMDisk
	runners map[uuid.UUID]uuid.UUID // run id -> the runner id its last write named
}

func (m macOSDiskStore) RecordMacOSVMDisk(ctx context.Context, runnerID, runID uuid.UUID, d store.MacOSVMDisk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.reports[runID], m.runners[runID] = d, runnerID
	return nil
}

func (m macOSDiskStore) MacOSVMDisk(_ context.Context, runID uuid.UUID) (store.MacOSVMDisk, bool, error) {
	d, ok := m.reports[runID]
	return d, ok, nil
}

func newMacOSDiskStore() macOSDiskStore {
	return macOSDiskStore{ownedRunners: ownedRunners{rows: map[uuid.UUID]types.Runner{}}, reports: map[uuid.UUID]store.MacOSVMDisk{}, runners: map[uuid.UUID]uuid.UUID{}}
}

// The view: the runner's report when there is one; else removing for ten minutes after the end of a run
// whose VM was made; else nothing. No case without a report is ever kept: an old runner never gets MV-D12.
func TestRunMacOSDiskView(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	at := now.Add(-2 * time.Minute)
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	machine := &types.VMMachine{Accelerator: types.MacVMAccelerator}
	mac := func(over func(*types.AgentRun)) types.AgentRun {
		r := types.AgentRun{ID: uuid.New(), State: types.RunCompleted, MachineKind: types.MachineMacOSVM, VMMachine: machine}
		over(&r)
		return r
	}
	type row struct {
		name   string
		run    types.AgentRun
		report *store.MacOSVMDisk
		plain  bool // a store without the interface
		want   *runMacOSDiskView
	}
	rows := []row{
		{name: "removed", run: mac(func(*types.AgentRun) {}), report: &store.MacOSVMDisk{State: "removed", At: at}, want: &runMacOSDiskView{State: "removed", At: &at}},
		{name: "kept", run: mac(func(*types.AgentRun) {}), report: &store.MacOSVMDisk{State: "kept", Reason: "helper_alive", At: at}, want: &runMacOSDiskView{State: "kept", Reason: "helper_alive"}},
		{name: "no report, ended a minute ago", run: mac(func(r *types.AgentRun) { r.EndedAt = ago(time.Minute) }), want: &runMacOSDiskView{State: "removing"}},
		{name: "no report, ended 11 minutes ago", run: mac(func(r *types.AgentRun) { r.EndedAt = ago(11 * time.Minute) })},
		{name: "no report, no VM made", run: mac(func(r *types.AgentRun) { r.EndedAt = ago(time.Minute); r.VMMachine = nil })},
		{name: "lease ended a minute ago", run: mac(func(r *types.AgentRun) {
			r.State, r.LostReason, r.LostAt = types.RunRunning, types.LostEnded, ago(time.Minute)
		}), want: &runMacOSDiskView{State: "removing"}},
		{name: "running, not ended", run: mac(func(r *types.AgentRun) { r.State = types.RunRunning })},
		{name: "linux_vm with a report", run: mac(func(r *types.AgentRun) { r.MachineKind = types.MachineLinuxVM; r.EndedAt = ago(time.Minute) }),
			report: &store.MacOSVMDisk{State: "removed", At: at}},
		{name: "container", run: mac(func(r *types.AgentRun) { r.MachineKind, r.VMMachine, r.EndedAt = "", nil, ago(time.Minute) })},
		{name: "a store without reports", run: mac(func(r *types.AgentRun) { r.EndedAt = ago(time.Minute) }), plain: true},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			st := newMacOSDiskStore()
			if tc.report != nil {
				st.reports[tc.run.ID] = *tc.report
			}
			var s *Server
			if tc.plain {
				s = &Server{cfg: Config{Store: st.ownedRunners, Now: func() time.Time { return now }}}
			} else {
				s = &Server{cfg: Config{Store: st, Now: func() time.Time { return now }}}
			}
			got := s.runMacOSDiskView(t.Context(), tc.run)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("view = %+v, want none", got)
			case tc.want == nil:
			case got == nil || got.State != tc.want.State || got.Reason != tc.want.Reason || (got.At == nil) != (tc.want.At == nil) || got.At != nil && !got.At.Equal(*tc.want.At):
				t.Fatalf("view = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The runner's reports are normalized to the closed sets before they are stored.
func TestRunnerMacOSVMDisksNormalizes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	st := newMacOSDiskStore()
	s := &Server{cfg: Config{Store: st, Now: func() time.Time { return now }}}
	runnerID := uuid.New()
	dropped, free, removed, zero, future := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	past := now.Add(-time.Minute)
	s.runnerMacOSVMDisks(t.Context(), runnerID, []runner.MacOSVMDisk{
		{RunID: dropped, State: "deleted", At: past},
		{RunID: free, State: runner.MacOSVMDiskKept, Reason: "rm -rf /Users/x/…: busy", At: past},
		{RunID: removed, State: runner.MacOSVMDiskRemoved, Reason: runner.MacOSVMDiskHelperAlive, At: past},
		{RunID: zero, State: runner.MacOSVMDiskRemoved},
		{RunID: future, State: runner.MacOSVMDiskRemoved, At: now.Add(time.Hour)},
	})
	if _, ok := st.reports[dropped]; ok {
		t.Fatal("a state outside the two was stored")
	}
	want := map[uuid.UUID]store.MacOSVMDisk{
		free:    {State: "kept", Reason: "", At: past},
		removed: {State: "removed", Reason: "", At: past},
		zero:    {State: "removed", At: now},
		future:  {State: "removed", At: now},
	}
	for id, w := range want {
		got, ok := st.reports[id]
		if !ok || got.State != w.State || got.Reason != w.Reason || !got.At.Equal(w.At) {
			t.Errorf("report %s = %+v (%v), want %+v", id, got, ok, w)
		}
		if st.runners[id] != runnerID {
			t.Errorf("report %s named runner %s, want %s", id, st.runners[id], runnerID)
		}
	}
}

// The writes outlive the call that carried the reports: a context already ended still stores them.
func TestRunnerMacOSVMDisksOutliveTheCall(t *testing.T) {
	st := newMacOSDiskStore()
	s := &Server{cfg: Config{Store: st, Now: time.Now}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	id := uuid.New()
	s.runnerMacOSVMDisks(ctx, uuid.New(), []runner.MacOSVMDisk{{RunID: id, State: runner.MacOSVMDiskRemoved, At: time.Now().Add(-time.Second)}})
	if _, ok := st.reports[id]; !ok {
		t.Fatal("a report carried by an ended call was not stored")
	}
}
