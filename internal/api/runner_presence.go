// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RecordRunnerConnected writes runner.connect for a runner's authenticated session. resumed is true
// for a session that resumed an earlier one; the runner hub calls it for the first authenticated
// session after the runner was offline, so the hub's wiring decides whether a resume writes a row.
func (s *Server) RecordRunnerConnected(ctx context.Context, id uuid.UUID, version string, resumed bool) {
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "runner:"+id.String(),
		placement.ActionRunnerConnect, id.String(), "success", mustJSON(map[string]any{"version": version, "resumed": resumed})))
}

// RecordRunnerDisconnected writes runner.disconnect for a session that ended and was not resumed
// inside the resume window. lastSeen is the runner's last_seen_at, or nil if it never connected.
func (s *Server) RecordRunnerDisconnected(ctx context.Context, id uuid.UUID, lastSeen *time.Time) {
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "runner:"+id.String(),
		placement.ActionRunnerDisconnect, id.String(), "success", mustJSON(map[string]any{"last_seen_at": lastSeen})))
}
