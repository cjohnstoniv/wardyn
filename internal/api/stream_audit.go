// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordStreamAudit uses the authorized run's actual substrate reference, not a
// request's placement hint. The caller's finish context survives disconnects.
func (s *Server) recordStreamAudit(ctx context.Context, sandboxRef string, ev types.AuditEvent) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ref, ok := strings.CutPrefix(sandboxRef, "runner:"); ok {
		name, local, found := strings.Cut(ref, "/")
		if id, err := uuid.Parse(name); found && local != "" && err == nil && name == id.String() {
			ctx = audit.WithRelay(ctx, id)
		}
	}
	s.recordAudit(ctx, ev)
}
