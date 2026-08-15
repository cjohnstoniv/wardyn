// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// base_images.go — the tier-2 BASE-IMAGE CATALOG endpoints: shared, reusable
// images (registry ref / custom recipe / BYO) referenced by many workspaces.
// "Recommended" is deliberately absent: that build is DERIVED from one
// workspace's merged source profiles, has no catalog identity, and the store's
// CHECK constraint refuses it structurally.

// validateBaseImageWrite mirrors the workspace's own base-image shape checks
// (validateWorkspaceBaseImage's rules, catalog-shaped): a known reusable kind,
// a non-empty image ref with no control characters, and custom steps under the
// same caps the wizard's build-steps editor enforces server-side.
func validateBaseImageWrite(b types.BaseImageEntry) string {
	switch b.Kind {
	case "registry", "byo":
		if len(b.Steps) > 0 {
			return "steps are custom-kind only"
		}
	case "custom":
	default:
		return `kind must be "registry", "custom", or "byo" — "recommended" is derived per workspace, never a catalog entry`
	}
	if strings.TrimSpace(b.Name) == "" {
		return "name is required"
	}
	img := strings.TrimSpace(b.Image)
	if img == "" {
		return "image is required"
	}
	if !repoFieldSafe(img) {
		return "image must not contain whitespace or control characters"
	}
	if len(b.Steps) > maxBaseImageSteps {
		return fmt.Sprintf("too many steps (max %d)", maxBaseImageSteps)
	}
	for i, step := range b.Steps {
		if len(step) > maxBaseImageStepLen {
			return fmt.Sprintf("steps[%d]: too long (max %d chars)", i, maxBaseImageStepLen)
		}
	}
	return ""
}

// handleListBaseImages returns the catalog.
//
//	GET /api/v1/base-images
func (s *Server) handleListBaseImages(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.ListBaseImages(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list base images: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"base_images": list})
}

// handleCreateBaseImage upserts a catalog image by identity (kind, image,
// steps) — saving the same recipe twice is one entry, which is the reuse the
// catalog exists for. 200 on an identity hit, 201 on a new row.
//
//	POST /api/v1/base-images
func (s *Server) handleCreateBaseImage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind  string   `json:"kind"`
		Name  string   `json:"name"`
		Image string   `json:"image"`
		Steps []string `json:"steps,omitempty"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	explicitName := strings.TrimSpace(req.Name)
	entry := types.BaseImageEntry{
		ID: uuid.New(), Kind: req.Kind, Name: explicitName,
		Image: strings.TrimSpace(req.Image), Steps: req.Steps,
		CreatedAt: s.cfg.Now().UTC(), UpdatedAt: s.cfg.Now().UTC(),
	}
	if entry.Name == "" {
		entry.Name = lastPathSegment(entry.Image)
	}
	if msg := validateBaseImageWrite(entry); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	created, err := s.cfg.Store.UpsertBaseImage(r.Context(), entry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert base image: "+err.Error())
		return
	}
	status := http.StatusCreated
	if created.ID != entry.ID {
		status = http.StatusOK
		// W7-S1-3: an identity hit is the Add dialog's ONLY rename route (no
		// separate edit UI/API/CLI/SDK) — apply the operator's explicitly
		// typed name here, never inside UpsertBaseImage's own conflict clause
		// (see its doc comment: that upsert is also a passthrough path with
		// no rename intent at all, which must never clobber a custom name).
		if explicitName != "" {
			updated, uerr := s.cfg.Store.UpdateBaseImageName(r.Context(), created.ID, explicitName)
			if uerr != nil {
				writeError(w, http.StatusInternalServerError, "apply name to existing base image: "+uerr.Error())
				return
			}
			created = updated
		}
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"base_image.write", created.ID.String(), "success", mustJSON(map[string]any{
			"kind": created.Kind, "image": created.Image, "steps": len(created.Steps),
		})))
	writeJSON(w, status, created)
}

// handleDeleteBaseImage removes a catalog row. In use → 409 naming the
// workspaces; ?force=1 detaches them, which is HONEST here rather than
// destructive: a detached workspace's base_image_id goes NULL, and NULL means
// the derived recommended build — a working state, stated as such. The in-use
// gate and the delete are ONE atomic statement in the store (DeleteBaseImage):
// WorkspacesUsingBaseImage here only names who's using it for the 409 body,
// it does not decide the outcome, so a workspace attaching between this call
// and the delete can never slip through.
//
//	DELETE /api/v1/base-images/{id}[?force=1]
func (s *Server) handleDeleteBaseImage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "base image")
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	names, err := s.cfg.Store.WorkspacesUsingBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check base image use: "+err.Error())
		return
	}
	if err := s.cfg.Store.DeleteBaseImage(r.Context(), id, force); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"base image is used by %d workspace(s): %s — repoint them first, or pass ?force=1 (they fall back to the derived recommended build)",
				len(names), strings.Join(names, ", ")))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such base image")
			return
		}
		writeError(w, http.StatusInternalServerError, "delete base image: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"base_image.delete", id.String(), "success", mustJSON(map[string]any{
			"detached_from": names, "forced": force,
		})))
	w.WriteHeader(http.StatusNoContent)
}
