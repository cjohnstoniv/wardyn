// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// PATCH /runs/{id}/title: the owner renames a run (#1197 L2, design.md
// §3.4). Deliberately its own route rather than a widened
// handleSetRunEndAndWait: that handler refuses a terminal run and admits a
// SUPER admin only (ownsRunOrSuperAdmin) — a rename is allowed in ANY run
// state and follows the wider owner-or-admin predicate every other run-detail
// read/write uses (getRunAuthorized), so folding it in would either narrow
// this route or widen that one.
type runTitleRequest struct {
	Title string `json:"title"`
}

func (s *Server) handleSetRunTitle(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	// Owner-or-admin (getRunAuthorized): a foreign run gets the byte-identical
	// 404 a truly-missing run would, same gate as GET /runs/{id}.
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	var req runTitleRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !validateRunTitle(w, req.Title) {
		return
	}
	titler, ok := s.cfg.Store.(store.RunTitler)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this store cannot rename a run")
		return
	}
	title := strings.TrimSpace(req.Title)
	if err := titler.SetRunTitle(r.Context(), id, title); err != nil {
		writeServerError(w, r, "set run title", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.title.set", run.ID.String(),
		"success", mustJSON(map[string]any{"from": run.Title, "to": title})))
	writeJSON(w, http.StatusOK, map[string]any{"id": run.ID, "title": title})
}

// validateRunTitle applies the SAME 200-character and control-character
// checks create-run's title field gets (runs_create_fields.go) — one trust
// boundary, not a second copy of it for the rename door.
func validateRunTitle(w http.ResponseWriter, title string) bool {
	if n := utf8.RuneCountInString(title); n > maxRunTitleLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(runFieldTooLongRefusal, "title", n, maxRunTitleLen))
		return false
	}
	if !runFieldCharsAllowed(title, false) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(runFieldControlCharRefusal, "title"))
		return false
	}
	return true
}
