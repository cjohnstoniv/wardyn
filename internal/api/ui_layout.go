// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-user run-detail widget layout. The run cockpit lets a user arrange the
// evidence widgets; that arrangement is theirs and has to survive a new
// machine, so it is a server row keyed by principal — ui/src/app/lib/storage.ts
// is localStorage and is explicitly NOT the answer here.
package api

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runLayoutPresets is the CLOSED set of cockpit presets a human may save a
// layout for: "live" (a run still executing) and "finished" (one that has
// stopped) genuinely want different arrangements — a live run's cockpit
// foregrounds the terminal/timeline, a finished one foregrounds
// audit/recording. Migration 0037_ui_layouts.sql places no CHECK on preset
// (this schema validates closed Go-side enums like ApprovalKind in
// application code, not SQL), so this slice is the ONLY enforcement — an
// open string here is an unbounded row-per-typo.
var runLayoutPresets = []string{"live", "finished"}

// runLayoutWidgetIDs is the CLOSED set of evidence widgets the run cockpit
// knows how to render. This is HALF of the contract with the console's own
// widget registry (ui/src/app/components/screens/run-detail/widgets/) and the
// two must be kept in sync by hand until a shared source of truth exists.
//
// Rejecting an id outside this set at WRITE time, rather than storing it, means
// a saved layout can never name a widget the current build cannot render — the
// alternative is a widget that silently vanishes on restore with nothing to
// explain why.
//
// NOT in this list, deliberately:
//   - "timeline" — the run timeline was DELETED from this screen by the
//     terminal-first redesign; the Audit tab owns the trail. Accepting the id
//     would let a client persist a layout slot for a widget that no longer
//     exists.
//   - "approvals"/"audit"/"recording" — those are TABS, not widgets on the
//     Overview canvas. A tab has no x/y/w/h to save.
var runLayoutWidgetIDs = []string{
	"terminal", "egress", "files", "sandbox", "credentials", "identity", "ssh",
}

// runLayoutMaxWidgets bounds the element count of a PUT layout. The known-
// widget-id set above is itself closed and small, so more entries than its
// length necessarily means a duplicate; this is a defensive cap against an
// unbounded/malformed body, not a limit the console is expected to approach.
var runLayoutMaxWidgets = len(runLayoutWidgetIDs)

// putRunLayoutRequest is the PUT /api/v1/me/run-layout body. There is
// deliberately no principal field — see the package doc and
// principalFromRequest's use below, the entire security surface of this
// endpoint.
type putRunLayoutRequest struct {
	Preset string                  `json:"preset"`
	Layout []types.RunLayoutWidget `json:"layout"`
}

// runLayoutResponse is the GET/PUT response body. UpdatedAt is a pointer so
// the GET default (nothing saved yet) omits it entirely rather than
// marshaling the zero time — its presence is how the console tells "never
// saved" apart from "saved a while ago".
type runLayoutResponse struct {
	Preset    string                  `json:"preset"`
	Layout    []types.RunLayoutWidget `json:"layout"`
	UpdatedAt *time.Time              `json:"updated_at,omitempty"`
}

// runLayoutEmptyResponse is the shared "nothing saved for this preset yet"
// shape both the no-row case and the no-RunLayoutStore degrade return —
// always 200, never an error (see handleGetRunLayout's doc).
func runLayoutEmptyResponse(preset string) runLayoutResponse {
	return runLayoutResponse{Preset: preset, Layout: []types.RunLayoutWidget{}}
}

// handleGetRunLayout serves GET /api/v1/me/run-layout?preset=live|finished.
//
// Scoped by principalFromRequest(r) ONLY — never a caller-supplied
// principal, the entire security surface of this endpoint (mirrors
// sshkeys.go's self-service posture). GET with no saved row is NOT an
// error: it returns the same empty/default shape as a store that cannot
// persist layouts at all (store.RunLayoutStore absent), so the console
// falls through to its own situational default either way instead of
// treating "I have never saved a layout" as a failure.
func (s *Server) handleGetRunLayout(w http.ResponseWriter, r *http.Request) {
	preset := r.URL.Query().Get("preset")
	if !slices.Contains(runLayoutPresets, preset) {
		writeError(w, http.StatusBadRequest, "preset must be one of: live, finished")
		return
	}

	ls, ok := s.cfg.Store.(store.RunLayoutStore)
	if !ok {
		writeJSON(w, http.StatusOK, runLayoutEmptyResponse(preset))
		return
	}

	layout, err := ls.GetRunLayout(r.Context(), principalFromRequest(r), preset)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, runLayoutEmptyResponse(preset))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get run layout: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runLayoutResponse{Preset: layout.Preset, Layout: layout.Layout, UpdatedAt: &layout.UpdatedAt})
}

// handlePutRunLayout serves PUT /api/v1/me/run-layout: save the caller's own
// layout for one preset (upsert — see store.PG.PutRunLayout). Strict-decoded
// (unknown fields rejected) like every other write in this package; preset
// and every widget id are validated against their closed sets before
// anything is stored, and the element count is bounded. 501s when the store
// cannot persist it (store.RunLayoutStore absent) rather than a silent 200
// that dropped the write on the floor — a caller should know the save never
// happened.
func (s *Server) handlePutRunLayout(w http.ResponseWriter, r *http.Request) {
	var req putRunLayoutRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !slices.Contains(runLayoutPresets, req.Preset) {
		writeError(w, http.StatusBadRequest, "preset must be one of: live, finished")
		return
	}
	if len(req.Layout) > runLayoutMaxWidgets {
		writeError(w, http.StatusBadRequest, "layout has too many widgets")
		return
	}
	for _, wgt := range req.Layout {
		if !slices.Contains(runLayoutWidgetIDs, wgt.Widget) {
			writeError(w, http.StatusBadRequest, "unknown widget id: "+wgt.Widget)
			return
		}
		if wgt.X < 0 || wgt.Y < 0 || wgt.W < 1 || wgt.H < 1 {
			writeError(w, http.StatusBadRequest, "widget "+wgt.Widget+" has an invalid position or size")
			return
		}
	}

	ls, ok := s.cfg.Store.(store.RunLayoutStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "layout persistence is not implemented on this build")
		return
	}

	saved, err := ls.PutRunLayout(r.Context(), principalFromRequest(r), req.Preset, req.Layout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save run layout: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runLayoutResponse{Preset: saved.Preset, Layout: saved.Layout, UpdatedAt: &saved.UpdatedAt})
}
