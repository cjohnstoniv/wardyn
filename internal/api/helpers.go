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
	// A store-less build (this package's own harnesses; wardynd always wires PG)
	// reaches here as a nil interface and would panic into the recoverer's opaque
	// 500. Answer the same 500 explicitly, with the reason — the workspace routes
	// authorize BEFORE they parse a body, so this is now the first store touch on
	// every one of them, and an unexplained panic there reads like an auth bug.
	if s.cfg.Store == nil {
		writeError(w, http.StatusInternalServerError, "get workspace: no store configured")
		return types.Workspace{}, false
	}
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

// ownsWorkspaceOrAdmin reports whether the caller of r may ACT ON ws as its
// owner (owned_by matches) or as an admin — the workspace analog of
// ownsRunOrAdmin, and the shared owner-or-admin PREDICATE behind both
// getWorkspaceAuthorized (mutating routes) and getWorkspaceReadable (reads).
//
// An OPERATOR-OWNED workspace (owned_by == "", every pre-0048 row and every
// admin-created one) is deliberately NOT owned by any member here: it stays
// member-READABLE via getWorkspaceReadable, but writing it stays an admin act,
// exactly as it is today. The empty-principal guard matters for the same
// reason: a caller whose principal resolves to "" must never match an
// operator-owned row's empty owned_by and inherit admin write.
//
// Deliberately isOperator, unlike its run twin ownsRunOrAdmin below — see the
// three-tier doctrine (internal/auth/oidc's RoleSecurityAdmin): a workspace
// write binds credentials and names host paths, which is the super admin's
// tier. A security admin READS the inventory (handleListWorkspaces) to govern
// it; they do not rewrite it.
func (s *Server) ownsWorkspaceOrAdmin(r *http.Request, ws types.Workspace) bool {
	if s.isOperator(r.Context()) {
		return true
	}
	principal := principalFromRequest(r)
	return ws.OwnedBy != "" && principal != "" && ws.OwnedBy == principal
}

// denyForeignWorkspace writes the refusal for a caller who may not act on a
// workspace that GENUINELY EXISTS but belongs to another member: the
// BYTE-IDENTICAL 404 getWorkspaceOr404 writes for a truly-missing id, never a
// 403 — so probing another member's workspace id learns nothing, there being no
// existence oracle distinguishing "not yours" from "does not exist". Same shape
// (and same reason) as getRunAuthorized's own deny.
//
// The authz.denied audit fires only HERE, i.e. only once the row is positively
// known to exist and to be foreign — a truly-missing workspace stays silent, so
// the audit trail is not a scan log of every 404.
func (s *Server) denyForeignWorkspace(w http.ResponseWriter, r *http.Request, ws types.Workspace) {
	writeError(w, http.StatusNotFound, "workspace not found")
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"authz.denied", ws.ID.String(), "denied", mustJSON(map[string]any{"reason": "not_owner"})))
}

// getWorkspaceAuthorized loads a workspace and authorizes the caller to MUTATE
// it as its owner or an admin (ownsWorkspaceOrAdmin) — the owner-scoped twin of
// getWorkspaceOr404, for the workspace routes a member may reach for their OWN
// workspaces (update/delete/scan/build). Callers must return immediately when
// ok is false. Two distinct refusals, and the split is the design:
//
//   - a foreign MEMBER-OWNED workspace gets the byte-identical 404
//     (denyForeignWorkspace) — no existence oracle across members.
//   - an OPERATOR-OWNED workspace gets the 403 requireOperator itself would
//     have written before these routes moved off the operatorOnly group. It is
//     already listable and readable by every member, so there is no existence
//     to hide, and answering 404 for a row the member can see in the list would
//     be a lie about a route that is simply admin-only. This keeps every
//     pre-0048 workspace's behavior BYTE-IDENTICAL to 0.5 for a member.
func (s *Server) getWorkspaceAuthorized(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.Workspace, bool) {
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return types.Workspace{}, false
	}
	if s.ownsWorkspaceOrAdmin(r, ws) {
		return ws, true
	}
	if ws.OwnedBy == "" {
		writeError(w, http.StatusForbidden, "requires admin role")
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"authz.denied", r.URL.Path, "denied", mustJSON(map[string]any{"reason": "admin_surface", "method": r.Method})))
		return types.Workspace{}, false
	}
	s.denyForeignWorkspace(w, r, ws)
	return types.Workspace{}, false
}

// getWorkspaceReadable loads a workspace for a member-tier READ (get, build
// status, observed egress, env-as-code). It is getWorkspaceAuthorized minus the
// write tier: an OPERATOR-OWNED workspace stays readable by any authenticated
// caller, exactly as it is today, while another MEMBER's owned workspace gets
// the byte-identical 404 — the one thing 0048 adds to these routes.
func (s *Server) getWorkspaceReadable(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.Workspace, bool) {
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return types.Workspace{}, false
	}
	if ws.OwnedBy == "" || s.ownsWorkspaceOrAdmin(r, ws) {
		return ws, true
	}
	s.denyForeignWorkspace(w, r, ws)
	return types.Workspace{}, false
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
//
// isSecurityOperator, not isOperator: KILLING a foreign run is incident
// response, the security admin's most time-critical act, and inspecting the run
// behind an approval or an audit line is the evidence a decision rests on.
// INSPECT-OR-STOP is the whole of that arm's warrant — run_files, run_resources,
// grants, kill, the approval/audit evidence reads.
//
// THE SANDBOX SWEEP'S TIER NOTE DEPENDS ON THIS LINE, so the two are
// cross-referenced rather than left to drift: routes.go once justified keeping
// POST /admin/sandboxes/sweep on operatorOnly as protecting "the axis the
// security tier does not get", meaning foreign-run termination — which this
// predicate grants on purpose. That note now says what is actually true (host
// reach and fleet-wide blast radius). If kill is ever narrowed to
// ownsRunOrSuperAdmin below, revisit it: TestSecurityAdminCanStopAForeignRun is
// the pin that will say so.
//
// THE SPLIT, named once: two routes sit under the same owner-or-admin shape and
// are NOT inspect-or-stop, so neither may use this predicate —
//
//   - the attach-ticket mint, which hands out an interactive shell in a foreign
//     sandbox: handleAttachTicket carries its OWN explicit strict re-check
//     against isOperator (attach_ticket.go). That guard is load-bearing, not
//     belt-and-braces: without it this one-word change silently grants a PTY.
//   - POST /runs/{id}/attach/takeover, which ENDS another human's live terminal
//     and frees the writer slot on a sandbox holding that run's injected
//     credentials: it authorizes on ownsRunOrSuperAdmin below.
func (s *Server) ownsRunOrAdmin(r *http.Request, run types.AgentRun) bool {
	return s.isSecurityOperator(r.Context()) || run.CreatedBy == principalFromRequest(r)
}

// ownsRunOrSuperAdmin is ownsRunOrAdmin's strict twin: the run's owner, or a
// SUPER admin (isOperator) — never a security_admin.
//
// It exists for the one route that WRITES into a live PTY rather than reading or
// stopping it (handleAttachTakeover). The security tier is refused an attach
// ticket (attach_ticket.go), refused the cookie attach lane (ticketOrHumanAuth →
// requireOperator) and stamped `member` on its SSH keys (sshkeys.go), so a tier
// that can reach no terminal on a foreign run must not be able to END one on it
// either: a take-over is a kick, and a kick is a write.
func (s *Server) ownsRunOrSuperAdmin(r *http.Request, run types.AgentRun) bool {
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
	return s.getRunAuthorizedBy(w, r, id, s.ownsRunOrAdmin)
}

// getRunAuthorizedBy is getRunAuthorized with the authorization PREDICATE
// supplied, so the stricter take-over gate (ownsRunOrSuperAdmin) refuses through
// the IDENTICAL response shape — the byte-identical 404 and the one not_owner
// audit — instead of growing a second, subtly different denial path beside it.
// Only the predicate differs between the two; everything a prober can observe is
// the same.
func (s *Server) getRunAuthorizedBy(w http.ResponseWriter, r *http.Request, id uuid.UUID,
	allow func(*http.Request, types.AgentRun) bool) (types.AgentRun, bool) {
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return types.AgentRun{}, false
	}
	if allow(r, run) {
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
		action, id.String(), "success", auditWorkspaceData(r, updated.OwnedBy, data(val))))
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
