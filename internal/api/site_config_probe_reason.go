// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// probeRunHints reads a probe run's failure hint, split by whose it is. A launch failure's hint (hint) is
// the reason of the first failed launch event and means no run.complete is coming. A never-started agent exec's
// (execNeverStartedHint) is the reason of the run.complete row that is still coming or already there, so the
// settle loop must not skip its wait for that row.
func (s *Server) probeRunHints(ctx context.Context, runID uuid.UUID) (hint, startHint string) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	switch {
	case err != nil || run.FailureHint == "":
		return "", ""
	case strings.HasPrefix(run.FailureHint, execNeverStartedHint):
		return "", run.FailureHint
	}
	return run.FailureHint, ""
}

// probeFailureReason words one failure-outcome audit event for a probe verdict:
// "<action>: <reason>". A launch failure's reason is the run's own failure hint (hint, what the run page
// shows), since the event's error names only the failed step; with no hint it is the event's error, else the
// action alone.
func probeFailureReason(ev *types.AuditEvent, hint string) string {
	if hint != "" {
		return fmt.Sprintf("%s: %s", ev.Action, hint)
	}
	var d struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	if d.Error != "" {
		return fmt.Sprintf("%s: %s", ev.Action, d.Error)
	}
	return ev.Action + " did not succeed"
}
