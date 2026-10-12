// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The template reads: the list (with where the caller may publish), one
// template, and one revision of it. Every row is read-authorised with
// types.TemplateAuthorize, and a template the caller cannot read answers the
// same bytes a missing one does.

// templateCaller is the verified caller of a template route: the actor
// TemplateAuthorize decides on, and every subject a grant may name them by.
type templateCaller struct {
	actor   types.TemplateActor
	persons []string
}

// templateStoreOr501 is the store's template seam, or the 501 a server without
// one answers.
func (s *Server) templateStoreOr501(w http.ResponseWriter) (store.TemplateStore, bool) {
	ts, ok := s.cfg.Store.(store.TemplateStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonTemplatesUnavailable, templatesUnavailableMsg())
	}
	return ts, ok
}

// templateCallerOf builds the caller from the request. OrgAdmin is the
// EFFECTIVE role: an administrator looking through the User view is a member
// here, so organisation publishing happens in the Admin view only. Person is
// the run-identity subject (a local-mode header names attribution, never whose
// templates these are). The group snapshot is the sign-in's, canonical; a
// missing or truncated one leaves every group answer unverified.
func (s *Server) templateCallerOf(ctx context.Context, r *http.Request, ts store.TemplateStore) (templateCaller, error) {
	users, groups, stale := capabilitySubjects(ctx)
	person := types.CanonicalUserSubject(runIdentitySubject(ctx, principalFromRequest(r)))
	c := templateCaller{
		actor:   types.TemplateActor{Person: person, OrgAdmin: s.isOperator(ctx), GroupsTruncated: stale},
		persons: []string{person},
	}
	for _, u := range users {
		if !slices.Contains(c.persons, u) {
			c.persons = append(c.persons, u)
		}
	}
	if groups != nil {
		c.actor.Groups = []string{}
		for _, g := range groups {
			if canon, ok := types.CanonicalGroupSubject(g); ok {
				c.actor.Groups = append(c.actor.Groups, canon)
			}
		}
	}
	grants, err := ts.TemplateGroupAdminGroups(ctx, c.persons)
	c.actor.GroupAdminGrants = grants
	return c, err
}

// grantCheck is the grant a write to owner must re-read inside its own
// transaction: one for group scope, none otherwise.
func (c templateCaller) grantCheck(owner types.TemplateOwner) *store.TemplateGrantCheck {
	if owner.Scope != types.TemplateScopeGroup {
		return nil
	}
	return &store.TemplateGrantCheck{Group: owner.Group, Persons: c.persons}
}

// publish is where the caller may save: their own scope always, the
// organisation as an effective organisation administrator, and each group they
// are a verified member of AND hold the grant for. Never a group from an
// unverifiable snapshot.
func (c templateCaller) publish() client.TemplatePublish {
	p := client.TemplatePublish{Org: c.actor.OrgAdmin, Groups: []string{}}
	if c.actor.Groups == nil || c.actor.GroupsTruncated {
		p.GroupsUnverified = true
		return p
	}
	for _, g := range c.actor.Groups {
		if slices.Contains(c.actor.GroupAdminGrants, g) && !slices.Contains(p.Groups, g) {
			p.Groups = append(p.Groups, g)
		}
	}
	slices.Sort(p.Groups)
	return p
}

func writeTemplateNotFound(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusNotFound, reasonTemplateNotFound, templateNotFoundMsg())
}

// templateRoute opens a template route: the store, the caller, and the id
// when the route names one.
func (s *Server) templateRoute(w http.ResponseWriter, r *http.Request, withID bool) (store.TemplateStore, templateCaller, uuid.UUID, bool) {
	ts, ok := s.templateStoreOr501(w)
	if !ok {
		return nil, templateCaller{}, uuid.Nil, false
	}
	var id uuid.UUID
	if withID {
		if id, ok = parseIDParam(w, r, "id", "template"); !ok {
			return nil, templateCaller{}, uuid.Nil, false
		}
	}
	c, err := s.templateCallerOf(r.Context(), r, ts)
	if err != nil {
		writeServerError(w, r, "read template grants", err)
		return nil, templateCaller{}, uuid.Nil, false
	}
	return ts, c, id, true
}

// readableTemplate loads id (revision 0 = current) and answers the 404 a
// missing template gets when the caller may not read it. The one exception is a
// write (forWrite: PUT, DELETE) to a group template that the caller's missing or
// truncated group snapshot cannot answer: that is 403 group_unverified, as
// templateDenialRefusal says, since the caller already holds the id and only
// signing in again helps.
func readableTemplate(w http.ResponseWriter, r *http.Request, ts store.TemplateStore, c templateCaller, id uuid.UUID, revision int, forWrite bool) (types.RunTemplate, bool) {
	var t types.RunTemplate
	var err error
	if revision > 0 {
		t, err = ts.GetTemplateRevision(r.Context(), id, revision)
	} else {
		t, err = ts.GetTemplate(r.Context(), id)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeTemplateNotFound(w)
		return t, false
	case err != nil:
		writeServerError(w, r, "read template", err)
		return t, false
	}
	switch d := types.TemplateAuthorize(c.actor, t.Owner, types.TemplateRead); {
	case d == types.TemplateDenyGroupUnverified && forWrite:
		writeErrorReason(w, http.StatusForbidden, reasonTemplateGroupUnverified, templateGroupUnverifiedMsg())
		return t, false
	case d != types.TemplateAllowed:
		writeTemplateNotFound(w)
		return t, false
	}
	return t, true
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	ts, c, _, ok := s.templateRoute(w, r, false)
	if !ok {
		return
	}
	rows, err := ts.ListTemplatesVisible(r.Context(), c.actor.Person, c.actor.Groups)
	if err != nil {
		writeServerError(w, r, "list templates", err)
		return
	}
	out := client.TemplateList{Templates: []client.TemplateSummary{}, Publish: c.publish()}
	labelled := slices.Clone(out.Publish.Groups)
	for _, t := range rows {
		if types.TemplateAuthorize(c.actor, t.Owner, types.TemplateRead) != types.TemplateAllowed {
			continue
		}
		sum, err := templateSummary(c, t)
		if err != nil {
			writeServerError(w, r, "read template", err)
			return
		}
		out.Templates = append(out.Templates, sum)
		if t.Owner.Group != "" {
			labelled = append(labelled, t.Owner.Group)
		}
	}
	labels, err := ts.TemplateGroupLabels(r.Context(), labelled)
	if err != nil {
		writeServerError(w, r, "read template group labels", err)
		return
	}
	for i := range out.Templates {
		out.Templates[i].GroupLabel = labels[out.Templates[i].GroupID]
	}
	for _, g := range out.Publish.Groups {
		if l := labels[g]; l != "" {
			if out.Publish.GroupLabels == nil {
				out.Publish.GroupLabels = map[string]string{}
			}
			out.Publish.GroupLabels[g] = l
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	s.getTemplate(w, r, 0)
}

// handleGetTemplateRevision answers a revision that is not a positive number
// as the missing template it names.
func (s *Server) handleGetTemplateRevision(w http.ResponseWriter, r *http.Request) {
	rev, err := strconv.Atoi(chi.URLParam(r, "revision"))
	if err != nil || rev < 1 {
		if _, ok := s.templateStoreOr501(w); ok {
			writeTemplateNotFound(w)
		}
		return
	}
	s.getTemplate(w, r, rev)
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request, revision int) {
	ts, c, id, ok := s.templateRoute(w, r, true)
	if !ok {
		return
	}
	t, ok := readableTemplate(w, r, ts, c, id, revision, false)
	if !ok {
		return
	}
	s.writeTemplate(w, r, ts, c, http.StatusOK, t)
}

// writeTemplate answers one template, with its group's label.
func (s *Server) writeTemplate(w http.ResponseWriter, r *http.Request, ts store.TemplateStore, c templateCaller, status int, t types.RunTemplate) {
	out, err := templateView(c, t)
	if err == nil && t.Owner.Group != "" {
		var labels map[string]string
		labels, err = ts.TemplateGroupLabels(r.Context(), []string{t.Owner.Group})
		out.GroupLabel = labels[t.Owner.Group]
	}
	if err != nil {
		writeServerError(w, r, "read template", err)
		return
	}
	writeJSON(w, status, out)
}

// templateView is one stored revision as the caller sees it. Authorship is
// shown only to a caller who may write the template.
func templateView(c templateCaller, t types.RunTemplate) (client.Template, error) {
	var doc client.TemplateDocument
	if err := json.Unmarshal(t.Document, &doc); err != nil {
		return client.Template{}, err
	}
	out := client.Template{
		ID: t.ID, Scope: t.Owner.Scope, OwnerID: t.Owner.Person, GroupID: t.Owner.Group, Kind: t.Kind,
		Name: t.Name, Description: t.Description, Revision: t.Revision, Coverage: doc.Coverage, Document: doc,
		Writable:  types.TemplateAuthorize(c.actor, t.Owner, types.TemplateWrite) == types.TemplateAllowed,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	if out.Writable {
		out.CreatedBy, out.UpdatedBy = t.CreatedBy, t.UpdatedBy
	}
	return out, nil
}

func templateSummary(c templateCaller, t types.RunTemplate) (client.TemplateSummary, error) {
	v, err := templateView(c, t)
	if err != nil {
		return client.TemplateSummary{}, err
	}
	sum := client.TemplateSummary{
		ID: v.ID, Scope: v.Scope, GroupID: v.GroupID, Kind: v.Kind, Name: v.Name, Description: v.Description,
		Revision: v.Revision, Coverage: v.Coverage, Includes: templateIncludes(v.Document),
		Fields: templateFieldNames(v.Document), NeedsSetup: []string{}, Writable: v.Writable, UpdatedAt: v.UpdatedAt,
	}
	if sum.Includes == nil {
		sum.Includes = []client.TemplatePart{}
	}
	for _, n := range v.Document.NeedsSetup {
		sum.NeedsSetup = append(sum.NeedsSetup, n.Field)
	}
	return sum, nil
}

// templateFieldNames lists the intent fields a document specifies, with
// inline_policy expanded to inline_policy.<key>, sorted.
func templateFieldNames(doc client.TemplateDocument) []string {
	names := []string{}
	for _, name := range doc.Intent.Names() {
		if name != "inline_policy" {
			names = append(names, name)
			continue
		}
		var spec map[string]json.RawMessage
		_ = json.Unmarshal(doc.Intent.Raw(name), &spec)
		for key := range spec {
			names = append(names, "inline_policy."+key)
		}
	}
	slices.SortFunc(names, strings.Compare)
	return names
}
