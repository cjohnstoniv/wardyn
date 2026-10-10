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

// RecordRunnerConnected writes runner.connect for the first authenticated session after the runner
// was offline. The runner hub's Connected callback calls it; a resumed session writes nothing.
func (s *Server) RecordRunnerConnected(ctx context.Context, id uuid.UUID, version string) {
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "runner:"+id.String(),
		placement.ActionRunnerConnect, id.String(), "success", mustJSON(map[string]any{"version": version})))
}

// RecordRunnerDisconnected writes runner.disconnect for a session that ended and was not resumed
// inside the resume window. lastSeen is the runner's last_seen_at, or nil if it never connected.
func (s *Server) RecordRunnerDisconnected(ctx context.Context, id uuid.UUID, lastSeen *time.Time) {
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "runner:"+id.String(),
		placement.ActionRunnerDisconnect, id.String(), "success", mustJSON(map[string]any{"last_seen_at": lastSeen})))
}
