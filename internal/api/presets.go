// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Launch presets (#1143, migration 0087): an admin-managed, named, versioned
// bundle of existing POST /runs fields that a launcher names instead of
// sending the whole spec. The server expands a preset into the equivalent
// explicit request BEFORE any create-path gate runs, so a preset carries no
// capability of its own: the caller's ceiling, capabilities, owner-scoped
// secrets and drive apply exactly as they would to the explicit body.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

type presetRequest = client.PresetRequest

const maxPresetDescriptionLen = 1000

// presetNameRE: lowercase letters and digits joined by single hyphens, the
// shape a launcher can put in a URL path and a CI file without quoting.
var presetNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const maxPresetNameLen = 63

// presetLaunchFields are the create-run fields a caller may set alongside
// `preset` — the per-launch half (title, task) and the preset reference
// itself. A stored preset may carry none of them.
var presetLaunchFields = map[string]bool{"title": true, "task": true, "preset": true, "preset_version": true}

// mountPresetRoutes registers /presets. Reads are open to any signed-in
// person, narrowed to the presets their user type may launch; writes are
// operatorOnly for the stored-policy reason (routes.go): a preset is
// selectable CONTENT.
func (s *Server) mountPresetRoutes(r, operatorOnly chi.Router) {
	r.Get("/presets", s.handleListPresets)
	r.Get("/presets/{name}", s.handleGetPreset)
	operatorOnly.Put("/presets/{name}", s.handlePutPreset)
	operatorOnly.Delete("/presets/{name}", s.handleDeletePreset)
}

// setRunFields names, by JSON key, every field of req that is not its zero
// value.
func setRunFields(req createRunRequest) []string {
	v := reflect.ValueOf(req)
	var out []string
	for i := range v.NumField() {
		if !v.Field(i).IsZero() {
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ",")
			out = append(out, name)
		}
	}
	return out
}

// presetOpenTo reports whether the caller may list and launch p: every
// preset for an admin, otherwise one open to every type or to theirs.
func (s *Server) presetOpenTo(r *http.Request, p types.LaunchPreset) bool {
	return s.isOperator(r.Context()) || len(p.UserTypes) == 0 || slices.Contains(p.UserTypes, runCreatorUserType(r.Context()))
}

// presetView decodes a stored row into its wire shape.
func presetView(p types.LaunchPreset) (client.Preset, error) {
	out := client.Preset{
		Name: p.Name, Version: p.Version, Description: p.Description, UserTypes: p.UserTypes,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, CreatedBy: p.CreatedBy, UpdatedBy: p.UpdatedBy,
	}
	if out.UserTypes == nil {
		out.UserTypes = []string{}
	}
	dec := json.NewDecoder(bytes.NewReader(p.Request))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out.Request); err != nil {
		return client.Preset{}, fmt.Errorf("stored preset %q: %w", p.Name, err)
	}
	return out, nil
}

func (s *Server) handleListPresets(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.ListLaunchPresets(r.Context())
	if err != nil {
		writeServerError(w, r, "list presets", err)
		return
	}
	doc := client.PresetsDocument{Presets: []client.Preset{}}
	for _, p := range list {
		if !s.presetOpenTo(r, p) {
			continue
		}
		v, err := presetView(p)
		if err != nil {
			writeServerError(w, r, "decode preset", err)
			return
		}
		doc.Presets = append(doc.Presets, v)
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleGetPreset answers 404 for a preset the caller's type may not launch,
// exactly as for one that does not exist: the list leaves it out, so the read
// by name must too.
func (s *Server) handleGetPreset(w http.ResponseWriter, r *http.Request) {
	p, err := s.cfg.Store.GetLaunchPreset(r.Context(), chi.URLParam(r, "name"))
	if errors.Is(err, store.ErrNotFound) || err == nil && !s.presetOpenTo(r, p) {
		writeError(w, http.StatusNotFound, "preset not found")
		return
	}
	if err != nil {
		writeServerError(w, r, "get preset", err)
		return
	}
	v, err := presetView(p)
	if err != nil {
		writeServerError(w, r, "decode preset", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// validatePresetRequest checks a preset write and writes its own 400. It
// checks what can be known without a caller: the name, the description, the
// user types named, and that the stored request is a well-formed create-run
// body carrying no per-launch field. Everything else is the create path's, at
// launch, under the launching caller.
func (s *Server) validatePresetRequest(w http.ResponseWriter, r *http.Request, name string, req presetRequest) bool {
	if len(name) > maxPresetNameLen || !presetNameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"preset name must be lowercase letters and digits joined by single hyphens, at most %d characters", maxPresetNameLen))
		return false
	}
	if utf8.RuneCountInString(req.Description) > maxPresetDescriptionLen || !runFieldCharsAllowed(req.Description, true) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"description must be at most %d characters, with no control characters", maxPresetDescriptionLen))
		return false
	}
	if bad := slices.DeleteFunc(setRunFields(req.Request), func(f string) bool { return !presetLaunchFields[f] }); len(bad) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"request.%s is set per launch, so a preset cannot carry it", bad[0]))
		return false
	}
	if msg := agentRequirementError(req.Request); msg != "" {
		writeError(w, http.StatusBadRequest, "request: "+msg)
		return false
	}
	if _, ok := parseConfinementClass(req.Request.ConfinementClass); !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("request: unknown confinement_class %q", req.Request.ConfinementClass))
		return false
	}
	if req.Request.InlinePolicy != nil {
		if err := validatePolicySpec(*req.Request.InlinePolicy); err != nil {
			writeError(w, http.StatusBadRequest, "request: invalid inline_policy: "+err.Error())
			return false
		}
	}
	if !s.validateRunTextFields(w, req.Request) {
		return false
	}
	if len(req.UserTypes) == 0 {
		return true
	}
	known, err := s.cfg.Store.ListUserTypes(r.Context())
	if err != nil {
		writeServerError(w, r, "list user types", err)
		return false
	}
	for _, t := range req.UserTypes {
		if !slices.ContainsFunc(known, func(k types.UserType) bool { return k.ID == t }) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("user type %q does not exist", t))
			return false
		}
	}
	return true
}

// handlePutPreset creates or replaces a preset by name. A body identical to
// the stored row writes nothing and keeps the version, so no audit row
// either; every write that lands is audited with the request it stored, which
// is the only record of what an older version (a run's preset_version)
// contained once the row has moved on.
func (s *Server) handlePutPreset(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req presetRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !s.validatePresetRequest(w, r, name, req) {
		return
	}
	raw, err := json.Marshal(req.Request)
	if err != nil {
		writeServerError(w, r, "encode preset", err)
		return
	}
	slices.Sort(req.UserTypes)
	saved, write, err := s.cfg.Store.PutLaunchPreset(r.Context(), types.LaunchPreset{
		Name: name, Description: req.Description, UserTypes: slices.Compact(req.UserTypes),
		Request: raw, CreatedBy: principalFromRequest(r),
	})
	if err != nil {
		writeServerError(w, r, "put preset", err)
		return
	}
	status := http.StatusOK
	if write != store.PresetUnchanged {
		action := "preset.update"
		if write == store.PresetCreated {
			action, status = "preset.create", http.StatusCreated
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			action, saved.Name, "success", mustJSON(map[string]any{
				"version": saved.Version, "user_types": saved.UserTypes, "request": saved.Request,
			})))
	}
	v, err := presetView(saved)
	if err != nil {
		writeServerError(w, r, "decode preset", err)
		return
	}
	writeJSON(w, status, v)
}

// handleDeletePreset removes a preset. Runs launched from it keep their
// preset and preset_version.
func (s *Server) handleDeletePreset(w http.ResponseWriter, r *http.Request) {
	p, err := s.cfg.Store.DeleteLaunchPreset(r.Context(), chi.URLParam(r, "name"))
	if notFoundIf(w, err, "preset") {
		return
	}
	if err != nil {
		writeServerError(w, r, "delete preset", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"preset.delete", p.Name, "success", mustJSON(map[string]any{"version": p.Version})))
	w.WriteHeader(http.StatusNoContent)
}

// decodeRunRequest is the create-run body decode both doors share (POST /runs
// and /runs/preflight): the strict decode, then the preset expansion, so every
// gate after it sees the explicit request a preset stands for and nothing
// else. Writes its own error and returns false once it has responded.
func (s *Server) decodeRunRequest(w http.ResponseWriter, r *http.Request, req *createRunRequest) bool {
	if !decodeStrict(w, r, req) {
		return false
	}
	return s.expandRunPreset(w, r, req)
}

// expandRunPreset replaces a preset launch with the stored request, keeping
// only the caller's per-launch title and task, and stamps the preset name and
// the version it expanded. A preset the caller's type may not launch answers
// the same 422 as an unknown one.
func (s *Server) expandRunPreset(w http.ResponseWriter, r *http.Request, req *createRunRequest) bool {
	if req.Preset == "" {
		if req.PresetVersion != 0 {
			writeErrorReason(w, http.StatusBadRequest, reasonPresetVersionNoName, "preset_version needs preset")
			return false
		}
		return true
	}
	if extra := slices.DeleteFunc(setRunFields(*req), func(f string) bool { return presetLaunchFields[f] }); len(extra) > 0 {
		writeErrorReason(w, http.StatusBadRequest, reasonPresetField, fmt.Sprintf(
			"%s cannot be combined with a preset: a preset launch sets only title and task", extra[0]))
		return false
	}
	p, err := s.cfg.Store.GetLaunchPreset(r.Context(), req.Preset)
	if errors.Is(err, store.ErrNotFound) || err == nil && !s.presetOpenTo(r, p) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPresetUnknown, fmt.Sprintf(
			"preset %q does not exist or is not open to you", req.Preset))
		return false
	}
	if err != nil {
		writeServerError(w, r, "get preset", err)
		return false
	}
	if req.PresetVersion != 0 && req.PresetVersion != p.Version {
		writeErrorReason(w, http.StatusConflict, reasonPresetVersionMoved, fmt.Sprintf(
			"preset %q is at version %d, not %d", p.Name, p.Version, req.PresetVersion))
		return false
	}
	v, err := presetView(p)
	if err != nil {
		writeServerError(w, r, "decode preset", err)
		return false
	}
	title, task := req.Title, req.Task
	*req = v.Request
	req.Title, req.Task = title, task
	req.Preset, req.PresetVersion = p.Name, p.Version
	return true
}
