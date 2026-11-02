// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The notices this server sends and takes over livebus (ha-l2.4): a run's lifecycle events to
// the other replicas' event rings, a kill to the replica creating the run's sandbox, and a runner's
// queued action to the replica holding its session. All are hints with a path that needs no notice
// (livebus's package comment): the events stream re-reads the store every beat, the dispatch's
// STARTING to RUNNING compare tears down a sandbox whose run was killed meanwhile, which is all a
// lost kill notice costs, and a runner reads a queued action at its next reconnect.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// liveBusPublishTimeout bounds one notice: a Postgres that does not answer costs the hint.
const liveBusPublishTimeout = 3 * time.Second

// replicaName is this server's identity in notices and rows other replicas read.
func (s *Server) replicaName() string {
	if s.cfg.LiveBus != nil {
		return s.cfg.LiveBus.Origin()
	}
	s.replicaOnce.Do(func() { s.replica = uuid.NewString() })
	return s.replica
}

// registerLiveBus wires this server's handlers and publishers onto its bus, when it has one.
func (s *Server) registerLiveBus() {
	b := s.cfg.LiveBus
	if b == nil {
		return
	}
	s.runEvents.publish = s.publishRunEvents
	b.Handle(livebus.KindRunEvent, s.onRunEventNotice)
	b.Handle(livebus.KindRunKill, func(m livebus.Message) {
		if m.Origin != b.Origin() {
			s.cancelLocalStart(m.Run)
		}
	})
	s.registerAttachLeaseNotices(b)
	b.Handle(livebus.KindRecordingGap, s.onRecordingGapNotice)
	b.Handle(livebus.KindRunnerGoaway, s.onRunnerGoawayNotice)
}

// publishNotice sends one notice on its own goroutine, so the caller (a state CAS, a kill) never
// waits for Postgres.
func (s *Server) publishNotice(kind string, run uuid.UUID, data any) {
	b := s.cfg.LiveBus
	if b == nil {
		return
	}
	s.goBackground(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), liveBusPublishTimeout)
		defer cancel()
		if err := b.Publish(ctx, kind, run, data); err != nil {
			slog.Warn("wardynd: a notice to the other replicas was not sent", slog.String("kind", kind),
				slog.String("run_id", run.String()), slog.Any("err", err))
		}
	})
}

// runnerGoawayNotice is a KindRunnerGoaway's data: when the sender queued what the runner must read.
type runnerGoawayNotice struct {
	At time.Time `json:"at"`
}

// announceRunnerGoaway tells every replica that runnerID's session should end, so the fresh one reads
// what was just queued for the runner. The replica holding the session may not be this one. A lost
// notice costs the old behaviour: the runner reads the action at its next reconnect.
func (s *Server) announceRunnerGoaway(runnerID uuid.UUID) {
	s.publishNotice(livebus.KindRunnerGoaway, runnerID, runnerGoawayNotice{At: s.cfg.Now()})
}

// onRunnerGoawayNotice ends the runner's session if this replica holds one. A session that began after
// the notice was sent read the queue after the action was in it, so a burst of queued actions makes the
// runner reconnect once, not once per action. The times are two replicas' clocks, so skew can cost one
// extra or one skipped GOAWAY, never a wrong delivery. The session checked here is the one ended: the
// end runs later, and by then a newer session, which read the queue after the check, may hold the slot.
func (s *Server) onRunnerGoawayNotice(m livebus.Message) {
	var n runnerGoawayNotice
	if unmarshalNotice(m, &n) != nil || n.At.IsZero() {
		return
	}
	session, began, ok := s.runnerSessionBegan(m.Run)
	if !ok || began.After(n.At) {
		return
	}
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return
	}
	// Off the listener's goroutine: ending a session may wait out a stalled link for a second.
	s.goBackground(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), liveBusPublishTimeout)
		defer cancel()
		if err := s.runnerHub(st).GoawaySession(ctx, m.Run, session); err != nil {
			slog.Warn("wardynd: a queued runner action waits for the runner's next session", slog.String("runner_id", m.Run.String()), slog.Any("err", err))
		}
	})
}

// cancelCreate stops runID's in-flight image build and CreateSandbox on this replica and, over the
// bus, on whichever replica holds them.
func (s *Server) cancelCreate(runID uuid.UUID) {
	s.cancelLocalStart(runID)
	s.publishNotice(livebus.KindRunKill, runID, nil)
}

// cancelLocalStart stops what this replica has in flight to start runID.
func (s *Server) cancelLocalStart(runID uuid.UUID) {
	s.runBuilds.cancel(runID)
	s.creates.cancel(runID)
}

func (s *Server) publishRunEvents(runID uuid.UUID, evs []client.RunEvent) {
	s.publishNotice(livebus.KindRunEvent, runID, evs)
}

// onRunEventNotice appends another replica's events to this one's ring, so a reader here
// follows a run another replica is dispatching.
func (s *Server) onRunEventNotice(m livebus.Message) {
	if m.Origin == s.replicaName() {
		return
	}
	var evs []client.RunEvent
	if err := json.Unmarshal(m.Data, &evs); err != nil || len(evs) == 0 {
		return
	}
	s.runEvents.append(m.Run, evs...)
}

// unmarshalNotice decodes a notice's data into v.
func unmarshalNotice(m livebus.Message, v any) error { return json.Unmarshal(m.Data, v) }
