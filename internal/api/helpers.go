// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// parseIDParam parses the {param} path segment as a UUID, writing a 400
// "invalid <noun> id" and returning ok=false on failure. Callers must return
// immediately when ok is false.
func parseIDParam(w http.ResponseWriter, r *http.Request, param, noun string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+noun+" id")
		return uuid.UUID{}, false
	}
	return id, true
}

// notFoundIf writes a 404 "<entity> not found" and reports true when err is
// store.ErrNotFound; any other err (including nil) is left for the caller to
// handle. Callers must return immediately when this reports true.
func notFoundIf(w http.ResponseWriter, err error, entity string) bool {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, entity+" not found")
		return true
	}
	return false
}

// composerEnabledOrNotFound writes the standard "AI Run Composer is not
// enabled" 404 and reports true when the composer is unconfigured or
// disabled. Callers must return immediately when this reports true.
func (s *Server) composerEnabledOrNotFound(w http.ResponseWriter) bool {
	if s.cfg.Composer == nil || !s.cfg.Composer.Enabled() {
		writeError(w, http.StatusNotFound, "AI Run Composer is not enabled on this control plane")
		return true
	}
	return false
}

// claimsForRunUpload verifies claimsFromContext succeeds and its RunID
// matches the {runID} path param — the cross-run-pollution guard shared by
// every authenticated in-sandbox upload endpoint (scan result, verify
// result, recording). Writes its own error and returns ok=false on failure.
func claimsForRunUpload(w http.ResponseWriter, r *http.Request) (*identity.Claims, bool) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "missing run claims")
		return nil, false
	}
	if claims.RunID.String() != chi.URLParam(r, "runID") {
		writeError(w, http.StatusForbidden, "run id mismatch")
		return nil, false
	}
	return claims, true
}

// authSandboxRunUpload authenticates a per-run sandbox upload (scan result /
// verify result): the token's claims must match the path run id (via
// claimsForRunUpload), and the TRUSTED run row it names must be a governed
// workspace run whose Task is one of wantTasks. Writes its own error
// response and returns ok=false on any failure. GetWorkspace is
// intentionally left to each caller so the read/unmarshal/GetWorkspace error
// precedence on a malformed body stays exactly what it is today.
func (s *Server) authSandboxRunUpload(w http.ResponseWriter, r *http.Request, notFoundMsg, notGovernedMsg, wrongTaskMsg string, wantTasks ...string) (*identity.Claims, types.AgentRun, bool) {
	claims, ok := claimsForRunUpload(w, r)
	if !ok {
		return nil, types.AgentRun{}, false
	}
	run, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
	if err != nil {
		writeError(w, http.StatusForbidden, notFoundMsg)
		return nil, types.AgentRun{}, false
	}
	if run.WorkspaceID == nil {
		writeError(w, http.StatusForbidden, notGovernedMsg)
		return nil, types.AgentRun{}, false
	}
	match := false
	for _, t := range wantTasks {
		if run.Task == t {
			match = true
			break
		}
	}
	if !match {
		writeError(w, http.StatusForbidden, wrongTaskMsg)
		return nil, types.AgentRun{}, false
	}
	return claims, run, true
}

// unionAllowedDomains appends any of add not already in spec.AllowedDomains
// (deduped, in-place, empty entries skipped) and returns what was actually
// added.
func unionAllowedDomains(spec *types.RunPolicySpec, add []string) []string {
	have := map[string]bool{}
	for _, d := range spec.AllowedDomains {
		have[d] = true
	}
	var added []string
	for _, d := range add {
		if d == "" || have[d] {
			continue
		}
		have[d] = true
		spec.AllowedDomains = append(spec.AllowedDomains, d)
		added = append(added, d)
	}
	return added
}

// refreshRun re-reads a run after a state-changing step (build failure,
// dispatch) so the caller returns the store's freshest row; on read error the
// pre-step snapshot is returned unchanged.
func (s *Server) refreshRun(ctx context.Context, runID uuid.UUID, fallback types.AgentRun) types.AgentRun {
	if refreshed, err := s.cfg.Store.GetRun(ctx, runID); err == nil {
		return refreshed
	}
	return fallback
}

// getWorkspaceOr404 loads a workspace, writing a 404 (missing) or 500 (store
// error) and returning ok=false on failure. Callers must return immediately
// when ok is false.
func (s *Server) getWorkspaceOr404(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.Workspace, bool) {
	ws, err := s.cfg.Store.GetWorkspace(r.Context(), id)
	if notFoundIf(w, err, "workspace") {
		return types.Workspace{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get workspace: "+err.Error())
		return types.Workspace{}, false
	}
	return ws, true
}

// decodeStrict strict-decodes the request body into dst (unknown fields
// rejected), writing a 400 and returning false on failure.
func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// readCappedBody reads the request body under capBytes, writing a labeled 413
// (over cap) or 400 (read error) and returning ok=false on failure.
func readCappedBody(w http.ResponseWriter, r *http.Request, capBytes int64, noun string) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, capBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, noun+" exceeds size limit")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "read "+noun+": "+err.Error())
		return nil, false
	}
	return raw, true
}
