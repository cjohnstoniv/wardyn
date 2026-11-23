// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantSessionEraser erases a person's assistant sessions (store.PG).
type assistantSessionEraser interface {
	EraseAssistantSessionPersonalFields(ctx context.Context, principals []string) ([]store.ErasedAssistantSession, int, error)
}

// eraseAssistantSessions erases the sessions of names, writes assistant.session.end with reason erased for each
// live one it ended (a row that names no one), stops their runs and reports how many sessions it touched.
func (s *Server) eraseAssistantSessions(ctx context.Context, e assistantSessionEraser, names []string) (int, error) {
	ended, n, err := e.EraseAssistantSessionPersonalFields(ctx, names)
	if err != nil {
		return 0, err
	}
	ss, _ := e.(store.AssistantSessionStore)
	var runs []uuid.UUID
	for _, es := range ended {
		if ss != nil {
			s.assistantEndAudit(ctx, ss, types.AssistantSession{ID: es.ID, RunID: es.RunID, Principal: types.AssistantErasedPrincipal}, types.AssistantEndErased)
		}
		if es.RunID != nil {
			runs = append(runs, *es.RunID)
		}
	}
	for _, run := range assistantRunsOf(runs) {
		s.stopAssistantRun(ctx, run)
	}
	return n, nil
}

// sweepAssistantSessions removes what an ended session kept past the run-output retention window: its events,
// and its draft, letters, groups and e-mail, leaving the audit fields. A zero window keeps everything, as for run
// outputs; the caller reads it. Only an ended session is swept: one whose run ended without a read stays live
// until it is read again, so the window does not reach it.
func (s *Server) sweepAssistantSessions(ctx context.Context) error {
	ss, ok := s.cfg.Store.(store.AssistantSessionStore)
	retention := s.cfg.RunOutputRetention
	if !ok || retention <= 0 {
		return nil
	}
	n, err := ss.SweepEndedAssistantSessions(ctx, s.cfg.Now().Add(-retention), 100)
	if err != nil {
		return err
	}
	if n > 0 {
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardyn/run-output-sweeper", "run.output.retention.sweep",
			"assistant_sessions", "success", mustJSON(map[string]any{"swept": n, "retention_days": int(retention.Hours() / 24)})))
	}
	return nil
}
