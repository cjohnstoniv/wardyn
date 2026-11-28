// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// repairAuditStore holds the run.output.finalize events owed by complete output rows that replaced incomplete
// ones (store.PG). The replacement writes its event's row in its own transaction, so the event survives a stop
// between the commit and the append.
type repairAuditStore interface {
	OwedRunOutputRepairAudits(ctx context.Context, limit int) ([]store.RunOutputRepairAudit, error)
	SettleRunOutputRepairAudit(ctx context.Context, eventID uuid.UUID) error
}

// repairAuditPage is how many owed repair events one sweep delivers.
const repairAuditPage = 100

func (s *Server) repairAudits() repairAuditStore {
	st, _ := s.cfg.Store.(repairAuditStore)
	return st
}

// deliverRepairAudit appends one owed repair event and settles it once the recorder accepted it or spooled it. It
// is a replay of the event's fixed id and time (store.WithAuditReplay), so the audit table keeps one row however
// often it is sent. A refusal leaves it owed for the next sweep.
func (s *Server) deliverRepairAudit(ctx context.Context, st repairAuditStore, owed store.RunOutputRepairAudit) {
	ev := s.auditEvent(&owed.RunID, types.ActorSystem, "wardynd", "run.output.finalize", owed.RunID.String(), "success",
		mustJSON(withFinalizeStep(map[string]any{"replaced_incomplete": true})))
	ev.ID, ev.Time = owed.EventID, owed.EventAt.UTC()
	if !s.recordAuditDurably(store.WithAuditReplay(ctx), ev) {
		slog.WarnContext(ctx, "wardynd: a replaced output row's audit event was not accepted, it stays owed",
			slog.String("run_id", owed.RunID.String()))
		return
	}
	if err := st.SettleRunOutputRepairAudit(ctx, owed.EventID); err != nil {
		slog.WarnContext(ctx, "wardynd: could not settle a replaced output row's audit event, it is sent again",
			slog.String("run_id", owed.RunID.String()), slog.Any("err", err))
	}
}

// settleRepairAudits delivers one page of owed repair events, oldest first.
func (s *Server) settleRepairAudits(ctx context.Context) error {
	st := s.repairAudits()
	if st == nil {
		return nil
	}
	owed, err := st.OwedRunOutputRepairAudits(ctx, repairAuditPage)
	if err != nil {
		return err
	}
	for _, o := range owed {
		if ctx.Err() != nil {
			return nil
		}
		s.deliverRepairAudit(ctx, st, o)
	}
	return nil
}
