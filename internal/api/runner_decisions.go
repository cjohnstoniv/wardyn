// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// DecisionIngestFromRunner is the checked Hub.Event spool adapter. runnerID
// comes exclusively from H2's authenticated stored identity, never event data.
// The store rechecks claimed ownership and revocation in the committing tx.
func (s *Server) DecisionIngestFromRunner(ctx context.Context, runnerID uuid.UUID, event runnerwire.Event) error {
	if event.Kind != runnerwire.EventSpool || event.RunID == nil || len(event.Data) > runnerspool.MaxRecordBytes || s.cfg.RunnerAuditPrepare == nil {
		return store.ErrRunnerSpoolRefused
	}
	target, ok := s.cfg.Store.(interface {
		IngestRunnerDecision(context.Context, uuid.UUID, runnerspool.Record, func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error)
	})
	if !ok {
		return store.ErrRunnerSpoolRefused
	}
	var row runnerspool.Record
	dec := json.NewDecoder(bytes.NewReader(event.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&row); err != nil {
		return store.ErrRunnerSpoolRefused
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return store.ErrRunnerSpoolRefused
	}
	if row.Decision.Request.RunID != *event.RunID {
		return store.ErrRunnerSpoolRefused
	}
	first, err := target.IngestRunnerDecision(ctx, runnerID, row, s.cfg.RunnerAuditPrepare)
	if err != nil || !first {
		return err
	}
	// The post-audit hooks handlePostDecision runs, once per first-time row, so a spooled
	// decision moves the same counters and run hint a direct one does. The idle clock is
	// not touched: the spool is audit only.
	d := row.Decision
	if d.Scan != nil && d.Scan.Action == "bypass" {
		return nil
	}
	if d.Decision == egress.Deny && row.Count == 0 && isPolicyDeny(d.RuleSource) {
		s.metrics.egressDenied()
	}
	s.countReauthTimeout(ctx, d)
	if d.UpstreamFault != "" {
		s.noteBedrockDataPlaneFault(context.WithoutCancel(ctx), *event.RunID, d.UpstreamFault)
	}
	return nil
}
