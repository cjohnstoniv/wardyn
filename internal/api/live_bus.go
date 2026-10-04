// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The notices this server sends and takes over livebus (ha-l2.4): a run's lifecycle events to
// the other replicas' event rings, and a kill to the replica creating the run's sandbox. Both
// are hints with a path that needs no notice (livebus's package comment): the events stream
// re-reads the store every beat, and the dispatch's STARTING to RUNNING compare tears down a
// sandbox whose run was killed meanwhile, which is all a lost kill notice costs.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
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
			s.creates.cancel(m.Run)
		}
	})
	s.registerAttachLeaseNotices(b)
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

// cancelCreate stops runID's in-flight CreateSandbox on this replica and, over the bus, on
// whichever replica holds it.
func (s *Server) cancelCreate(runID uuid.UUID) {
	s.creates.cancel(runID)
	s.publishNotice(livebus.KindRunKill, runID, nil)
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
