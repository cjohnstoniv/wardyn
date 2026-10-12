// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/go-chi/chi/v5"
)

// The run template routes (0.9). A store with no template seam answers 501
// templates_unavailable on every one except POST /templates/import, which
// validates a document and stores nothing (template_import.go); the Postgres
// store has the seam.
//
//	every /templates route        member at the router; the handler then checks the
//	                              caller against the template's scope with
//	                              types.TemplateAuthorize (types.TemplateRoutes
//	                              records the per-scope authority of each route)
//	/admin/template-group-admins  admin (SUPER): it writes a grant that lets one
//	                              person publish to one group, and nothing else
//
// The {id} routes are classMember with handler-side scoping rather than
// classOwner: a template is owned by a person, an organisation or a group, so
// "the caller owns it" is one arm of the check and not the whole of it. A
// template the caller cannot see answers the 404 a missing one gets
// (readableTemplate), which TestTemplateForeignIDIsTheMissingID404 probes on
// every {id} route, every scope and every caller.
func (s *Server) mountTemplateRoutes(r, operatorOnly chi.Router) {
	r.Get("/templates", s.handleListTemplates)
	r.Post("/templates", s.handleCreateTemplate)
	r.Post("/templates/import", s.handleImportTemplate)
	r.Get("/templates/{id}", s.handleGetTemplate)
	r.Get("/templates/{id}/revisions/{revision}", s.handleGetTemplateRevision)
	r.Put("/templates/{id}", s.handleUpdateTemplate)
	r.Delete("/templates/{id}", s.handleDeleteTemplate)
	r.Post("/templates/{id}/copy", s.handleCopyTemplate)
	operatorOnly.Get("/admin/template-group-admins", s.handleListTemplateGroupAdmins)
	operatorOnly.Put("/admin/template-group-admins", s.handlePutTemplateGroupAdmin)
	operatorOnly.Delete("/admin/template-group-admins", s.handleDeleteTemplateGroupAdmin)
}
