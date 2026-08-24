// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// The two halves of "an admin acting on a MEMBER's workspace stays visible":
// the offboarding reassign (decision O6) and the cross-user audit marker
// (decision O5) every workspace-scoped write stamps.

// handleReassignWorkspace returns a member-owned workspace to the operator
// (owned_by = ""), the offboarding path for a member who has left: their
// owned rows would otherwise point at an identity nobody can sign in as.
//
// ADMIN-ONLY, enforced by the operatorOnly group in routes.go rather than
// in here — deliberately, because that refusal is written before any lookup
// and is therefore a CONSTANT 403 for every id a member can name: existing,
// foreign, own, or invented. That is strictly blinder than the
// getWorkspaceAuthorized 404/403 split the owner-or-admin routes use, where
// the status necessarily varies with what the caller may already see. Owning a
// workspace does not let a member disown it, so there is no owner tier here at
// all.
//
// IDEMPOTENT on purpose: reassigning an already-operator-owned row succeeds and
// audits from_owner "" — offboarding runs over a list of ids and must not fail
// halfway through because one of them was already handled.
//
// ONE CONSEQUENCE WORTH NAMING: the row's local_dir sources stop being
// member-authored the moment ownership moves, so memberMountPosture no longer
// resolves roots for them and the member root/dotfile gate no longer applies —
// they become ordinary operator mounts, bounded by ValidateMountSource alone.
// That is the correct reading of "the operator owns this now", and it is not a
// widening a member can reach: only an admin may call this, and an admin could
// already onboard any host path directly. It IS an authoring act, though, so
// reassign a departed member's workspace with the same care as creating one.
func (s *Server) handleReassignWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	// Read first: from_owner is the whole point of the audit row, and the
	// scoped write below overwrites it.
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	updated, err := s.cfg.Store.SetWorkspaceOwner(r.Context(), id, "")
	if notFoundIf(w, err, "workspace") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reassign workspace: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.reassign", id.String(), "success",
		auditWorkspaceData(r, ws.OwnedBy, map[string]any{"from_owner": ws.OwnedBy})))
	writeJSON(w, http.StatusOK, updated)
}

// auditWorkspaceData marshals a workspace-scoped write's audit Data, stamping
// the O5 cross-user marker: when the ACTOR is not the workspace's owner — i.e.
// an admin acting on a member-owned workspace, for support or offboarding —
// the event additionally carries workspace_owner. That makes cross-user admin
// access to member data QUERYABLE (?actor=<admin> plus workspace_owner != actor)
// rather than merely present in the log.
//
// What it deliberately does NOT do is change the actor. The actor stays the
// admin's own principal, exactly as auditEvent's caller passes it: an admin
// acting on a member's workspace must never be recorded as the member. There is
// no impersonation anywhere in this path, and TestWorkspaceOwner_NoImpersonation
// pins that.
//
// owner "" (an operator-owned workspace, i.e. every pre-0.6 row) never stamps,
// so the audit Data of an admin-only deployment is byte-identical to what it
// was before ownership existed. A nil map with nothing to add stays nil rather
// than becoming `null`, so a caller that logs no Data still logs no Data.
func auditWorkspaceData(r *http.Request, owner string, data map[string]any) json.RawMessage {
	return auditWorkspaceDataFor(principalFromRequest(r), owner, data)
}

// auditWorkspaceDataFor is the same stamp for a writer that holds the acting
// principal as a STRING rather than the request it came from — the approval
// write-backs (persistWorkspaceEgressDecision, learnVerifyEgress) run off the
// decide handler with `by` already resolved and no *http.Request in hand.
// Splitting here rather than threading a request keeps ONE stamping rule: a
// second copy of the "actor != owner" comparison is exactly how the marker
// drifts onto some writes and off others.
func auditWorkspaceDataFor(actor, owner string, data map[string]any) json.RawMessage {
	if owner != "" && owner != actor {
		if data == nil {
			data = map[string]any{}
		}
		data["workspace_owner"] = owner
	}
	if data == nil {
		return nil
	}
	return mustJSON(data)
}

// workspaceOwner reads a workspace's owner for the O5 marker ALONE, on the
// give-up paths of writers that never got a row back to read it from (the
// durable write failed, or was never attempted). Every failure answers "" — no
// store, a row deleted mid-flight — which stamps nothing, so a marker lookup
// can never turn a recorded miss into an unrecorded one.
func (s *Server) workspaceOwner(ctx context.Context, id uuid.UUID) string {
	if s.cfg.Store == nil {
		return ""
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, id)
	if err != nil {
		return ""
	}
	return ws.OwnedBy
}
