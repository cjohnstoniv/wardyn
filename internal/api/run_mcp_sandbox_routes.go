// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run page's MCP server routes: the list, a person's Restart (the owner or a super admin, under the
// run's lock and only on a live run) and a person's Stop (also a security admin, as the kill route; no
// lock: the row is stopped first, then its sandbox killed).

// mountRunMCPRoutes registers the three run-page routes of a run's MCP servers that start from a command.
func (s *Server) mountRunMCPRoutes(r chi.Router) {
	r.Get("/runs/{id}/mcp-servers", s.handleListRunMCPServers)
	r.Post("/runs/{id}/mcp-servers/{key}/restart", s.handleRestartRunMCPServer)
	r.Post("/runs/{id}/mcp-servers/{key}/stop", s.handleStopRunMCPServer)
}

// mcpServerView is one row of the run page's MCP servers panel. It names the server by its key and never its
// command, arguments or environment.
type mcpServerView struct {
	Key       string     `json:"key"`
	State     string     `json:"state"`
	LastCause string     `json:"last_cause,omitempty"`
	Restarts  int        `json:"restarts"`
	CPUMillis int64      `json:"cpu_millis,omitempty"`
	MemoryMiB int64      `json:"memory_mib,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
}

func mcpServerViewOf(row store.RunMCPSandbox) mcpServerView {
	return mcpServerView{Key: row.ServerKey, State: row.State, LastCause: row.LastCause, Restarts: row.Restarts,
		CPUMillis: row.Reserved.CPUMillis, MemoryMiB: row.Reserved.MemoryMiB, UpdatedAt: row.UpdatedAt, StoppedAt: row.StoppedAt}
}

// runMCPRows is the run's MCP sandbox rows, none on a store or runner without them.
func (s *Server) runMCPRows(r *http.Request, run types.AgentRun) ([]store.RunMCPSandbox, error) {
	st, _, ok := s.mcpSandboxSeams()
	if !ok {
		return nil, nil
	}
	return st.ListRunMCPSandboxes(r.Context(), run.ID)
}

// writeRunMCPServers answers the run's rows as the panel reads them.
func (s *Server) writeRunMCPServers(w http.ResponseWriter, r *http.Request, run types.AgentRun) {
	rows, err := s.runMCPRows(r, run)
	if err != nil {
		writeServerError(w, r, "read the run's MCP servers", err)
		return
	}
	views := make([]mcpServerView, 0, len(rows))
	for _, row := range rows {
		views = append(views, mcpServerViewOf(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"mcp_servers": views})
}

// handleListRunMCPServers is GET /api/v1/runs/{id}/mcp-servers: the owner or an admin.
func (s *Server) handleListRunMCPServers(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrAdmin)
	if !ok {
		return
	}
	s.writeRunMCPServers(w, r, run)
}

// writeMCPServerUnknown answers a key the run has no server for (404).
func writeMCPServerUnknown(w http.ResponseWriter, key string) {
	writeErrorReason(w, http.StatusNotFound, reasonMCPSandboxUnknown, mcpSandboxSentence(mcpSandboxUnknownSentence, key, ""))
}

// handleRestartRunMCPServer is POST /api/v1/runs/{id}/mcp-servers/{key}/restart: the owner or a super admin.
// It takes the run's lock (503 lock_unavailable when another operation holds it), and answers 409
// mcp_sandbox_run_ended when the run is not live (mcpCreateAllowed) and 404 mcp_sandbox_unknown for a key the
// run has no server for. A restart whose create fails answers the row, failed.
func (s *Server) handleRestartRunMCPServer(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return
	}
	key := chi.URLParam(r, "key")
	lctx, unlock, ok := s.tryLockRunOp(r.Context(), run.ID)
	if !ok {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg)
		return
	}
	defer unlock()
	actorType, actor := actorFromRequest(r)
	switch err := s.restartMCPSandbox(lctx, run, key, actorType, actor, false); {
	case errors.Is(err, errMCPSandboxUnknownKey):
		writeMCPServerUnknown(w, key)
		return
	case errors.Is(err, errMCPRestartRunEnded):
		writeErrorReason(w, http.StatusConflict, reasonMCPSandboxRunEnded, mcpSandboxRunEndedSentence)
		return
	case err != nil:
		writeServerError(w, r, "restart the MCP server", err)
		return
	}
	s.writeRunMCPServers(w, r, run)
}

// handleStopRunMCPServer is POST /api/v1/runs/{id}/mcp-servers/{key}/stop: the owner or an admin, a security
// admin included, as the kill route. It takes no run lock (OP53-1): the row is stopped (person) first, so a
// revive's reopen or keep and a restart's ready write lose to it, then the sandbox is killed. An ended run
// answers 409 mcp_sandbox_run_ended.
func (s *Server) handleStopRunMCPServer(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrAdmin)
	if !ok {
		return
	}
	key := chi.URLParam(r, "key")
	if isTerminalRunState(run.State) {
		writeErrorReason(w, http.StatusConflict, reasonMCPSandboxRunEnded, mcpSandboxRunEndedSentence)
		return
	}
	st, m, ok := s.mcpSandboxSeams()
	if !ok {
		writeMCPServerUnknown(w, key)
		return
	}
	rows, err := st.ListRunMCPSandboxes(r.Context(), run.ID)
	if err != nil {
		writeServerError(w, r, "read the run's MCP servers", err)
		return
	}
	i := slices.IndexFunc(rows, func(row store.RunMCPSandbox) bool { return row.ServerKey == key })
	if i < 0 {
		writeMCPServerUnknown(w, key)
		return
	}
	row := rows[i]
	stopped, err := st.StopMCPSandboxRow(r.Context(), run.ID, row.Ordinal, mcpEndPerson)
	if err != nil {
		writeServerError(w, r, "stop the MCP server", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	outcome := "success"
	if kerr := m.KillMCPSandbox(r.Context(), mcpSandboxIDOf(run, row)); kerr != nil {
		outcome = "failure" // the row is stopped: the kill confirm and the run's end kill it again
	}
	if stopped || outcome == "failure" {
		s.auditMCPSandboxEnd(r.Context(), run.ID, actorType, actor, &row, outcome, mcpEndPerson)
	}
	s.writeRunMCPServers(w, r, run)
}
