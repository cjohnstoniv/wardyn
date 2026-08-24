// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Permissioning CRUD (0.6 pillar 2, stage A-C): the admin-facing surface over
// capability_grants + capability_enforcement (migration 0042) and the
// member-safe read of a caller's own effective set. The resolver these routes
// feed lives in capabilities.go; this file is only validation + persistence +
// audit for the four writes plus the two reads.
package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxCapabilityGrantFieldLen bounds subject/value on write. Generous for a
// group name, an email, a host, or an image ref; small enough that a grant
// table stays a table, not a blob store.
const maxCapabilityGrantFieldLen = 512

// permissionsResponse is GET /permissions's body: the whole grant table plus
// the per-kind enforcement switches, in one call — the admin Permissions
// screen's entire data need (plan: "one call").
type permissionsResponse struct {
	Grants      []types.CapabilityGrant `json:"grants"`
	Enforcement map[string]bool         `json:"enforcement"`
}

// handleGetPermissions returns every capability grant (admin audience — the
// FULL table, unlike GET /me/capabilities below) plus the enforcement switch
// map. operatorOnly (routes.go).
func (s *Server) handleGetPermissions(w http.ResponseWriter, r *http.Request) {
	grants, err := s.cfg.Store.ListCapabilityGrants(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list capability grants: "+err.Error())
		return
	}
	enf, err := s.cfg.Store.GetCapabilityEnforcement(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get capability enforcement: "+err.Error())
		return
	}
	// The ETag covers ONLY the enforcement map, not Grants: it exists so a
	// caller can round-trip it as PUT /permissions/enforcement's If-Match
	// (etag.go) — that endpoint's own whole-document replace, not this one.
	w.Header().Set("ETag", computeETag(enf))
	writeJSON(w, http.StatusOK, permissionsResponse{Grants: grants, Enforcement: enf})
}

// grantWriteRequest is POST /permissions/grants's body. ID/CreatedAt/CreatedBy
// are never accepted from the wire — the natural key (subject_type, subject,
// capability, value) is what a caller names; the row's identity and
// provenance are always server-assigned.
type grantWriteRequest struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	Capability  string                      `json:"capability"`
	Value       string                      `json:"value"`
	Effect      types.CapabilityEffect      `json:"effect"`
}

// controlCharFree reports whether s has no control characters or DEL. Applied
// to every grant field on write: a subject/value round-trips into an admin
// screen and (for egress_host once D-wiring lands) an egress comparison, so
// this is the same cheap hygiene validateSiteConfig applies to URL/host
// fields — not a full per-kind shape validator, which the plan leaves to the
// closed kind set's own resolver-side matching (capValueMatches).
func controlCharFree(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validateCapabilityGrant normalizes g in place (trims, lowercases the
// subject to match how capabilitySubjects/sessionGroups normalize a caller's
// own identity) and validates it against the closed kind set + the subject
// shape migration 0042 documents. Fail closed: an unknown kind, an invalid
// subject_type/effect, or an empty value/subject is rejected outright rather
// than stored inert.
func validateCapabilityGrant(g *types.CapabilityGrant) error {
	if !g.SubjectType.Valid() {
		return fmt.Errorf("subject_type: invalid %q", g.SubjectType)
	}
	if !validCapabilityKind(g.Capability) {
		return fmt.Errorf("capability: unknown kind %q", g.Capability)
	}
	if !g.Effect.Valid() {
		return fmt.Errorf("effect: invalid %q", g.Effect)
	}
	g.Value = strings.TrimSpace(g.Value)
	if g.Value == "" {
		return fmt.Errorf("value: required")
	}
	if len(g.Value) > maxCapabilityGrantFieldLen || !controlCharFree(g.Value) {
		return fmt.Errorf("value: invalid")
	}
	// egress_host values are HOSTS, and they are matched by the proxy's own
	// entry semantics — so they get the proxy's own shape check, the same one
	// every allowed_domains ingest runs. Without it "*example.com" stored fine
	// and then covered "evilexample.com" (entryCoversAny is a bare suffix
	// test), and a mid-label or URL-shaped value stored as a row that can never
	// match — a deny that protects nothing. The "*" wildcard is this table's
	// own spelling for "every value of this kind", not a domain, so it is
	// exempt.
	if g.Capability == capEgressHost && g.Value != capWildcard {
		if err := proxy.ValidDomainEntry(g.Value); err != nil {
			return fmt.Errorf("value: %w", err)
		}
	}
	if g.SubjectType == types.CapabilitySubjectAll {
		// "all" names every signed-in human; the migration is explicit that
		// subject is '' for this type, so a caller-supplied value is dropped
		// rather than trusted (there is nothing for it to legitimately name).
		g.Subject = ""
		return nil
	}
	// user/group: lowercased the SAME way the caller's own identities are
	// (capabilitySubjects, sessionGroups) — a grant written "Alice@Corp.com"
	// must still hit the lowercased sub/email the resolver compares against.
	g.Subject = strings.ToLower(strings.TrimSpace(g.Subject))
	if g.Subject == "" {
		return fmt.Errorf("subject: required for subject_type %q", g.SubjectType)
	}
	if len(g.Subject) > maxCapabilityGrantFieldLen || !controlCharFree(g.Subject) {
		return fmt.Errorf("subject: invalid")
	}
	return nil
}

// handleUpsertCapabilityGrant creates or re-grants one row, keyed on the
// natural (subject_type, subject, capability, value). A conflict FLIPS the
// existing row's effect in place (store.UpsertCapabilityGrant) rather than
// leaving two contradictory rows — the response status tells the caller which
// happened: 201 for a genuinely new row, 200 when an existing one was
// updated (the console's DUPLICATE copy). operatorOnly (routes.go).
func (s *Server) handleUpsertCapabilityGrant(w http.ResponseWriter, r *http.Request) {
	var req grantWriteRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	g := types.CapabilityGrant{
		SubjectType: req.SubjectType,
		Subject:     req.Subject,
		Capability:  req.Capability,
		Value:       req.Value,
		Effect:      req.Effect,
	}
	if err := validateCapabilityGrant(&g); err != nil {
		writeError(w, http.StatusBadRequest, "invalid grant: "+err.Error())
		return
	}
	// A fresh candidate id: UpsertCapabilityGrant returns the EXISTING row's id
	// on a natural-key conflict, never this one — comparing the two is how the
	// handler tells created from updated without a separate existence read.
	g.ID = uuid.New()
	g.CreatedBy = principalFromRequest(r)
	saved, err := s.cfg.Store.UpsertCapabilityGrant(r.Context(), g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert capability grant: "+err.Error())
		return
	}
	action, status := "capability.grant.created", http.StatusCreated
	if saved.ID != g.ID {
		action, status = "capability.grant.updated", http.StatusOK
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, saved.ID.String(), "success", mustJSON(map[string]any{
			"subject_type": saved.SubjectType,
			"subject":      saved.Subject,
			"capability":   saved.Capability,
			"value":        saved.Value,
			"effect":       saved.Effect,
		})))
	writeJSON(w, status, saved)
}

// handleDeleteCapabilityGrant removes one grant by id. operatorOnly
// (routes.go) — there is no owning principal to scope this to, so a 404 on an
// unknown id is the whole story (no existence oracle to protect: an admin
// already sees the full table via GET /permissions).
func (s *Server) handleDeleteCapabilityGrant(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "capability grant")
	if !ok {
		return
	}
	if err := s.cfg.Store.DeleteCapabilityGrant(r.Context(), id); err != nil {
		if notFoundIf(w, err, "capability grant") {
			return
		}
		writeError(w, http.StatusInternalServerError, "delete capability grant: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"capability.grant.deleted", id.String(), "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// handlePutCapabilityEnforcement replaces the WHOLE per-kind switch map — the
// body is exactly the JSON shape GetCapabilityEnforcement returns, and
// PutCapabilityEnforcement's own doc explains why omitting a key is a real
// "turn it off": absent == not enforced everywhere else this state is read.
// Every key must be one of the four known kinds — validated here rather than
// left inert, matching the closed-kind write-boundary rule the grant handler
// above already applies. operatorOnly (routes.go); its own table, never
// SiteConfig, so a stale client round-tripping an older document can never
// silently disable this (see migration 0042's comment).
//
// If-Match (etag.go) is optional optimistic concurrency on top of this
// whole-map replace: an absent header behaves exactly as before, a present
// one that no longer matches GET /permissions's current Enforcement ETag is
// refused with 412 before the write reaches the store. capEnforcementMu
// (server.go) makes the check-then-write atomic against a second overlapping
// PUT on this process.
func (s *Server) handlePutCapabilityEnforcement(w http.ResponseWriter, r *http.Request) {
	var body map[string]bool
	if !decodeStrict(w, r, &body) {
		return
	}
	for kind := range body {
		if !validCapabilityKind(kind) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown capability kind %q", kind))
			return
		}
	}
	s.capEnforcementMu.Lock()
	defer s.capEnforcementMu.Unlock()
	existing, err := s.cfg.Store.GetCapabilityEnforcement(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get existing capability enforcement: "+err.Error())
		return
	}
	if !ifMatchSatisfied(r, computeETag(existing)) {
		writeError(w, http.StatusPreconditionFailed,
			"If-Match does not match the current capability enforcement map — GET /permissions again and retry")
		return
	}
	saved, err := s.cfg.Store.PutCapabilityEnforcement(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put capability enforcement: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"capability.enforcement.write", "capability_enforcement", "success", mustJSON(saved)))
	w.Header().Set("ETag", computeETag(saved))
	writeJSON(w, http.StatusOK, saved)
}

// meCapabilitiesResponse is GET /me/capabilities's body: what the CALLER
// personally holds, never the full admin table (that stays behind
// GET /permissions). Grants is exactly ListCapabilityGrantsFor(users, groups)
// — the same subject resolution capAllowed itself uses — so the console can
// answer "am I granted X" the identical way the server would, without a
// dedicated per-value probe endpoint.
type meCapabilitiesResponse struct {
	Grants              []types.CapabilityGrant `json:"grants"`
	Enforcement         map[string]bool         `json:"enforcement"`
	SessionGroups       []string                `json:"session_groups"`
	GroupsSnapshotStale bool                    `json:"groups_snapshot_stale"`
}

// handleMeCapabilities is the member-safe twin of GET /permissions: it sits on
// the plain /me block (classMember, not operatorOnly — see routes.go), and
// answers only for the caller's OWN subjects. GroupsSnapshotStale surfaces the
// nil-vs-empty distinction capabilitySubjects documents: a pre-0.6 cookie or a
// session with no group claim at all must read as "can't tell yet", not as
// silently holding no group grants.
func (s *Server) handleMeCapabilities(w http.ResponseWriter, r *http.Request) {
	users, groups, stale := capabilitySubjects(r.Context())
	grants, err := s.cfg.Store.ListCapabilityGrantsFor(r.Context(), users, groups)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list capability grants: "+err.Error())
		return
	}
	enf, err := s.cfg.Store.GetCapabilityEnforcement(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get capability enforcement: "+err.Error())
		return
	}
	// CreatedBy names the ADMIN who wrote the row (principal or email). A member
	// needs to know WHAT they hold, never which colleague signed it — and the
	// field is `omitempty`, so blanking it drops it from the body rather than
	// shipping an empty string. GET /permissions (operatorOnly) still carries it.
	for i := range grants {
		grants[i].CreatedBy = ""
	}
	writeJSON(w, http.StatusOK, meCapabilitiesResponse{
		Grants:              grants,
		Enforcement:         enf,
		SessionGroups:       groups,
		GroupsSnapshotStale: stale,
	})
}
