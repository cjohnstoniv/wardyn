// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The group template-manager grants (/admin/template-group-admins, SUPER at the
// router, which reads the effective role). A grant lets one person publish to
// one group's templates and reaches nothing else. It is stored only in the form
// TemplateGroupAdmin.Canonical returns, so a grant written as "DevTeam"
// authorises exactly the group the sign-in calls "devteam".

// maxTemplateGroupLabelRunes bounds a group's display label.
const maxTemplateGroupLabelRunes = 64

// templateGrantFromRequest decodes a grant body and makes it canonical. The
// person resolves as ?owner= does on the secrets routes (resolveSecretOwner):
// a name the identity directory knows becomes that principal, an unknown sub is
// kept as written, and an email that pairs with no principal, or with more than
// one, is refused. So every grant is keyed by the subject the person's revoke
// and erasure name, never by an email that someone else may later carry.
func (s *Server) templateGrantFromRequest(w http.ResponseWriter, r *http.Request, outcome string) (types.TemplateGroupAdmin, bool) {
	var g types.TemplateGroupAdmin
	if !decodeStrict(w, r, &g) {
		return g, false
	}
	owner, _, refusal, err := s.resolveSecretOwner(r.Context(), g.Person)
	if err != nil {
		writeServerError(w, r, "resolve the grant's person", err)
		return g, false
	}
	if refusal != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, ownerRefusalReason(refusal), templateGrantOwnerRefusalMsg(refusal, outcome))
		return g, false
	}
	g.Person = owner
	canon, ok := g.Canonical()
	if !ok {
		writeTemplateDiagnostics(w, *templateInvalid("group_id", "and person must name one group and one person"))
		return g, false
	}
	d := &templateDecoder{}
	if utf8.RuneCountInString(canon.Label) > maxTemplateGroupLabelRunes || !validTemplateText(canon.Label) {
		d.invalid("label", fmt.Sprintf("must be printable text of at most %d characters", maxTemplateGroupLabelRunes))
	} else {
		d.scanString("label", canon.Label)
	}
	if len(d.diags) > 0 {
		writeTemplateDiagnostics(w, d.diags...)
		return g, false
	}
	return canon, true
}

func (s *Server) handleListTemplateGroupAdmins(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.templateStoreOr501(w)
	if !ok {
		return
	}
	grants, err := ts.ListTemplateGroupAdmins(r.Context())
	if err != nil {
		writeServerError(w, r, "list template group admins", err)
		return
	}
	if grants == nil {
		grants = []types.TemplateGroupAdmin{}
	}
	writeJSON(w, http.StatusOK, client.TemplateGroupAdmins{Grants: grants})
}

// handlePutTemplateGroupAdmin grants (or re-grants) one person one group, and
// sets the group's label when the body carries one.
func (s *Server) handlePutTemplateGroupAdmin(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.templateStoreOr501(w)
	if !ok {
		return
	}
	g, ok := s.templateGrantFromRequest(w, r, templateGrantWritten)
	if !ok {
		return
	}
	g.GrantedBy = types.CanonicalUserSubject(runIdentitySubject(r.Context(), principalFromRequest(r)))
	stored, err := ts.PutTemplateGroupAdmin(r.Context(), g)
	if err != nil {
		writeServerError(w, r, "write template group admin", err)
		return
	}
	data := map[string]any{"group_id": stored.Group, "person": stored.Person}
	if g.Label != "" {
		data["label"] = stored.Label
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"template_group_admin.grant", stored.Group, "success", mustJSON(data)))
	writeJSON(w, http.StatusOK, stored)
}

// handleDeleteTemplateGroupAdmin revokes one grant. The person resolves as on
// the grant, so the revoke names the key the grant was stored under. Revoking
// one that is not there succeeds: removed is false in the audit row. A group write that began before
// the revoke finishes first; one after it is refused (the store's FOR SHARE).
func (s *Server) handleDeleteTemplateGroupAdmin(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.templateStoreOr501(w)
	if !ok {
		return
	}
	g, ok := s.templateGrantFromRequest(w, r, templateGrantRemoved)
	if !ok {
		return
	}
	removed, err := ts.DeleteTemplateGroupAdmin(r.Context(), g.Group, g.Person)
	if err != nil {
		writeServerError(w, r, "revoke template group admin", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"template_group_admin.revoke", g.Group, "success", mustJSON(map[string]any{"group_id": g.Group, "person": g.Person, "removed": removed})))
	w.WriteHeader(http.StatusNoContent)
}
