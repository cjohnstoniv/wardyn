// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macOSDiskPendingFor is how long after a macOS VM run's end its page says the disk is being removed
// while no report has come. Past it the page says nothing of the disk: the runner may predate the report,
// and a report a later sweep brings still shows.
const macOSDiskPendingFor = 10 * time.Minute

// runMacOSDiskView is where a macOS VM run's disk stands on its Mac (mock M-D1 frames 9 to 11), for
// display only. State is "removing" (no report yet), "removed" (with At) or "kept" (with Reason, a
// runner.MacOSVMDisk reason code or "").
type runMacOSDiskView struct {
	State  string     `json:"state"`
	At     *time.Time `json:"at,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// runMacOSDiskView is the run's view. It is its runner's report if there is one; else "removing" for
// macOSDiskPendingFor after the end of a run whose VM was made; else nil. It is also nil for any other
// run, on a store that keeps no reports, and after a failed read.
func (s *Server) runMacOSDiskView(ctx context.Context, run types.AgentRun) *runMacOSDiskView {
	if !run.MachineKind.IsMacOSVM() {
		return nil
	}
	st, ok := s.cfg.Store.(store.MacOSVMDiskStore)
	if !ok {
		return nil
	}
	d, found, err := st.MacOSVMDisk(ctx, run.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: read a macOS VM disk report", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return nil
	}
	if found {
		if d.State == runner.MacOSVMDiskRemoved {
			return &runMacOSDiskView{State: runner.MacOSVMDiskRemoved, At: &d.At}
		}
		return &runMacOSDiskView{State: runner.MacOSVMDiskKept, Reason: d.Reason}
	}
	ended := run.EndedAt
	if ended == nil && run.LostReason == types.LostEnded {
		ended = run.LostAt // a run its lease ended stays RUNNING
	}
	if run.VMMachine == nil || ended == nil || s.cfg.Now().Sub(*ended) >= macOSDiskPendingFor {
		return nil
	}
	return &runMacOSDiskView{State: "removing"}
}

// runnerMacOSVMDisks records the macOS VM disk reports runnerID sent. They are the runner's word:
//   - a state other than the two is dropped;
//   - a reason outside the closed set becomes "";
//   - a time that is zero, or ahead of this server's clock, becomes now;
//   - the store keeps a report only for a macOS VM run of that runner.
//
// The writes outlive the call that carried them, on a bound of their own.
func (s *Server) runnerMacOSVMDisks(ctx context.Context, runnerID uuid.UUID, reports []runner.MacOSVMDisk) {
	st, ok := s.cfg.Store.(store.MacOSVMDiskStore)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	now := s.cfg.Now()
	for _, r := range reports {
		if r.State != runner.MacOSVMDiskRemoved && r.State != runner.MacOSVMDiskKept {
			continue
		}
		reason := ""
		if r.State == runner.MacOSVMDiskKept && runner.IsMacOSVMDiskReason(r.Reason) {
			reason = r.Reason
		}
		at := r.At
		if at.IsZero() || at.After(now) {
			at = now
		}
		if err := st.RecordMacOSVMDisk(ctx, runnerID, r.RunID, store.MacOSVMDisk{State: r.State, Reason: reason, At: at}); err != nil {
			slog.WarnContext(ctx, "wardynd: record a macOS VM disk report", slog.String("runner_id", runnerID.String()),
				slog.String("run_id", r.RunID.String()), slog.Any("err", err))
		}
	}
}
