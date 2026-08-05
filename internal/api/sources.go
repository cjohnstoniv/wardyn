// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sources.go — the tier-1 SOURCE LIBRARY endpoints: a repo/dir configured
// once (its own requirements contract, its own scan) and attached to many
// workspaces. Reads are humanOrAdmin like the workspace list; writes are
// operatorOnly and audited, the exact posture of the workspaces block.

// mountLibraryRoutes registers the tier-1 source library and tier-2 base-image
// catalog endpoints: a repo/dir configured once (its own contract + scan,
// attached to many workspaces) and the shared registry/custom/byo images
// ("recommended" is derived per workspace, never a row). Reads humanOrAdmin,
// writes operatorOnly — the workspaces block's exact posture.
func (s *Server) mountLibraryRoutes(r chi.Router, operatorOnly chi.Router) {
	r.Get("/sources", s.handleListSources)
	operatorOnly.Post("/sources", s.handleCreateSource)
	r.Get("/sources/{id}", s.handleGetSource)
	operatorOnly.Post("/sources/{id}/scan", s.handleScanSource)
	operatorOnly.Put("/sources/{id}", s.handleUpdateSource)
	operatorOnly.Delete("/sources/{id}", s.handleDeleteSource)
	r.Get("/base-images", s.handleListBaseImages)
	operatorOnly.Post("/base-images", s.handleCreateBaseImage)
	r.Get("/base-images/{id}", s.handleGetBaseImage)
	operatorOnly.Delete("/base-images/{id}", s.handleDeleteBaseImage)
}

// canonicalSourceIdentity normalizes the identity triple the library dedupes
// on. The store stores what it is given, so EVERY write path must come
// through here or "/home/me/payments" and "/home/me/payments/" become two
// library entries with two contracts.
func canonicalSourceIdentity(kind types.SourceKind, locator, ref string) (string, string) {
	locator = strings.TrimSpace(locator)
	switch kind {
	case types.SourceLocalDir:
		if locator != "/" {
			locator = strings.TrimRight(locator, "/")
		}
		return locator, ""
	case types.SourceRepo:
		return strings.ToLower(locator), strings.TrimSpace(ref)
	}
	return locator, strings.TrimSpace(ref)
}

// validateSourceWrite checks a library source the way the workspace's own
// source validation always has — the same deny-list for a host path, the same
// slug/URL discipline for a repo — plus the contract grammar, with ONE
// narrowing: a source's contract may not carry integration:<id> keys.
// Integrations compose at the aggregate (tier 3, owner decision); the fold
// would pass them through unharmed, so relaxing later is this one branch.
func validateSourceWrite(src types.Source) string {
	if strings.TrimSpace(src.Name) == "" {
		return "name is required"
	}
	switch src.Kind {
	case types.SourceLocalDir:
		if src.Locator == "" {
			return "locator (the host directory path) is required"
		}
		if err := runner.ValidateMountSource(src.Locator); err != nil {
			return "invalid path: " + err.Error()
		}
	case types.SourceRepo:
		if src.Locator == "" {
			return "locator (the repo slug or clone URL) is required"
		}
		if !repoFieldSafe(src.Locator) {
			return "locator must not contain control characters or whitespace"
		}
		if repoCloneURL(src.Locator) == "" {
			return "locator is not a recognized repo slug or http(s) clone URL"
		}
	default:
		return `kind must be "local_dir" or "repo" (ephemeral scratch is a per-workspace row, not a library entry)`
	}
	if len(src.Requirements) > maxWorkspaceRequirements {
		return fmt.Sprintf("too many requirements (max %d)", maxWorkspaceRequirements)
	}
	for _, key := range sortedKeys(src.Requirements) {
		if typ, _, ok := types.SplitRequirementKey(key); ok && typ == "integration" {
			return fmt.Sprintf("requirement %q: integrations compose at the workspace, not on a source — attach the source and require the integration there", key)
		}
		if msg := validateWorkspaceRequirement(key, src.Requirements[key]); msg != "" {
			return msg
		}
	}
	return ""
}

// sourceRequest is the POST/PUT body. POST is an UPSERT by identity — the
// library's whole point is that the same dir/repo configured twice is one
// entry — so a re-POST of an existing identity returns the existing row (200,
// not 201) rather than a conflict.
type sourceRequest struct {
	Kind         types.SourceKind                      `json:"kind"`
	Locator      string                                `json:"locator"`
	Ref          string                                `json:"ref,omitempty"`
	Name         string                                `json:"name"`
	Requirements map[string]types.WorkspaceRequirement `json:"requirements,omitempty"`
}

// handleListSources returns the whole library.
//
//	GET /api/v1/sources
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list sources: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": list})
}

// handleCreateSource upserts a library source by identity.
//
//	POST /api/v1/sources
func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	locator, ref := canonicalSourceIdentity(req.Kind, req.Locator, req.Ref)
	if req.Name == "" {
		req.Name = lastPathSegment(locator)
	}
	src := types.Source{
		ID: uuid.New(), Kind: req.Kind, Locator: locator, Ref: ref, Name: strings.TrimSpace(req.Name),
		Requirements: req.Requirements, Status: types.WorkspacePendingScan,
		CreatedAt: s.cfg.Now().UTC(), UpdatedAt: s.cfg.Now().UTC(),
	}
	if msg := validateSourceWrite(src); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	created, err := s.cfg.Store.UpsertSource(r.Context(), src)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert source: "+err.Error())
		return
	}
	status := http.StatusCreated
	if created.ID != src.ID {
		status = http.StatusOK // identity already in the library — that IS the feature
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"source.write", created.ID.String(), "success", mustJSON(map[string]any{
			"kind": string(created.Kind), "locator": created.Locator, "ref": created.Ref,
		})))
	writeJSON(w, status, created)
}

// handleGetSource returns one library source.
//
//	GET /api/v1/sources/{id}
func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "source")
	if !ok {
		return
	}
	src, err := s.cfg.Store.GetSource(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such source")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get source: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, src)
}

// handleUpdateSource replaces a source's operator-editable fields: name and
// its OWN requirements contract. Identity (kind/locator/ref) is immutable —
// the same rule a workspace source-change follows: different code is a
// different source, with a fresh contract, not an edit.
//
//	PUT /api/v1/sources/{id}
func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "source")
	if !ok {
		return
	}
	var req struct {
		Name         string                                `json:"name"`
		Requirements map[string]types.WorkspaceRequirement `json:"requirements,omitempty"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	cur, err := s.cfg.Store.GetSource(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such source")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get source: "+err.Error())
		return
	}
	cur.Name, cur.Requirements = strings.TrimSpace(req.Name), req.Requirements
	if msg := validateSourceWrite(cur); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	updated, err := s.cfg.Store.UpdateSourceConfig(r.Context(), id, cur.Name, cur.Requirements)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update source: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"source.write", id.String(), "success", mustJSON(map[string]any{
			"requirements": len(updated.Requirements),
		})))
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteSource removes a library source — LOUDLY refusing while
// workspaces attach it. A dangling attachment silently un-mounts code and
// turns runs into unexplained 422s at the mount gate, so in-use is a 409
// naming every attaching workspace; ?force=1 is the explicit
// detach-everywhere escape.
//
//	DELETE /api/v1/sources/{id}[?force=1]
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "source")
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	names, err := s.cfg.Store.WorkspacesAttaching(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check source use: "+err.Error())
		return
	}
	if len(names) > 0 && !force {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"source is attached by %d workspace(s): %s — detach them first, or pass ?force=1 to detach everywhere and delete",
			len(names), strings.Join(names, ", ")))
		return
	}
	if err := s.cfg.Store.DeleteSource(r.Context(), id, force); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such source")
			return
		}
		writeError(w, http.StatusInternalServerError, "delete source: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"source.delete", id.String(), "success", mustJSON(map[string]any{
			"detached_from": names, "forced": force,
		})))
	w.WriteHeader(http.StatusNoContent)
}

// lastPathSegment names a source from its locator when the caller didn't:
// the trailing path/slug segment, or the locator itself when there is none.
func lastPathSegment(locator string) string {
	trimmed := strings.TrimRight(locator, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 && i+1 < len(trimmed) {
		return trimmed[i+1:]
	}
	if trimmed == "" {
		return locator
	}
	return trimmed
}

// upsertAndAttach turns a request's embedded sources + base image into the
// three-tier shape: each dir/repo upserts into the shared library (dedupe on
// canonical identity — the request "already configured this" case lands on the
// existing entry, contract and all) and becomes an attachment carrying the
// request's per-attachment target/writable; ephemerals stay inline. A
// registry/custom/byo base image upserts into the catalog; "recommended"/nil
// yields a nil id — NULL is the derived-build marker.
func (s *Server) upsertAndAttach(ctx context.Context, srcs []types.WorkspaceSource, baseImage *types.WorkspaceBaseImage) ([]types.WorkspaceAttachment, *uuid.UUID, error) {
	now := s.cfg.Now().UTC()
	atts := make([]types.WorkspaceAttachment, 0, len(srcs))
	for _, src := range srcs {
		switch src.Type {
		case types.WorkspaceSourceTypeEphemeral:
			atts = append(atts, types.WorkspaceAttachment{Ephemeral: true, Target: src.Target})
			continue
		case types.WorkspaceSourceTypeLocalDir, types.WorkspaceSourceTypeRepo:
		default:
			continue // decodeWorkspaceRequest already rejected anything else
		}
		kind := types.SourceKind(src.Type)
		locator := src.Path
		if kind == types.SourceRepo {
			locator = src.Source
		}
		locator, ref := canonicalSourceIdentity(kind, locator, src.Ref)
		row, err := s.cfg.Store.UpsertSource(ctx, types.Source{
			ID: uuid.New(), Kind: kind, Locator: locator, Ref: ref,
			Name: lastPathSegment(locator), Status: types.WorkspacePendingScan,
			CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			return nil, nil, err
		}
		id := row.ID
		atts = append(atts, types.WorkspaceAttachment{
			SourceID: &id, Target: src.Target, Writable: src.Writable,
		})
	}
	var baseImageID *uuid.UUID
	if baseImage != nil && baseImage.Kind != "" && baseImage.Kind != "recommended" {
		row, err := s.cfg.Store.UpsertBaseImage(ctx, types.BaseImageEntry{
			ID: uuid.New(), Kind: baseImage.Kind, Name: lastPathSegment(baseImage.Image),
			Image: baseImage.Image, Steps: baseImage.Steps,
			CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			return nil, nil, err
		}
		id := row.ID
		baseImageID = &id
	}
	return atts, baseImageID, nil
}
