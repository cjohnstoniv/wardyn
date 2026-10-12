// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The template writes: create, update, delete and copy. Each one decides the
// caller's authority with types.TemplateAuthorize before it reads the
// document, holds the document to the one door (validateTemplateDocument) for
// the scope it lands in, and stores what the door returned. A group write's
// grant is re-read inside the store's own transaction.

// targetOwner is the catalogue key a save names: the caller themself for
// person scope, the organisation, or one group by its canonical subject.
func (c templateCaller) targetOwner(scope types.TemplateScope, group string) (types.TemplateOwner, *client.TemplateDiagnostic) {
	owner := types.TemplateOwner{Scope: scope}
	switch scope {
	case types.TemplateScopePerson:
		owner.Person = c.actor.Person
	case types.TemplateScopeGroup:
		canon, ok := types.CanonicalGroupSubject(group)
		if !ok {
			return owner, templateInvalid("group_id", "must name one group")
		}
		owner.Group = canon
		return owner, nil
	case types.TemplateScopeOrg:
	default:
		return owner, templateInvalid("scope", "must be person, org or group")
	}
	if group != "" {
		return owner, templateInvalid("group_id", "is only for group scope")
	}
	return owner, nil
}

func templateInvalid(path, detail string) *client.TemplateDiagnostic {
	return &client.TemplateDiagnostic{Path: path, Reason: reasonTemplateFieldInvalid, Message: templateFieldInvalidMsg(path, detail)}
}

// writeTemplateDiagnostics answers a refused document: the first diagnostic's
// status, reason and sentence, and every diagnostic.
func writeTemplateDiagnostics(w http.ResponseWriter, diags ...client.TemplateDiagnostic) {
	writeJSON(w, templateReasonStatus(diags[0].Reason), client.TemplateSaveRefusal{Error: diags[0].Message, Reason: diags[0].Reason, Diagnostics: diags})
}

// templateForSave holds a save to the door for owner and kind, and returns the
// row to store: the door's normalised document, and the name and description
// the request gives or, left out, the document's.
func templateForSave(req client.TemplateSaveRequest, owner types.TemplateOwner, kind types.TemplateKind) (types.RunTemplate, []client.TemplateDiagnostic) {
	doc, diags := validateTemplateDocument(req.Document, owner)
	if len(diags) > 0 {
		return types.RunTemplate{}, diags
	}
	if kind == types.TemplateKindPolicy {
		if diags := templatePolicyKindRefusals(doc); len(diags) > 0 {
			return types.RunTemplate{}, diags
		}
	}
	d := &templateDecoder{}
	text := func(path, given, inDoc string, max int) string {
		switch {
		case given == "":
			given = inDoc
		case inDoc != "" && inDoc != given:
			d.invalid(path, "differs from the document's "+path+": give one, or the same")
		}
		if utf8.RuneCountInString(given) > max || !validTemplateText(given) {
			d.invalid(path, fmt.Sprintf("must be printable text of at most %d characters", max))
		} else {
			d.scanString(path, given)
		}
		return given
	}
	name := text("name", req.Name, doc.Name, maxTemplateNameRunes)
	desc := text("description", req.Description, doc.Description, maxTemplateDescriptionRune)
	if strings.TrimSpace(name) == "" {
		d.invalid("name", "is required")
	}
	if len(d.diags) > 0 {
		return types.RunTemplate{}, d.diags
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return types.RunTemplate{}, []client.TemplateDiagnostic{{Reason: reasonTemplateDocumentInvalid, Message: templateDocumentInvalidMsg("it cannot be written as JSON")}}
	}
	return types.RunTemplate{Owner: owner, Kind: kind, Name: name, Description: desc, Document: body}, nil
}

// writeTemplateStoreErr answers a store refusal of a write.
func writeTemplateStoreErr(w http.ResponseWriter, r *http.Request, err error) {
	var limit *store.TemplateLimitError
	var conflict *store.TemplateRevisionConflictError
	switch {
	case errors.Is(err, store.ErrTemplateNameTaken):
		writeErrorReason(w, http.StatusConflict, reasonTemplateNameTaken, templateNameTakenMsg())
	case errors.As(err, &limit):
		writeErrorReason(w, http.StatusConflict, reasonTemplateLimitReached, templateLimitReachedMsg(limit.Limit))
	case errors.As(err, &conflict):
		writeErrorReason(w, http.StatusConflict, reasonTemplateRevisionConflict, templateRevisionConflictMsg(conflict.Current))
	case errors.Is(err, store.ErrTemplateGrantGone):
		writeErrorReason(w, http.StatusForbidden, reasonTemplateScopeForbidden, templateScopeForbiddenMsg(string(types.TemplateScopeGroup)))
	case errors.Is(err, store.ErrNotFound):
		writeTemplateNotFound(w)
	default:
		writeServerError(w, r, "write template", err)
	}
}

// refuseTemplateWrite answers a write the caller may not make to owner.
func (s *Server) refuseTemplateWrite(w http.ResponseWriter, r *http.Request, c templateCaller, owner types.TemplateOwner) bool {
	d := types.TemplateAuthorize(c.actor, owner, types.TemplateWrite)
	return templateDenialRefusal(d, types.TemplateWrite, owner).write(s, w, r)
}

func (s *Server) auditTemplate(r *http.Request, action string, t types.RunTemplate, data map[string]any) {
	data["scope"], data["kind"], data["revision"] = t.Owner.Scope, t.Kind, t.Revision
	if t.Owner.Group != "" {
		data["group_id"] = t.Owner.Group
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, t.ID.String(), "success", mustJSON(data)))
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	ts, c, _, ok := s.templateRoute(w, r, false)
	if !ok {
		return
	}
	var req client.TemplateSaveRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	owner, diag := c.targetOwner(req.Scope, req.GroupID)
	if diag != nil {
		writeTemplateDiagnostics(w, *diag)
		return
	}
	if s.refuseTemplateWrite(w, r, c, owner) {
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = types.TemplateKindTemplate
	}
	if !kind.Valid() {
		writeTemplateDiagnostics(w, *templateInvalid("kind", "must be template or policy"))
		return
	}
	t, diags := templateForSave(req, owner, kind)
	if len(diags) > 0 {
		writeTemplateDiagnostics(w, diags...)
		return
	}
	s.createTemplate(w, r, ts, c, t, map[string]any{})
}

// createTemplate stores t as a new template the caller saved, audits it with
// data, and answers it.
func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request, ts store.TemplateStore, c templateCaller, t types.RunTemplate, data map[string]any) {
	t.ID, t.CreatedBy = uuid.New(), c.actor.Person
	created, err := ts.CreateTemplate(r.Context(), t, c.grantCheck(t.Owner))
	if err != nil {
		writeTemplateStoreErr(w, r, err)
		return
	}
	var doc client.TemplateDocument
	_ = json.Unmarshal(created.Document, &doc)
	data["includes"] = templateIncludes(doc)
	s.auditTemplate(r, "template.create", created, data)
	s.writeTemplate(w, r, ts, c, http.StatusCreated, created)
}

// handleUpdateTemplate saves a new revision. expected_revision is required, and
// a template that moved on answers template_revision_conflict and keeps
// nothing. Scope and kind never change: a template moves to another audience
// by copy.
func (s *Server) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	ts, c, id, ok := s.templateRoute(w, r, true)
	if !ok {
		return
	}
	var req client.TemplateSaveRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	cur, ok := readableTemplate(w, r, ts, c, id, 0, true)
	if !ok || s.refuseTemplateWrite(w, r, c, cur.Owner) {
		return
	}
	var diag *client.TemplateDiagnostic
	if req.ExpectedRevision == nil {
		diag = templateInvalid("expected_revision", "is required: the revision you read")
	} else if req.Scope != "" {
		if owner, d := c.targetOwner(req.Scope, req.GroupID); d != nil || owner.Scope != cur.Owner.Scope || owner.Group != cur.Owner.Group {
			diag = templateInvalid("scope", "cannot change: copy the template to publish it elsewhere")
		}
	}
	if diag == nil && req.Kind != "" && req.Kind != cur.Kind {
		diag = templateInvalid("kind", "cannot change: save a new template instead")
	}
	if diag != nil {
		writeTemplateDiagnostics(w, *diag)
		return
	}
	t, diags := templateForSave(req, cur.Owner, cur.Kind)
	if len(diags) > 0 {
		writeTemplateDiagnostics(w, diags...)
		return
	}
	t.ID, t.UpdatedBy = id, c.actor.Person
	updated, err := ts.UpdateTemplate(r.Context(), t, *req.ExpectedRevision, c.grantCheck(cur.Owner))
	if err != nil {
		writeTemplateStoreErr(w, r, err)
		return
	}
	s.auditTemplate(r, "template.update", updated, map[string]any{"renamed": updated.Name != cur.Name})
	s.writeTemplate(w, r, ts, c, http.StatusOK, updated)
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	ts, c, id, ok := s.templateRoute(w, r, true)
	if !ok {
		return
	}
	cur, ok := readableTemplate(w, r, ts, c, id, 0, true)
	if !ok || s.refuseTemplateWrite(w, r, c, cur.Owner) {
		return
	}
	deleted, err := ts.DeleteTemplate(r.Context(), id, c.grantCheck(cur.Owner))
	if err != nil {
		writeTemplateStoreErr(w, r, err)
		return
	}
	s.auditTemplate(r, "template.delete", deleted, map[string]any{})
	w.WriteHeader(http.StatusNoContent)
}

// handleCopyTemplate copies the current revision into another audience: the
// caller reads the source and writes the target, and the document passes the
// door again for the target, so a field only one person has never reaches a
// shared audience. Publishing to the organisation is this route, in the Admin
// view.
func (s *Server) handleCopyTemplate(w http.ResponseWriter, r *http.Request) {
	ts, c, id, ok := s.templateRoute(w, r, true)
	if !ok {
		return
	}
	var req client.TemplateCopyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	src, ok := readableTemplate(w, r, ts, c, id, 0, false)
	if !ok {
		return
	}
	to, diag := c.targetOwner(req.Scope, req.GroupID)
	if diag != nil {
		writeTemplateDiagnostics(w, *diag)
		return
	}
	d := types.TemplateCopyAuthorize(c.actor, src.Owner, to)
	if templateDenialRefusal(d, types.TemplateWrite, to).write(s, w, r) {
		return
	}
	var doc client.TemplateDocument
	if err := json.Unmarshal(src.Document, &doc); err != nil {
		writeServerError(w, r, "read template", err)
		return
	}
	name := req.Name
	if name == "" {
		name = src.Name
	}
	if doc.Name != "" {
		doc.Name = name
	}
	t, diags := templateForSave(client.TemplateSaveRequest{Name: name, Description: src.Description, Document: doc}, to, src.Kind)
	if len(diags) > 0 {
		writeTemplateDiagnostics(w, diags...)
		return
	}
	s.createTemplate(w, r, ts, c, t, map[string]any{"from_id": src.ID.String()})
}
