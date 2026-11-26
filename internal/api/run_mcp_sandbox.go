// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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

// The causes a row's move to stopped records (last_cause), one per writer.
const (
	mcpEndCompleted = "completed"     // the run's end (finalizeRunEffects)
	mcpEndKilled    = "killed"        // a kill (killTeardownTail, the kill confirm)
	mcpEndUnstarted = "unstarted"     // dispatch returned before the run was RUNNING
	mcpEndLost      = "lost"          // a lost path
	mcpEndEnded     = "ended"         // the lease's end kept the run
	mcpEndPerson    = "person"        // a person's Stop
	mcpEndLimit     = "restart_limit" // past mcpRestartMax restarts in mcpRestartWindow
	mcpEndReplaced  = "superseded"    // a revive's keep found the row moved on
)

// mcpSandboxEndTimeout bounds one run's MCP end, its own bound and never the caller's: each row's graceful
// proxy stop, its kill and its row write (four servers' stops fit in it, EST).
const mcpSandboxEndTimeout = reconcileFinalizeTimeout

// mcpSandboxSeams are the store and the runner an MCP sandbox needs; ok is false on a deployment with neither.
func (s *Server) mcpSandboxSeams() (store.RunMCPSandboxStore, runner.MCPSandboxer, bool) {
	st, ok := s.cfg.Store.(store.RunMCPSandboxStore)
	m, mok := s.cfg.Runner.(runner.MCPSandboxer)
	return st, m, ok && mok
}

// mcpSandboxIDOf is the runner's name for row, which a row with no sandbox ref still has.
func mcpSandboxIDOf(run types.AgentRun, row store.RunMCPSandbox) runner.MCPSandboxID {
	return runner.MCPSandboxID{RunID: run.ID, Ordinal: row.Ordinal, Class: run.ConfinementClass}
}

// endRunMCPSandboxes ends every MCP sandbox of the run that is not stopped: each one's proxy is stopped
// gracefully, so it flushes its last decisions, its container is killed, and its row is stopped with cause.
// It runs on its own bound and returns nothing, so no caller's own stop or answer changes, and it is not
// fenced: a newer revive's servers are ended too, which costs availability only. When the rows cannot be
// read it kills every MCP object of the run by listing, so no container or secret outlives the cut; the rows
// keep their state until a later end or the watcher's kill. It is idempotent.
func (s *Server) endRunMCPSandboxes(ctx context.Context, runID uuid.UUID, cause string) {
	st, m, ok := s.mcpSandboxSeams()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpSandboxEndTimeout)
	defer cancel()
	rows, err := st.ListRunMCPSandboxes(ctx, runID)
	var run types.AgentRun
	if err == nil && slices.ContainsFunc(rows, mcpRowUp) {
		run, err = s.cfg.Store.GetRun(ctx, runID) // the class names the substrate
	}
	if err != nil {
		kerr := m.KillRunMCPSandboxes(ctx, runID)
		slog.WarnContext(ctx, "wardynd: a run's MCP servers could not be read at its end; every one found was killed",
			slog.String("run_id", runID.String()), slog.Any("err", err), slog.Any("kill_err", kerr))
		s.auditMCPSandboxEnd(ctx, runID, types.ActorSystem, "wardynd", nil, "failure", cause)
		return
	}
	for _, row := range rows {
		if mcpRowUp(row) {
			s.endMCPSandbox(ctx, run, row, cause)
		}
	}
}

// mcpRowUp reports whether a row is not stopped.
func mcpRowUp(row store.RunMCPSandbox) bool { return row.State != store.MCPSandboxStopped }

// endMCPSandbox is endRunMCPSandboxes' end of one row, by its name, so a starting row with no ref is ended
// too: the kill (the proxy drained first), then the row stopped with cause, then run.mcp_sandbox.end. A row
// another writer stopped first keeps its cause and writes no second row. A kill that failed leaves the row
// up, with a failure row, so the run's containment lists it and the kill confirm and the watcher kill it
// again; nothing restarts it, since a run that is over, lost or kept is never one mcpCreateAllowed admits.
// gone reports whether the kill succeeded.
func (s *Server) endMCPSandbox(ctx context.Context, run types.AgentRun, row store.RunMCPSandbox, cause string) (gone bool) {
	st, m, ok := s.mcpSandboxSeams()
	if !ok {
		return true
	}
	if kerr := m.KillMCPSandbox(ctx, mcpSandboxIDOf(run, row)); kerr != nil {
		slog.WarnContext(ctx, "wardynd: killing an MCP server's sandbox failed", slog.String("run_id", run.ID.String()),
			slog.String("server", row.ServerKey), slog.Any("err", kerr))
		s.auditMCPSandboxEnd(ctx, run.ID, types.ActorSystem, "wardynd", &row, "failure", cause)
		return false
	}
	stopped, werr := st.StopMCPSandboxRow(ctx, run.ID, row.Ordinal, cause)
	if werr == nil && !stopped {
		return true
	}
	outcome := "success"
	if werr != nil {
		outcome = "failure"
		slog.WarnContext(ctx, "wardynd: recording an MCP server's end failed", slog.String("run_id", run.ID.String()),
			slog.String("server", row.ServerKey), slog.Any("err", werr))
	}
	s.auditMCPSandboxEnd(ctx, run.ID, types.ActorSystem, "wardynd", &row, outcome, cause)
	return true
}

// mcpSandboxesUp is the keys of the run's MCP sandbox rows that are not stopped, in ordinal order.
func (s *Server) mcpSandboxesUp(ctx context.Context, runID uuid.UUID) ([]string, error) {
	st, _, ok := s.mcpSandboxSeams()
	if !ok {
		return nil, nil
	}
	rows, err := st.ListRunMCPSandboxes(ctx, runID)
	if err != nil {
		return nil, err
	}
	var up []string
	for _, row := range rows {
		if mcpRowUp(row) {
			up = append(up, row.ServerKey)
		}
	}
	return up, nil
}

// confirmMCPSandboxesGone is the kill confirm's MCP step: every sandbox of the run is killed again by its
// name, each row still up is ended, and while a kill fails or the rows cannot be read the run's containment
// is not confirmed (errKillConfirmBusy, retried).
func (s *Server) confirmMCPSandboxesGone(ctx context.Context, run types.AgentRun) error {
	st, m, ok := s.mcpSandboxSeams()
	if !ok {
		return nil
	}
	rows, err := st.ListRunMCPSandboxes(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("%w: read the run's MCP servers: %w", errKillConfirmBusy, err)
	}
	var left []string
	for _, row := range rows {
		var gone bool
		if mcpRowUp(row) {
			gone = s.endMCPSandbox(ctx, run, row, mcpEndKilled)
		} else {
			gone = m.KillMCPSandbox(ctx, mcpSandboxIDOf(run, row)) == nil
		}
		if !gone {
			left = append(left, row.ServerKey)
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("%w: the MCP servers %s are not confirmed gone", errKillConfirmBusy, strings.Join(left, ", "))
	}
	return nil
}

// mcpSandboxesUpAfterEnd is mcpSandboxesUp for a run that is over, nil for one still running: its MCP servers
// are then the run's own, not containment.
func (s *Server) mcpSandboxesUpAfterEnd(ctx context.Context, run types.AgentRun) ([]string, error) {
	if !isTerminalRunState(run.State) {
		return nil, nil
	}
	return s.mcpSandboxesUp(ctx, run.ID)
}
