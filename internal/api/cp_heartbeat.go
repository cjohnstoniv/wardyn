// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	cpHeartbeatEvery       = 10 * time.Second
	outageWindowKeep       = 30 * 24 * time.Hour
	outageWindowPruneEach  = time.Hour
	runnerLinkWriteTimeout = 5 * time.Second
	// outageWindowUpkeepTimeout bounds one pass's heal and prune.
	outageWindowUpkeepTimeout = 30 * time.Second
	// serverOutageGroupMin is how many of the server's own runs a no-renewals or mass-lapse census must count before
	// it opens a server_outage window: one run's dead proxy is not an outage of the server, and must not cover its own
	// expiry.
	serverOutageGroupMin = 2
)

// runCPHeartbeat beats for this replica until ctx ends, so that a gap no replica filled is recorded as a cp_outage
// window (#4082). It beats once at start and then on every tick. It also closes runner-link windows that will never
// close by themselves, and prunes old windows once an hour.
func (s *Server) runCPHeartbeat(ctx context.Context) {
	ow, ok := s.cfg.Store.(store.OutageWindows)
	if !ok {
		return
	}
	failing := false
	var lastPrune time.Time
	pass := func() {
		err := func() error {
			beatCtx, cancel := context.WithTimeout(ctx, cpHeartbeatEvery)
			w, err := ow.BeatControlPlane(beatCtx, store.ControlPlaneGapOver)
			cancel()
			if err != nil {
				return err
			}
			if w != nil && w.EndedAt != nil {
				slog.InfoContext(ctx, "wardynd: recorded a control-plane outage window", slog.Time("started_at", w.StartedAt),
					slog.Time("ended_at", *w.EndedAt), slog.Duration("gap", w.EndedAt.Sub(w.StartedAt)))
			}
			// A heal or prune that waits must not hold up the next beat for long: past 30 s that is a gap of its own.
			upkeepCtx, cancel := context.WithTimeout(ctx, outageWindowUpkeepTimeout)
			defer cancel()
			if _, err := ow.HealRunnerLinkWindows(upkeepCtx); err != nil {
				return err
			}
			if time.Since(lastPrune) >= outageWindowPruneEach {
				if _, err := ow.PruneOutageWindows(upkeepCtx, outageWindowKeep); err != nil {
					return err
				}
				lastPrune = time.Now()
			}
			return nil
		}()
		switch {
		case err != nil && ctx.Err() != nil:
		case err != nil && !failing:
			failing = true
			slog.WarnContext(ctx, "wardynd: the control-plane heartbeat failed; it is retried every tick and not logged again until it succeeds", slog.Any("err", err))
		case err == nil:
			failing = false
		}
	}
	pass()
	t := time.NewTicker(cpHeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}

// runnerAdmissibleNow says whether this server would admit runner id now: claimed (the test the hub's
// admission makes in Hub.start, which a revoked row fails), its owner active, and runners switched on. A read error answers false:
// a runner the server would not take back is not away on a lost link, and its runs are not held for it (#4082).
func (s *Server) runnerAdmissibleNow(ctx context.Context, st store.RunnerStore, id uuid.UUID) (bool, error) {
	rn, err := st.GetRunner(ctx, id)
	if err != nil {
		return false, err
	}
	if rn.State != types.RunnerClaimed {
		return false, nil
	}
	inactive, err := s.runnerOwnerInactive(ctx, rn)
	if err != nil {
		return false, err
	}
	if inactive {
		return false, nil
	}
	if err := s.runnersEnabled(ctx); err != nil {
		if errors.Is(err, errRunnersDisabled) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// openRunnerLinkWindow records that runner id's link has been down since its session ended, droppedFor ago, unless a
// later session has begun since, or the runner is one this server would not admit now (#4082). The window starts at
// the database's now minus droppedFor because the hub saw the drop then, on its own monotonic clock: neither the
// session's start (last_seen_at) nor a replica's wall clock enters.
func (s *Server) openRunnerLinkWindow(ctx context.Context, st store.RunnerStore, id uuid.UUID, session string, droppedFor time.Duration) {
	ow, ok := st.(store.OutageWindows)
	if !ok {
		return
	}
	// The hub's resume timer holds the session's lock while this runs.
	ctx, cancel := context.WithTimeout(ctx, runnerLinkWriteTimeout)
	defer cancel()
	admissible, err := s.runnerAdmissibleNow(ctx, st, id)
	if !admissible {
		if err != nil {
			slog.WarnContext(ctx, "wardynd: reading a runner whose link was lost failed; no link outage is recorded for it",
				slog.String("runner_id", id.String()), slog.Any("err", err))
		}
		return
	}
	opened, err := ow.OpenRunnerLinkWindow(ctx, id, session, droppedFor)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: recording a runner's link outage failed", slog.String("runner_id", id.String()), slog.Any("err", err))
		return
	}
	if opened {
		slog.InfoContext(ctx, "wardynd: recorded a runner link outage", slog.String("runner_id", id.String()), slog.Duration("dropped_for", droppedFor))
	}
}

// closeRunnerLinkWindow ends runner id's open link window when a new session for it begins.
func (s *Server) closeRunnerLinkWindow(ctx context.Context, st store.RunnerStore, id uuid.UUID) error {
	ow, ok := st.(store.OutageWindows)
	if !ok {
		return nil
	}
	if err := ow.CloseRunnerLinkWindow(ctx, id); err != nil {
		return fmt.Errorf("close the runner's link outage window: %w", err)
	}
	return nil
}

// recordServerOutageWindow opens or closes the server_outage window from this pass's verdict. A recovering store
// or an unread census changes nothing: the heartbeat's window covers the first, and a failed read neither opens nor
// closes a window. A no-renewals or mass-lapse condition opens one only for a group of the server's runs. With rule M
// off, an open window is closed.
func (s *Server) recordServerOutageWindow(ctx context.Context, v lapseVerdict) {
	ow, ok := s.cfg.Store.(store.OutageWindows)
	if !ok {
		return
	}
	var since time.Duration
	group := false
	if s.cfg.StoreHealth != nil && s.cfg.StoreHealth.Grace() != 0 {
		if slices.Contains(v.conditions, "recovering") || slices.Contains(v.conditions, "census_unread") {
			return
		}
		if slices.Contains(v.conditions, "no_renewals") && v.census.Running >= serverOutageGroupMin {
			since, group = v.census.NewestRenewAge, true
		}
		if slices.Contains(v.conditions, "mass_lapse") && v.census.Lapsed >= serverOutageGroupMin {
			since, group = max(since, lapseCensusWindow), true
		}
	}
	var err error
	if group {
		err = ow.OpenServerOutageWindow(ctx, since, map[string]any{"conditions": v.conditions, "server_runs": v.census.Running,
			"lapsed_recent": v.census.Lapsed})
	} else {
		err = ow.CloseServerOutageWindow(ctx)
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: recording the server outage window failed", slog.Any("err", err))
	}
}
