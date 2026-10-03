// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run busy inside its sandbox moves nothing the idle reaper reads: no egress,
// no attach. So the pause sweep also reads each auto-stop candidate's CPU from
// the substrate (runner.ActivitySampler: a PodMetrics list on Kubernetes, the
// daemon's stats on Docker; never an exec) and, above idleQuietCorePercent,
// bumps updated_at, the clock the reaper measures by, through Store.TouchRun.
// The same reading decides an idle pause, so the two cannot disagree.
//
// This is a workload-activity signal. It is never runner liveness, and a run
// with no reading is simply not touched: that is neither idle nor gone.
//
// ponytail: a sandbox can keep its own run alive by burning CPU; the run's
// maximum age is what bounds that, not this sweep.

// idleCPUSignalTTL is how long the last read of whether the CPU signal exists
// stands in for a new one on /setup/status.
const idleCPUSignalTTL = time.Minute

// activitySignal is the sweep's last word on whether the substrate can report
// CPU, and whether any run asked for auto-stop, for the /setup/status row.
type activitySignal struct {
	mu           sync.Mutex
	checked      time.Time
	off          bool
	autoStopSeen bool
}

func (a *activitySignal) record(off bool, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checked, a.off = now, off
}

func (a *activitySignal) noteAutoStop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.autoStopSeen = true
}

// autoStopSampleFraction is the share of a run's auto_stop_after_sec it must be
// quiet for before the sweep reads its CPU: late enough that a young run costs
// nothing, early enough that a read, or the few after it, lands before the reap.
const autoStopSampleFraction = 2

// autoStopCandidate reports whether run is close enough to its auto-stop to be
// worth a CPU reading. A paused run is frozen and cannot be busy.
func autoStopCandidate(run types.AgentRun, now time.Time) bool {
	return run.AutoStopAfterSec > 0 && run.PausedAt == nil &&
		now.Sub(run.UpdatedAt) >= time.Duration(run.AutoStopAfterSec)*time.Second/autoStopSampleFraction
}

// sampleRunCPU reads each of runs' CPU, as a percent of one core, in the fewest
// calls the runner's sampler allows: one for a batching substrate, one per run
// otherwise. The caller has already bounded a per-run substrate to
// idleSamplesPerTick runs. A run absent from the result has no reading.
func (s *Server) sampleRunCPU(ctx context.Context, runs []types.AgentRun) map[uuid.UUID]float64 {
	smp, ok := s.cfg.Runner.(runner.ActivitySampler)
	if !ok || len(runs) == 0 {
		return nil
	}
	groups := [][]types.AgentRun{runs}
	if !smp.BatchSample() {
		groups = groups[:0]
		for _, run := range runs {
			groups = append(groups, []types.AgentRun{run})
		}
	}
	out := make(map[uuid.UUID]float64, len(runs))
	for _, group := range groups {
		for id, pct := range s.sampleGroup(ctx, smp, group) {
			out[id] = pct
		}
	}
	return out
}

// sampleGroup is one sampler call over runs. A disk walk's CPU lands in the
// sandbox's stats too, so each run's window is claimed first, as the pause's
// own read always did: walks in flight finish before it opens and none starts
// inside it. A window not claimed in time is a run with no reading.
func (s *Server) sampleGroup(ctx context.Context, smp runner.ActivitySampler, runs []types.AgentRun) map[uuid.UUID]float64 {
	var (
		refs []string
		byID = make(map[string]uuid.UUID, len(runs))
	)
	for _, run := range runs {
		claim, stop := context.WithTimeout(ctx, runResourcesExecTimeout)
		ok := s.pause.beginSample(claim, run.ID)
		stop()
		if !ok {
			continue
		}
		defer s.pause.endSample(run.ID)
		refs = append(refs, run.SandboxRef)
		byID[run.SandboxRef] = run.ID
	}
	if len(refs) == 0 {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, runResourcesExecTimeout)
	defer cancel()
	got, err := smp.SampleCPU(callCtx, refs)
	if err == nil || errors.Is(err, runner.ErrActivityUnavailable) {
		s.activity.record(err != nil, s.cfg.Now())
	}
	out := make(map[uuid.UUID]float64, len(got))
	for ref, pct := range got {
		if id, ok := byID[ref]; ok {
			out[id] = pct
		}
	}
	return out
}

// idleCPUSignalOff reports whether the substrate has no CPU signal, probing at
// most once per idleCPUSignalTTL (the sweep refreshes it on its own reads).
func (s *Server) idleCPUSignalOff(ctx context.Context, smp runner.ActivitySampler) bool {
	s.activity.mu.Lock()
	if !s.activity.checked.IsZero() && s.cfg.Now().Sub(s.activity.checked) < idleCPUSignalTTL {
		off := s.activity.off
		s.activity.mu.Unlock()
		return off
	}
	s.activity.mu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, runResourcesExecTimeout)
	defer cancel()
	_, err := smp.SampleCPU(callCtx, nil)
	switch {
	case err == nil:
		s.activity.record(false, s.cfg.Now())
	case errors.Is(err, runner.ErrActivityUnavailable):
		s.activity.record(true, s.cfg.Now())
	}
	s.activity.mu.Lock()
	defer s.activity.mu.Unlock()
	return s.activity.off
}

// idleCPUSignalRow is the /setup/status "Idle detection" row. It shows only
// where idle auto-stop is in use and the runner can report CPU at all.
func (s *Server) idleCPUSignalRow(ctx context.Context) (SetupCheck, bool) {
	smp, ok := s.cfg.Runner.(runner.ActivitySampler)
	if !ok {
		return SetupCheck{}, false
	}
	s.activity.mu.Lock()
	inUse := s.cfg.DefaultPolicy.AutoStopAfterSec > 0 || s.activity.autoStopSeen
	s.activity.mu.Unlock()
	if !inUse {
		return SetupCheck{}, false
	}
	return idleCPUSignalCheck(s.idleCPUSignalOff(ctx, smp)), true
}
