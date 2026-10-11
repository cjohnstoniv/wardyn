// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// The run template routes (0.9). They are mounted behind their final router
// tiers so routeMatrix (authz_test.go) knows them before any storage exists,
// and every one answers 501 templates_unavailable until the template store
// lands. The handler of each is the one place the storage lane writes:
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
// "the caller owns it" is one arm of the check and not the whole of it. The
// storage lane therefore probes foreign ids by hand (every {id} route, every
// scope, every caller) and answers a template the caller cannot see with the
// 404 a missing one gets.
func (s *Server) mountTemplateRoutes(r, operatorOnly chi.Router) {
	r.Get("/templates", s.handleTemplatesUnavailable)
	r.Post("/templates", s.handleTemplatesUnavailable)
	r.Post("/templates/import", s.handleTemplatesUnavailable)
	r.Get("/templates/{id}", s.handleTemplatesUnavailable)
	r.Get("/templates/{id}/revisions/{revision}", s.handleTemplatesUnavailable)
	r.Put("/templates/{id}", s.handleTemplatesUnavailable)
	r.Delete("/templates/{id}", s.handleTemplatesUnavailable)
	r.Post("/templates/{id}/copy", s.handleTemplatesUnavailable)
	operatorOnly.Get("/admin/template-group-admins", s.handleTemplatesUnavailable)
	operatorOnly.Put("/admin/template-group-admins", s.handleTemplatesUnavailable)
	operatorOnly.Delete("/admin/template-group-admins", s.handleTemplatesUnavailable)
}

func (s *Server) handleTemplatesUnavailable(w http.ResponseWriter, _ *http.Request) {
	writeErrorReason(w, http.StatusNotImplemented, reasonTemplatesUnavailable, templatesUnavailableMsg())
}
