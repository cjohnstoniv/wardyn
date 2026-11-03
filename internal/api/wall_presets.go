// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Wall presets (types.WallPreset): named walls that only narrow, built in or kept for one person, a
// group or the organisation, and picked at launch. Their authority is the run templates', borrowed
// whole: types.TemplateAuthorize on the preset's Owner(), the caller templateCallerOf builds (one
// canonical owner subject), and the template group-admin grant (wallPresetCaller). A preset the caller
// cannot read answers exactly as a missing one does. The spec is held to validatePersonalWalls, the
// same door as a person's own walls.

const (
	auditWallPresetCreate = "wall_preset.create"
	auditWallPresetUpdate = "wall_preset.update"
	auditWallPresetDelete = "wall_preset.delete"
)

// mountWallPresetRoutes mounts the wall preset routes on the authenticated group. Each handler checks
// the caller against the preset's scope, as /templates does: an {id} is owned by a person, a group or
// the organisation, so "the caller owns it" is one arm of the check and not the whole of it.
func (s *Server) mountWallPresetRoutes(r chi.Router) {
	r.Get("/wall-presets", s.handleListWallPresets)
	r.Post("/wall-presets", s.handleCreateWallPreset)
	r.Get("/wall-presets/{id}", s.handleGetWallPreset)
	r.Put("/wall-presets/{id}", s.handleUpdateWallPreset)
	r.Delete("/wall-presets/{id}", s.handleDeleteWallPreset)
}

// wallPresetCaller is the one place wall presets take the template group-admin grant: the caller's
// GroupAdminGrants (who may save a group's presets, and where publish offers) and the grant a group
// write re-reads in its own transaction (grantCheck) are the templates'. A separate grant for wall
// presets changes this function: withdrawing the templates' grant from presets is clearing
// c.actor.GroupAdminGrants here, so no group write passes TemplateAuthorize and grantCheck is never
// reached.
func (s *Server) wallPresetCaller(ctx context.Context, r *http.Request, ps store.WallPresetStore) (templateCaller, error) {
	return s.templateCallerOf(ctx, r, ps)
}

// wallPresetRoute opens a wall preset route: the store (501 without one), for a write the person gate
// (400 walls_need_person: presets are saved by a signed-in person), and the caller.
func (s *Server) wallPresetRoute(w http.ResponseWriter, r *http.Request, write bool) (store.WallPresetStore, templateCaller, bool) {
	ps, ok := s.cfg.Store.(store.WallPresetStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonWallPresetsUnavailable, "Wall presets are not available on this server yet.")
		return nil, templateCaller{}, false
	}
	if write && wallsPerson(r.Context()) == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonWallsNeedPerson,
			"Wall presets are saved by a signed-in person. The admin token, local mode and a service identity save none.")
		return nil, templateCaller{}, false
	}
	c, err := s.wallPresetCaller(r.Context(), r, ps)
	if err != nil {
		writeServerError(w, r, "read template grants", err)
		return nil, templateCaller{}, false
	}
	return ps, c, true
}

// findWallPreset is the preset id names, a built-in by its name or a stored preset by its uuid, with
// actor's read decision on it (a built-in is read by anyone). An id that is neither is
// store.ErrNotFound. Every caller answers a denial as a missing preset, so an id is no existence
// oracle; only a write whose group snapshot cannot answer says to sign in again.
func findWallPreset(ctx context.Context, ps store.WallPresetStore, actor types.TemplateActor, id string) (types.WallPreset, types.TemplateDenial, error) {
	if p, ok := types.BuiltinWallPreset(id); ok {
		return p, types.TemplateAllowed, nil
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return types.WallPreset{}, types.TemplateAllowed, store.ErrNotFound
	}
	p, err := ps.GetWallPreset(ctx, uid)
	if err != nil {
		return types.WallPreset{}, types.TemplateAllowed, err
	}
	return p, types.TemplateAuthorize(actor, p.Owner(), types.TemplateRead), nil
}

func writeWallPresetNotFound(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusNotFound, reasonWallPresetNotFound, "That wall preset does not exist, or you cannot see it.")
}

func wallPresetInvalid(detail string) *runRefusal {
	return runError(http.StatusUnprocessableEntity, reasonWallPresetInvalid, "invalid wall preset: "+detail)
}

// wallPresetWriteRefusal answers a write the caller may not make to owner, naming who may.
func wallPresetWriteRefusal(d types.TemplateDenial, owner types.TemplateOwner) *runRefusal {
	msg := "A personal wall preset belongs to the person who owns it."
	switch {
	case d == types.TemplateAllowed:
		return nil
	case d == types.TemplateDenyGroupUnverified:
		msg = templateGroupUnverifiedMsg()
	case owner.Scope == types.TemplateScopeOrg:
		msg = "Only an organisation administrator can save wall presets for the organisation, from the Admin view."
	case owner.Scope == types.TemplateScopeGroup:
		msg = "Only the template managers of that group can save wall presets for it."
	}
	return runError(http.StatusForbidden, reasonWallPresetScopeForbidden, msg)
}

// wallPresetView is a preset as it is answered: who saved a person preset is never sent.
func wallPresetView(p types.WallPreset) types.WallPreset {
	if p.Scope == types.WallPresetScopePerson {
		p.UpdatedBy = ""
	}
	return p
}

func (s *Server) handleListWallPresets(w http.ResponseWriter, r *http.Request) {
	ps, c, ok := s.wallPresetRoute(w, r, false)
	if !ok {
		return
	}
	rows, err := ps.ListWallPresetsVisible(r.Context(), c.actor.Person, c.actor.Groups)
	if err != nil {
		writeServerError(w, r, "list wall presets", err)
		return
	}
	out := client.WallPresetList{Presets: types.BuiltinWallPresets(), Publish: c.publish()}
	labelled := slices.Clone(out.Publish.Groups)
	for _, p := range rows {
		if types.TemplateAuthorize(c.actor, p.Owner(), types.TemplateRead) == types.TemplateAllowed {
			out.Presets = append(out.Presets, wallPresetView(p))
			if p.GroupID != "" && !slices.Contains(labelled, p.GroupID) {
				labelled = append(labelled, p.GroupID)
			}
		}
	}
	// publish.group_labels labels the groups the caller may save to and those of the group presets
	// they can read, as the templates' list does; never a group outside both.
	labels, err := ps.TemplateGroupLabels(r.Context(), labelled)
	if err != nil {
		writeServerError(w, r, "read wall preset group labels", err)
		return
	}
	for _, g := range labelled {
		if l := labels[g]; l != "" {
			if out.Publish.GroupLabels == nil {
				out.Publish.GroupLabels = map[string]string{}
			}
			out.Publish.GroupLabels[g] = l
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetWallPreset(w http.ResponseWriter, r *http.Request) {
	ps, c, ok := s.wallPresetRoute(w, r, false)
	if !ok {
		return
	}
	p, d, err := findWallPreset(r.Context(), ps, c.actor, chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && d != types.TemplateAllowed:
		writeWallPresetNotFound(w)
	case err != nil:
		writeServerError(w, r, "read wall preset", err)
	default:
		writeJSON(w, http.StatusOK, wallPresetView(p))
	}
}

// writableWallPreset is the stored preset the {id} of a PUT or DELETE names, when the caller may
// write it: 403 wall_preset_builtin for a built-in, the missing 404 for one they cannot read, and
// 403 wall_preset_scope_forbidden for one they may read but not write.
func (s *Server) writableWallPreset(w http.ResponseWriter, r *http.Request, ps store.WallPresetStore, c templateCaller) (types.WallPreset, bool) {
	id := chi.URLParam(r, "id")
	if b, ok := types.BuiltinWallPreset(id); ok {
		writeErrorReason(w, http.StatusForbidden, reasonWallPresetBuiltin,
			fmt.Sprintf("%s is built in, so it cannot be changed or deleted. Save a copy of it as your own instead.", b.Name))
		return b, false
	}
	p, d, err := findWallPreset(r.Context(), ps, c.actor, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeWallPresetNotFound(w)
		return p, false
	case err != nil:
		writeServerError(w, r, "read wall preset", err)
		return p, false
	case d == types.TemplateDenyGroupUnverified:
		return p, !wallPresetWriteRefusal(d, p.Owner()).write(s, w, r)
	case d != types.TemplateAllowed:
		writeWallPresetNotFound(w)
		return p, false
	}
	return p, !wallPresetWriteRefusal(types.TemplateAuthorize(c.actor, p.Owner(), types.TemplateWrite), p.Owner()).write(s, w, r)
}

// wallPresetForSave holds a save to the walls door and returns the row to store for owner. A name is
// required, printable and bounded, and never a built-in's in any case; the spec must narrow something.
func wallPresetForSave(req client.WallPresetSaveRequest, owner types.TemplateOwner) (types.WallPreset, *runRefusal) {
	name := strings.TrimSpace(req.Name)
	switch {
	case name == "":
		return types.WallPreset{}, wallPresetInvalid("name is required")
	case utf8.RuneCountInString(name) > maxTemplateNameRunes || !validTemplateText(name):
		return types.WallPreset{}, wallPresetInvalid(fmt.Sprintf("name must be printable text of at most %d characters", maxTemplateNameRunes))
	case utf8.RuneCountInString(req.Description) > maxTemplateDescriptionRune || !validTemplateText(req.Description):
		return types.WallPreset{}, wallPresetInvalid(fmt.Sprintf("description must be printable text of at most %d characters", maxTemplateDescriptionRune))
	case types.IsBuiltinWallPresetName(name):
		return types.WallPreset{}, runError(http.StatusConflict, reasonWallPresetNameTaken,
			fmt.Sprintf("%s is the name of a built-in wall preset. Choose another name.", name))
	}
	if err := validatePersonalWalls(req.Spec); err != nil {
		return types.WallPreset{}, wallPresetInvalid(err.Error())
	}
	if req.Spec.Empty() {
		return types.WallPreset{}, wallPresetInvalid("the preset names no limit; Open is the built-in for no preset")
	}
	return types.WallPreset{Scope: types.WallPresetScope(owner.Scope), OwnerID: owner.Person, GroupID: owner.Group,
		Name: name, Description: req.Description, Spec: req.Spec}, nil
}

// writeWallPresetStoreErr answers a store refusal of a write.
func writeWallPresetStoreErr(w http.ResponseWriter, r *http.Request, err error) {
	var limit *store.TemplateLimitError
	var conflict *store.TemplateRevisionConflictError
	switch {
	case errors.Is(err, store.ErrTemplateNameTaken):
		writeErrorReason(w, http.StatusConflict, reasonWallPresetNameTaken, "Another wall preset available to the same people already uses this name.")
	case errors.As(err, &limit):
		writeErrorReason(w, http.StatusConflict, reasonWallPresetLimitReached,
			fmt.Sprintf("This audience already holds %d wall presets, the most it can. Delete one first.", limit.Limit))
	case errors.As(err, &conflict):
		writeErrorReason(w, http.StatusConflict, reasonWallPresetVersionConflict,
			fmt.Sprintf("Someone saved this wall preset since you opened it; it is at version %d. Reload it, then apply your edit again.", conflict.Current))
	case errors.Is(err, store.ErrTemplateGrantGone):
		wallPresetWriteRefusal(types.TemplateDenyNotGroupAdmin, types.TemplateOwner{Scope: types.TemplateScopeGroup}).write(nil, w, r)
	case errors.Is(err, store.ErrNotFound):
		writeWallPresetNotFound(w)
	default:
		writeServerError(w, r, "write wall preset", err)
	}
}

// auditWallPreset records a write: where the preset is published, its version and name, and the
// knobs it names with each list's size. A shared preset's whole spec is recorded too, since its
// audience reads it anyway and an auditor needs to see what changed; a person preset's lists are
// private, as a person's own walls are. A delete records no spec.
func (s *Server) auditWallPreset(r *http.Request, action string, p types.WallPreset) {
	data := map[string]any{}
	if action != auditWallPresetDelete {
		maps.Copy(data, wallsAuditData(p.Spec))
		if p.Scope != types.WallPresetScopePerson {
			data["spec"] = p.Spec
		}
	}
	data["scope"], data["version"], data["name"] = p.Scope, p.Version, p.Name
	if p.GroupID != "" {
		data["group_id"] = p.GroupID
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, p.ID, "success", mustJSON(data)))
}

func (s *Server) handleCreateWallPreset(w http.ResponseWriter, r *http.Request) {
	ps, c, ok := s.wallPresetRoute(w, r, true)
	if !ok {
		return
	}
	var req client.WallPresetSaveRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	owner, diag := c.targetOwner(types.TemplateScope(req.Scope), req.GroupID)
	if diag != nil {
		wallPresetInvalid(diag.Message).write(s, w, r)
		return
	}
	if wallPresetWriteRefusal(types.TemplateAuthorize(c.actor, owner, types.TemplateWrite), owner).write(s, w, r) {
		return
	}
	p, refusal := wallPresetForSave(req, owner)
	if refusal.write(s, w, r) {
		return
	}
	p.ID, p.UpdatedBy = uuid.NewString(), c.actor.Person
	created, err := ps.CreateWallPreset(r.Context(), p, c.grantCheck(owner))
	if err != nil {
		writeWallPresetStoreErr(w, r, err)
		return
	}
	s.auditWallPreset(r, auditWallPresetCreate, created)
	writeJSON(w, http.StatusCreated, wallPresetView(created))
}

// handleUpdateWallPreset replaces a preset's name, description and spec. expected_version is
// required, and a preset that moved on answers wall_preset_version_conflict and keeps nothing. The
// scope never changes: a preset reaches another audience by being saved there as a new one.
func (s *Server) handleUpdateWallPreset(w http.ResponseWriter, r *http.Request) {
	ps, c, ok := s.wallPresetRoute(w, r, true)
	if !ok {
		return
	}
	var req client.WallPresetSaveRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	cur, ok := s.writableWallPreset(w, r, ps, c)
	if !ok {
		return
	}
	var refusal *runRefusal
	if req.ExpectedVersion == nil {
		refusal = wallPresetInvalid("expected_version is required: the version you read")
	} else if req.Scope != "" || req.GroupID != "" {
		owner, d := c.targetOwner(types.TemplateScope(cmp.Or(req.Scope, cur.Scope)), req.GroupID)
		if d != nil || owner.Scope != cur.Owner().Scope || owner.Group != cur.GroupID {
			refusal = wallPresetInvalid("scope cannot change: save the preset as a new one where you want it")
		}
	}
	if refusal.write(s, w, r) {
		return
	}
	p, refusal := wallPresetForSave(req, cur.Owner())
	if refusal.write(s, w, r) {
		return
	}
	p.ID, p.UpdatedBy = cur.ID, c.actor.Person
	updated, err := ps.UpdateWallPreset(r.Context(), p, *req.ExpectedVersion, c.grantCheck(cur.Owner()))
	if err != nil {
		writeWallPresetStoreErr(w, r, err)
		return
	}
	s.auditWallPreset(r, auditWallPresetUpdate, updated)
	writeJSON(w, http.StatusOK, wallPresetView(updated))
}

// handleDeleteWallPreset removes a preset. Runs already launched with it keep their frozen copy.
func (s *Server) handleDeleteWallPreset(w http.ResponseWriter, r *http.Request) {
	ps, c, ok := s.wallPresetRoute(w, r, true)
	if !ok {
		return
	}
	cur, ok := s.writableWallPreset(w, r, ps, c)
	if !ok {
		return
	}
	deleted, err := ps.DeleteWallPreset(r.Context(), uuid.MustParse(cur.ID), c.grantCheck(cur.Owner()))
	if err != nil {
		writeWallPresetStoreErr(w, r, err)
		return
	}
	s.auditWallPreset(r, auditWallPresetDelete, deleted)
	w.WriteHeader(http.StatusNoContent)
}
