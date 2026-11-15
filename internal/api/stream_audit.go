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
	s.recordAudit(streamAuditCtx(ctx, sandboxRef), ev)
}

// recordStreamAuditDetached is recordStreamAudit for a stream's start-failure row, which its door writes on the
// channel's context: that context ends with the client's channel or connection, and a row written on it is
// refused, so the row keeps the context's values but not its end, bounded by heldStoreCallBound, the bound one
// audit row takes (recordAuditHeld). A row its bound cuts goes to the audit spool as any failed row does.
func (s *Server) recordStreamAuditDetached(ctx context.Context, sandboxRef string, ev types.AuditEvent) {
	heldCall(context.WithoutCancel(ctx), heldStoreCallBound, func(ctx context.Context) { s.recordStreamAudit(ctx, sandboxRef, ev) })
}

// recordStreamAuditDurably is recordStreamAudit that says whether the row was durably accepted
// (recordAuditDurably).
func (s *Server) recordStreamAuditDurably(ctx context.Context, sandboxRef string, ev types.AuditEvent) bool {
	return s.recordAuditDurably(streamAuditCtx(ctx, sandboxRef), ev)
}

// streamAuditCtx is ctx carrying the relay a runner-relayed sandboxRef names, for the audit row.
func streamAuditCtx(ctx context.Context, sandboxRef string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ref, ok := strings.CutPrefix(sandboxRef, "runner:"); ok {
		name, local, found := strings.Cut(ref, "/")
		if id, err := uuid.Parse(name); found && local != "" && err == nil && name == id.String() {
			ctx = audit.WithRelay(ctx, id)
		}
	}
	return ctx
}
