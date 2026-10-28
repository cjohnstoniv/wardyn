// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// base_images.go — the tier-2 BASE-IMAGE CATALOG endpoints: shared, reusable
// images (registry ref / custom recipe / BYO) referenced by many workspaces.
// "Recommended" is deliberately absent: that build is DERIVED from one
// workspace's merged source profiles, has no catalog identity, and the store's
// CHECK constraint refuses it structurally.

// validateBaseImageWrite mirrors the workspace's own base-image shape checks
// (validateWorkspaceBaseImage's rules, catalog-shaped): a known reusable kind and
// a non-empty image ref with no control characters.
//
// The step caps are gone: they justified themselves as "the same caps the
// wizard's build-steps editor enforces", and that editor does not exist — nor
// could the steps ever run. See types.BaseImageEntry.Steps.
func validateBaseImageWrite(b types.BaseImageEntry) string {
	switch b.Kind {
	case "registry", "byo", "custom", types.BaseImageKindPrewrapped:
	default:
		return `kind must be "registry", "custom", "byo" or "prewrapped" — "recommended" is derived per workspace, never a catalog entry`
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
	if !imageRefPathSafe(img) {
		return fmt.Sprintf(image400DotSegment, "image")
	}
	return ""
}

// image400DotSegment — %s is the field name.
const image400DotSegment = `%s is not an image reference — an image reference has no backslash and no "", "." or ".." path segment`

// imageRefPathSafe reports whether an image ref has no backslash and no
// empty, "." or ".." segment. No registry names an image that way, and an
// image ref is also a capability value the console builds a control path from
// (the Images tab's Available to), so a member-typed `../policy/<uuid>` must
// never be stored.
// Not repoLocatorPathSafe: that reads "host:5000/..." and "img:tag" as
// scp-form and checks only what follows the colon.
func imageRefPathSafe(ref string) bool {
	if strings.Contains(ref, `\`) {
		return false
	}
	for _, seg := range strings.Split(strings.TrimSpace(ref), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// handleListBaseImages returns the catalog. A security admin sees the prewrapped rows only: the
// other kinds name the operator's registry coordinate (mayWriteBaseImageKind). image_builder says
// whether this control plane wraps the other kinds at all; the console's Add image reads it.
//
//	GET /api/v1/base-images
func (s *Server) handleListBaseImages(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.ListBaseImages(r.Context())
	if err != nil {
		writeServerError(w, r, "list base images", err)
		return
	}
	now := s.cfg.Now()
	out := make([]types.BaseImageEntry, 0, len(list))
	for _, b := range list {
		if s.mayWriteBaseImageKind(r.Context(), b.Kind) {
			out = append(out, prewrappedView(b, now))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"base_images": out, "image_builder": s.cfg.ImageBuilder != nil})
}

// mayWriteBaseImageKind: an admin writes every kind; a security admin writes prewrapped only (design
// final §3), and the route group has already refused anyone else.
func (s *Server) mayWriteBaseImageKind(ctx context.Context, kind string) bool {
	return s.isOperator(ctx) || kind == types.BaseImageKindPrewrapped
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
		entry.Name = lastPathSegment(entry.Image, nil)
	}
	if !s.mayWriteBaseImageKind(r.Context(), entry.Kind) {
		s.refuse(w, r, authz.Deny(authz.ReasonAdminSurface, r.URL.Path, ""))
		return
	}
	if msg := validateBaseImageWrite(entry); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonBaseImageWriteInvalid, msg)
		return
	}
	var checker runner.PrewrappedImageChecker
	if entry.Kind == types.BaseImageKindPrewrapped {
		if !prewrappedPinned(entry.Image) {
			writeErrorReason(w, http.StatusBadRequest, reasonPrewrappedImageNotPinned,
				"a prewrapped image is registered by digest: <repository>[:tag]@sha256:<64 hex>")
			return
		}
		var why string
		if checker, why = s.prewrappedChecker(); checker == nil {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonPrewrappedCheckUnavailable,
				"a prewrapped image is checked on this control plane's own Kubernetes cluster, and it cannot check one: "+why)
			return
		}
		entry.Check = &types.BaseImageCheck{Attempt: uuid.New()}
	}
	created, err := s.cfg.Store.UpsertBaseImage(r.Context(), entry)
	if err != nil {
		writeServerError(w, r, "upsert base image", err)
		return
	}
	if created.ID == entry.ID && checker != nil {
		s.startImageCheck(r, checker, created)
	}
	status := http.StatusCreated
	if created.ID != entry.ID {
		status = http.StatusOK
		// An identity hit is the Add dialog's ONLY rename route (no
		// separate edit UI/API/CLI/SDK) — apply the operator's explicitly
		// typed name here, never inside UpsertBaseImage's own conflict clause
		// (see its doc comment: that upsert is also a passthrough path with
		// no rename intent at all, which must never clobber a custom name).
		if explicitName != "" {
			updated, uerr := s.cfg.Store.UpdateBaseImageName(r.Context(), created.ID, explicitName)
			if uerr != nil {
				writeServerError(w, r, "apply name to existing base image", uerr)
				return
			}
			created = updated
		}
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"base_image.write", created.ID.String(), "success", mustJSON(map[string]any{
			"kind": created.Kind, "image": created.Image, "steps": len(created.Steps),
		})))
	writeJSON(w, status, prewrappedView(created, s.cfg.Now()))
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
	// The stored kind decides who may delete, before any referencing workspace is named. Detaching
	// workspaces stays an admin's.
	if !s.isOperator(r.Context()) {
		b, err := s.cfg.Store.GetBaseImage(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeErrorReason(w, http.StatusNotFound, reasonBaseImageNotFound, "no such base image")
			return
		}
		if err != nil {
			writeServerError(w, r, "read base image", err)
			return
		}
		if force || !s.mayWriteBaseImageKind(r.Context(), b.Kind) {
			s.refuse(w, r, authz.Deny(authz.ReasonAdminSurface, r.URL.Path, ""))
			return
		}
	}
	names, err := s.cfg.Store.WorkspacesUsingBaseImage(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "check base image use", err)
		return
	}
	if err := s.cfg.Store.DeleteBaseImage(r.Context(), id, force); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErrorReason(w, http.StatusConflict, reasonBaseImageInUse, fmt.Sprintf(
				"base image is used by %d workspace(s): %s — repoint them first, or pass ?force=1 (they fall back to the derived recommended build)",
				len(names), strings.Join(names, ", ")))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeErrorReason(w, http.StatusNotFound, reasonBaseImageNotFound, "no such base image")
			return
		}
		writeServerError(w, r, "delete base image", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"base_image.delete", id.String(), "success", mustJSON(map[string]any{
			"detached_from": names, "forced": force,
		})))
	w.WriteHeader(http.StatusNoContent)
}
