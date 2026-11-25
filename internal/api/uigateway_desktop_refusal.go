// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/google/uuid"
)

// uiRefusalCapture holds one door's answer, so a desktop's entry can show a refusal as the viewer's page. It keeps the
// first status, as http.ResponseWriter does.
type uiRefusalCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *uiRefusalCapture) Header() http.Header { return c.header }

func (c *uiRefusalCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *uiRefusalCapture) Write(p []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.body.Write(p)
}

// uiDesktopDoor runs door, which answers on the writer it is given and reports whether it answered. For a desktop,
// door answers into a buffer: a 403 with a governance reason (uiDesktopDeniedReason) whose body names its sentence becomes the viewer's refusal page with that sentence
// (serveDesktopDenied), and any other answer, or a 403 with no viewer to serve, is written as door wrote it. The
// door writes its own audit row before it answers, so the row is the same either way. Any other app runs door on w.
func (s *Server) uiDesktopDoor(w http.ResponseWriter, r *http.Request, runID uuid.UUID, desktop bool, door func(http.ResponseWriter) bool) bool {
	if !desktop {
		return door(w)
	}
	c := &uiRefusalCapture{header: http.Header{}}
	answered := door(c)
	if c.status == 0 {
		return answered
	}
	var body errorBody
	if c.status == http.StatusForbidden && json.Unmarshal(c.body.Bytes(), &body) == nil && body.Error != "" &&
		uiDesktopDeniedReason(body.Reason) && s.serveDesktopDenied(w, r, runID, body.Error) {
		return true
	}
	maps.Copy(w.Header(), c.header)
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body.Bytes())
	return true
}

// uiDesktopDeniedReason is true for the reasons a governance profile refuses a desktop with. Any other 403 keeps the
// door's JSON: a support grant that does not open the desktop (refuseDesktopKindApp), or a data_out rule.
func uiDesktopDeniedReason(reason string) bool {
	return reason == string(authz.ReasonGovernanceUnit) || reason == string(authz.ReasonGovernanceOverlayUnsatisfiable)
}

// serveDesktopDenied is serveDesktopRefusal for a desktop that a door refused (its governance profile):
// the same page, with the denied card and the door's own sentence; false when there is no viewer (no WARDYN_UI_DIR).
func (s *Server) serveDesktopDenied(w http.ResponseWriter, r *http.Request, runID uuid.UUID, said string) bool {
	return s.serveDesktopViewer(w, r, runID, map[string]any{"state": "refused", "said": said, "cause": "denied"}, http.StatusForbidden)
}
