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
// §3.4, packet H-5 = A: "Rename on the run page (owner only)"). OWNER
// ONLY — unlike every other /runs/{id} route, an admin or a security_admin
// on a foreign run gets the byte-identical 404 a non-owner gets (ownsRun,
// helpers.go — no admin bypass at all). Deliberately its own route rather
// than a widened handleSetRunEndAndWait: that handler refuses a terminal run
// and admits a SUPER admin (ownsRunOrSuperAdmin) — a rename is allowed in ANY
// run state and follows the STRICTER owner-only predicate, so folding it in
// would either narrow that route's admin bypass or widen this one's.
type runTitleRequest struct {
	Title string `json:"title"`
}

func (s *Server) handleSetRunTitle(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	// Owner ONLY (ownsRun, via getRunAuthorizedBy) — deliberately NOT
	// getRunAuthorized's own default (owner-or-admin) predicate: a foreign
	// run gets the byte-identical 404 a truly-missing run would, the same
	// 404 shape GET /runs/{id} answers, just with no admin/security_admin
	// door through.
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRun)
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
