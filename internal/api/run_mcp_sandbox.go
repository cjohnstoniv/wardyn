// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The audit rows of an MCP server's own sandbox. Each names the server by its ordinal and key, and never its
// command, arguments or environment, which a person may have written.

// mcpSandboxDatum is what an MCP server's rows say of it, with the cause when there is one.
func mcpSandboxDatum(row store.RunMCPSandbox, cause string) map[string]any {
	d := map[string]any{"ordinal": row.Ordinal, "mcp_sandbox": row.ServerKey}
	if cause != "" {
		d["cause"] = cause
	}
	return d
}

// auditMCPSandboxCreate writes run.mcp_sandbox.create for row: success with its sandbox, or failure or denied
// with the cause.
func (s *Server) auditMCPSandboxCreate(ctx context.Context, row store.RunMCPSandbox, outcome, cause string) {
	data := mcpSandboxDatum(row, cause)
	if row.SandboxRef != "" {
		data["sandbox_ref"] = row.SandboxRef
	}
	s.recordAudit(ctx, s.auditEvent(&row.RunID, types.ActorSystem, "wardynd", "run.mcp_sandbox.create", row.ServerKey, outcome, mustJSON(data)))
}

// auditMCPSandboxRestart writes run.mcp_sandbox.restart for row, by the restart watcher (actor system) or by
// a person, with the cause and the restarts the row counts in its window.
func (s *Server) auditMCPSandboxRestart(ctx context.Context, actorType types.ActorType, actor string, row store.RunMCPSandbox, outcome, cause string) {
	data := mcpSandboxDatum(row, cause)
	data["restarts"] = row.Restarts
	s.recordAudit(ctx, s.auditEvent(&row.RunID, actorType, actor, "run.mcp_sandbox.restart", row.ServerKey, outcome, mustJSON(data)))
}

// auditMCPSandboxEnd writes run.mcp_sandbox.end for row with its cause, by the run's end (actor system) or a
// person's stop. With row nil it is the end of every server of the run whose rows could not be read.
func (s *Server) auditMCPSandboxEnd(ctx context.Context, runID uuid.UUID, actorType types.ActorType, actor string, row *store.RunMCPSandbox, outcome, cause string) {
	data, target := map[string]any{}, ""
	if row != nil {
		data, target = mcpSandboxDatum(*row, cause), row.ServerKey
	} else if cause != "" {
		data["cause"] = cause
	}
	s.recordAudit(ctx, s.auditEvent(&runID, actorType, actor, "run.mcp_sandbox.end", target, outcome, mustJSON(data)))
}
