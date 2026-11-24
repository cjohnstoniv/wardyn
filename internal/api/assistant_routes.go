// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/go-chi/chi/v5"

// mountAssistantRoutes registers the assistant's routes on the authenticated group. The settings are the
// super admin's; the availability read is any signed-in person's.
func (s *Server) mountAssistantRoutes(r chi.Router) {
	operatorOnly := r.With(s.requireOperator)
	operatorOnly.Get("/assistant/settings", s.handleGetAssistantSettings)
	operatorOnly.Put("/assistant/settings", s.handlePutAssistantSettings)
	r.Get("/me/assistant", s.handleGetMyAssistant)
	// The sessions are the signed-in person's own: another person's session answers 404.
	r.Post("/assistant/preview", s.handleAssistantPreview)
	r.Post("/assistant/sessions", s.handleCreateAssistantSession)
	r.Post("/assistant/sessions/{id}/messages", s.handleAssistantMessage)
	r.Post("/assistant/sessions/{id}/edits", s.handleAssistantEdits)
	r.Get("/assistant/sessions/{id}/events", s.handleAssistantEvents)
	r.Delete("/assistant/sessions/{id}", s.handleDeleteAssistantSession)
	// The checks strip and the replay answer the signed-in person with no model, and write nothing.
	r.Post("/assistant/checks", s.handleAssistantChecks)
	r.Post("/assistant/replay", s.handleAssistantReplay)
}

// mountAssistantToolRoutes registers the run-token tool routes: the tool list and one tool call, each for the
// session the token's run id names. mountAssistantInternalRoutes calls it. Neither is ever relayed.
func (s *Server) mountAssistantToolRoutes(r chi.Router) {
	r.Get("/internal/assistant/tools", s.handleAssistantToolList)
	r.Post("/internal/assistant/tools/{name}", s.handleAssistantToolCall)
}
