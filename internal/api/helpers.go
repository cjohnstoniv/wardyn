// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sortedKeys returns m's keys in order, never nil. slices.Sorted yields nil for
// an empty map, which marshals to `null` rather than `[]` — use this wherever
// the result reaches a response body or an audit payload.
func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	if len(m) == 0 {
		return []K{}
	}
	return slices.Sorted(maps.Keys(m))
}

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
	if run.WorkspaceID == nil && run.SourceID == nil {
		// Governed = carries a trusted linkage: a workspace step run OR a
		// per-source scan run (the three-tier retarget).
		writeError(w, http.StatusForbidden, notGovernedMsg)
		return nil, types.AgentRun{}, false
	}
	if !slices.Contains(wantTasks, run.Task) {
		writeError(w, http.StatusForbidden, wrongTaskMsg)
		return nil, types.AgentRun{}, false
	}
	return claims, run, true
}

// unionDomains appends any of add not already in *dst (deduped, in-place,
// empty entries skipped) and returns what was actually added. Dedupe is
// case/space-insensitive — the same key composer.Clamp and domainAllowedExact
// already use, and what the proxy concludes (CompilePolicy lowercases before
// matching). Keying on raw bytes instead let an operator's `API.Anthropic.com`
// re-add a second, semantically identical entry AND report it to the operator
// as a widening that had not happened. Stored spelling is the operator's own;
// only the equality test is normalized.
//
// Takes *[]string rather than *RunPolicySpec so it can back BOTH
// unionAllowedDomains (spec.AllowedDomains) and unionDeniedDomains
// (spec.DeniedDomains, Phase 4) off one implementation — the two lists share
// the exact same dedupe/append contract, only the target field differs.
func unionDomains(dst *[]string, add []string) []string {
	key := func(d string) string { return strings.ToLower(strings.TrimSpace(d)) }
	have := map[string]bool{}
	for _, d := range *dst {
		have[key(d)] = true
	}
	var added []string
	for _, d := range add {
		if d == "" || have[key(d)] {
			continue
		}
		have[key(d)] = true
		*dst = append(*dst, d)
		added = append(added, d)
	}
	return added
}

// unionAllowedDomains is unionDomains applied to spec.AllowedDomains — see
// unionDomains for the dedupe rule. ~11 production call sites key on this
// exact signature; extend behavior via unionDomains, never by changing this
// one's shape.
func unionAllowedDomains(spec *types.RunPolicySpec, add []string) []string {
	return unionDomains(&spec.AllowedDomains, add)
}

// unionDeniedDomains is unionAllowedDomains' Phase-4 twin: unionDomains applied
// to spec.DeniedDomains instead of AllowedDomains. Used by unionWorkspaceEgress
// to fold a workspace's permanent `deny · always` decisions (Workspace.
// DeniedEgress) into a run the same way ApprovedEgress folds into
// AllowedDomains. No production call site reports what this adds the way
// unionAllowedDomains's return is consumed elsewhere — see unionWorkspaceEgress
// for why the denied side stays out of ITS return value too.
func unionDeniedDomains(spec *types.RunPolicySpec, add []string) []string {
	return unionDomains(&spec.DeniedDomains, add)
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

// getRunOr404 loads a run, writing a 404 (missing) or 500 (store error) and
// returning ok=false on failure — the run-noun twin of getWorkspaceOr404.
// Callers must return immediately when ok is false.
func (s *Server) getRunOr404(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.AgentRun, bool) {
	run, err := s.cfg.Store.GetRun(r.Context(), id)
	if notFoundIf(w, err, "run") {
		return types.AgentRun{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return types.AgentRun{}, false
	}
	return run, true
}

// ownsRunOrAdmin reports whether the caller of r may act on run as its owner
// (created_by matches) or as an admin. The shared owner-or-admin PREDICATE
// behind getRunAuthorized (which shapes an HTTP response) and any other call
// site that needs the same decision without getRunAuthorized's specific
// "run not found" wording (e.g. the approval decide path, which owns "approval
// not found" instead).
func (s *Server) ownsRunOrAdmin(r *http.Request, run types.AgentRun) bool {
	return s.isOperator(r.Context()) || run.CreatedBy == principalFromRequest(r)
}

// getRunAuthorized loads a run and authorizes the caller as its owner or an
// admin (ownsRunOrAdmin) — the owner-scoped twin of getRunOr404, for every
// /runs/{id} route a member may reach for their OWN runs (get/kill/profile/
// grants/attach-ticket). A non-owner, non-admin caller gets the BYTE-IDENTICAL
// 404 a truly-missing run would (getRunOr404's own status/message) — never a
// 403 — so probing another user's run id learns nothing: there is no existence
// oracle distinguishing "not yours" from "does not exist". Callers must return
// immediately when ok is false.
func (s *Server) getRunAuthorized(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.AgentRun, bool) {
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return types.AgentRun{}, false
	}
	if s.ownsRunOrAdmin(r, run) {
		return run, true
	}
	writeError(w, http.StatusNotFound, "run not found")
	// M1: audited AFTER confirming the run genuinely exists — a truly-missing
	// run (the getRunOr404 branch above) stays silent, so only a POSITIVELY
	// identified foreign run reaches this audit (reason not_owner). The response
	// written above is unaffected (byte-identical either way).
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorTypeFromRequest(r), principalFromRequest(r),
		"authz.denied", run.ID.String(), "denied", mustJSON(map[string]any{"reason": "not_owner"})))
	return types.AgentRun{}, false
}

// scopedWorkspaceWrite is the shared body of the operator-owned single-column
// workspace writes (PUT semantics, full replacement): parse the id, strict-decode
// the body into T, normalize+validate it into the stored value V (a non-empty
// message is a 400), write JUST that column so a concurrently-finishing async
// scan's profile/status is never clobbered, audit, and return the updated
// workspace. Callers supply only what genuinely differs: normalize, the store
// setter, and the audit payload — which stays count/shape-only, never
// value-shaped, for anything that could carry repo content.
func scopedWorkspaceWrite[T, V any](s *Server, w http.ResponseWriter, r *http.Request, action string,
	normalize func(T) (V, string),
	set func(context.Context, uuid.UUID, V) (types.Workspace, error),
	data func(V) map[string]any,
) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	var req T
	if !decodeStrict(w, r, &req) {
		return
	}
	val, msg := normalize(req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	updated, err := set(r.Context(), id, val)
	if notFoundIf(w, err, "workspace") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, action+": "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, id.String(), "success", mustJSON(data(val))))
	writeJSON(w, http.StatusOK, updated)
}

// maxJSONBody caps a control-plane JSON request body. Nothing this package
// decodes as JSON is anywhere near it; the uploads that legitimately are (raw
// recording, scan/verify/compose results, composer input, the ground-truth
// batch) carry their own larger, named cap at their own handler.
const maxJSONBody = 1 << 20 // 1 MiB

// decodeStrictMsg strict-decodes the request body into dst (unknown fields
// rejected) under maxJSONBody, returning a human-readable message on failure
// and "" on success. This is the package's single JSON-body decode primitive —
// decode somewhere else and the cap is silently lost. An over-cap body surfaces
// as the 400 below ("... request body too large"), not a labeled 413; use
// readCappedBody where the 413 matters.
func decodeStrictMsg(w http.ResponseWriter, r *http.Request, dst any) string {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return "invalid JSON body: " + err.Error()
	}
	return ""
}

// decodeStrict is decodeStrictMsg with the 400 written for the caller, for the
// handlers that have no further validation message of their own. Callers must
// return immediately when it reports false.
func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	if msg := decodeStrictMsg(w, r, dst); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
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
