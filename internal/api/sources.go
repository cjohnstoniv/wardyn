// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
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
//
// DEADCODE-1: there is no PUT /sources/{id} or GET /base-images/{id} — a
// source's own contract is authored through POST /sources instead:
// re-POSTing an existing identity with a new requirements body now APPLIES
// it (WSPIPE-8), so a contract edit lands on the SAME endpoint console/CLI/
// SDK already call to create a source, rather than a second route no
// console button, CLI flag or SDK method ever reached (the SDK's
// client.SourceRequest already carries Requirements; a CLI flag to drive it
// is the remaining gap, tracked separately). The write-once,
// discard-on-conflict handler that route used to be (handleUpdateSource) and
// its base-image GET-by-id twin (handleGetBaseImage, whose only caller was
// its own now-removed route) are gone, not stubbed.
func (s *Server) mountLibraryRoutes(r chi.Router, operatorOnly chi.Router) {
	r.Get("/sources", s.handleListSources)
	operatorOnly.Post("/sources", s.handleCreateSource)
	r.Get("/sources/{id}", s.handleGetSource)
	operatorOnly.Post("/sources/{id}/scan", s.handleScanSource)
	operatorOnly.Delete("/sources/{id}", s.handleDeleteSource)
	r.Get("/base-images", s.handleListBaseImages)
	operatorOnly.Post("/base-images", s.handleCreateBaseImage)
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
		return canonicalRepoLocator(locator), strings.TrimSpace(ref)
	}
	return locator, strings.TrimSpace(ref)
}

// canonicalRepoLocator lowercases ONLY the scheme+host of a repo locator,
// leaving the path verbatim: a case-sensitive forge (self-hosted GitLab/
// Gitea/Bitbucket, or any case-sensitive path segment) needs the operator's
// clone path preserved exactly as authored, while scheme/host is
// case-insensitive by definition — so "https://Git.Corp.Example/MyGroup/
// MyRepo.git" dedupes with the lowercase spelling but hydrate always serves
// "MyGroup/MyRepo.git" back, not "mygroup/myrepo.git". A bare "<org>/<name>"
// GitHub slug has no host component in the string itself and passes through
// unchanged.
func canonicalRepoLocator(locator string) string {
	if strings.Contains(locator, "://") {
		if u, err := url.Parse(locator); err == nil && u.Host != "" {
			u.Scheme = strings.ToLower(u.Scheme)
			u.Host = strings.ToLower(u.Host)
			return u.String()
		}
		return locator
	}
	// scp-form user@host:path (no scheme) — lowercase only the host between
	// '@' and the following ':'.
	if at := strings.IndexByte(locator, '@'); at >= 0 {
		rest := locator[at+1:]
		if colon := strings.IndexByte(rest, ':'); colon >= 0 {
			return locator[:at+1] + strings.ToLower(rest[:colon]) + rest[colon:]
		}
	}
	// Bare "<org>/<name>" GitHub slug: repoCloneURL already resolves this
	// whole alias family (any case, with or without a trailing ".git") to one
	// github.com clone URL, and gitBrokerKeyFromSlug canonicalizes it the same
	// lowercased, ".git"-stripped way once it gets there — so dedupe on the
	// identical key here instead of letting two spellings of one repo mint two
	// library entries.
	if repoCloneURL(locator) != "" {
		return strings.ToLower(strings.TrimSuffix(locator, ".git"))
	}
	return locator
}

// validateSourceWrite checks a library source the way the workspace's own
// source validation always has — the same deny-list for a host path, the same
// slug/URL discipline for a repo — plus the contract grammar, with ONE
// narrowing: a source's contract may not carry integration:<id> keys.
// Integrations compose at the aggregate (tier 3, owner decision); the fold
// would pass them through unharmed, so relaxing later is this one branch.
func validateSourceWrite(src types.Source) string {
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
	// Checked AFTER kind/locator: Name derives from the locator when the
	// caller didn't set one (handleCreateSource), so an empty Name is a
	// SYMPTOM of a missing/invalid locator, not an independent error — put
	// the kind/locator checks first so a missing --locator reports itself,
	// not the misleading "name is required" every missing-locator request
	// used to surface instead (W6-S1-6).
	if strings.TrimSpace(src.Name) == "" {
		return "name is required"
	}
	if len(src.Requirements) > maxWorkspaceRequirements {
		return fmt.Sprintf("too many requirements (max %d)", maxWorkspaceRequirements)
	}
	for _, key := range sortedKeys(src.Requirements) {
		if typ, _, ok := types.SplitRequirementKey(key); ok && typ == "integration" {
			return fmt.Sprintf("requirement %q: integrations compose at the workspace, not on a source — attach the source and require the integration there", key)
		}
		// WSPIPE-5: mirror FoldWorkspaceContract rule 4 at the write boundary.
		// The fold silently DROPS a write:<path> row whose path is not this
		// source's own Locator (a shared source may only claim write on
		// itself, never widen a sibling mount), so validating it here without
		// that check let an operator PUT a row that answers 200, shows up on
		// GET, and can never take effect — the build fails at run time with
		// nothing anywhere explaining why.
		if typ, path, ok := types.SplitRequirementKey(key); ok && typ == "write" && path != src.Locator {
			return fmt.Sprintf("requirement %q: a source's write: key must name its own locator (%q) — a shared source may only declare write on itself; declare a sibling path on the attaching workspace's own contract instead", key, src.Locator)
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
// not 201) rather than a conflict. A type ALIAS (not a hand-maintained copy)
// of the public SDK's client.SourceRequest — the alias discipline the other
// request DTOs already follow (dto_alias_test.go pins it at compile time).
type sourceRequest = client.SourceRequest

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

// handleCreateSource upserts a library source by identity. Re-POSTing an
// identity already in the library is not merely a no-op read of the existing
// row: a non-nil req.Requirements is APPLIED to it (WSPIPE-8) — the only way
// to author a source's own contract from any shipped surface (console, CLI,
// or a script driving the SDK directly), now that there is no separate PUT
// route for it (DEADCODE-1). Without this, UpsertSource's ON CONFLICT clause
// only ever bumps updated_at, so a bootstrap script's re-run with a NEW
// contract silently kept the OLD one and still audited "success".
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
		if req.Requirements != nil {
			updated, uerr := s.cfg.Store.UpdateSourceConfig(r.Context(), created.ID, created.Name, req.Requirements)
			if uerr != nil {
				writeError(w, http.StatusInternalServerError, "apply requirements to existing source: "+uerr.Error())
				return
			}
			created = updated
		}
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

// deleteSourceResponse is handleDeleteSource's 200 body: the workspaces the
// delete detached the source from (empty when it wasn't attached to any).
// W6-S1-2: force silently narrows those workspaces down to their remaining
// sources — nothing 422s, so this is the operator's only visibility into
// which workspaces just lost a mount; a bare 204 gave them none.
type deleteSourceResponse struct {
	DetachedFrom []string `json:"detached_from"`
}

// handleDeleteSource removes a library source — LOUDLY refusing while
// workspaces attach it: in-use is a 409 naming every attaching workspace;
// ?force=1 is the explicit detach-everywhere escape. Forcing does NOT make a
// workspace's next run fail loudly (W6-S1-2 — the mount gate has no check for
// a source that used to be there): it un-mounts the source and the workspace's
// remaining sources mount as normal, so DetachedFrom above is the only signal
// the operator gets that anything changed. The in-use gate and the delete are
// ONE atomic statement in the store (DeleteSource): WorkspacesAttaching here
// only names who's attached for the 409/200 body, it does not decide the
// outcome, so a workspace attaching between this call and the delete can
// never slip through.
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
	if err := s.cfg.Store.DeleteSource(r.Context(), id, force); err != nil {
		if errors.Is(err, store.ErrConflict) {
			if force {
				// STORE-1: force still refuses when detaching would leave a
				// workspace with ZERO attachments (the store's own error names
				// which ones) — never the "pass ?force=1" hint, force is
				// already set.
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"source is attached by %d workspace(s): %s — detach them first, or pass ?force=1 to detach everywhere and delete",
				len(names), strings.Join(names, ", ")))
			return
		}
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
	writeJSON(w, http.StatusOK, deleteSourceResponse{DetachedFrom: names})
}

// lastPathSegment names a source from its locator when the caller didn't:
// the trailing path/slug segment, or the locator itself when there is none.
func lastPathSegment(locator string) string {
	if locator == "" {
		return locator
	}
	return path.Base(locator)
}

// overridesBySourceID indexes existing's per-source Overrides by source id —
// upsertAndAttach's carry-forward lookup (WSPIPE-7): a plain composition edit
// (e.g. a rename) that doesn't itself touch Overrides must not silently wipe
// a stance the operator set on an earlier PUT, since upsertAndAttach rebuilds
// every attachment from scratch on every call.
func overridesBySourceID(existing []types.WorkspaceAttachment) map[uuid.UUID]map[string]string {
	out := make(map[uuid.UUID]map[string]string, len(existing))
	for _, att := range existing {
		if att.SourceID != nil && att.Overrides != nil {
			out[*att.SourceID] = att.Overrides
		}
	}
	return out
}

// upsertAndAttach turns a request's embedded sources + base image into the
// three-tier shape: each dir/repo upserts into the shared library (dedupe on
// canonical identity — the request "already configured this" case lands on the
// existing entry, contract and all) and becomes an attachment carrying the
// request's per-attachment target/writable/overrides; ephemerals stay inline.
// A registry/custom/byo base image upserts into the catalog; "recommended"/nil
// yields a nil id — NULL is the derived-build marker.
//
// existing is the workspace's CURRENT attachments before this edit (nil for a
// brand-new workspace) — its per-source Overrides carry forward by SourceID
// (WSPIPE-7) onto a source the request doesn't explicitly set Overrides for.
//
// r (not a bare ctx) so a genuinely NEW library row can be audited under the
// request's own actor (WSPIPE-4): a plain "sources": <count> on
// workspace.create/update names no host path, so an incident review asking
// which directory was exposed, and whether read-write, had no answer in the
// trail besides the mutable workspace row itself.
func (s *Server) upsertAndAttach(r *http.Request, srcs []types.WorkspaceSource, baseImage *types.WorkspaceBaseImage, existing []types.WorkspaceAttachment) ([]types.WorkspaceAttachment, *uuid.UUID, error) {
	ctx := r.Context()
	now := s.cfg.Now().UTC()
	carried := overridesBySourceID(existing)
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
		newID := uuid.New()
		row, err := s.cfg.Store.UpsertSource(ctx, types.Source{
			ID: newID, Kind: kind, Locator: locator, Ref: ref,
			Name: lastPathSegment(locator), Status: types.WorkspacePendingScan,
			CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			return nil, nil, err
		}
		if row.ID == newID {
			// A genuinely NEW library row, not a hit on an existing identity —
			// audit it (WSPIPE-4), including the writable consent: it is the
			// operator's authorization for a sandboxed agent's changes to
			// persist to a HOST directory, so it belongs in the trail.
			s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"source.write", row.ID.String(), "success", mustJSON(map[string]any{
					"kind": string(row.Kind), "locator": row.Locator, "ref": row.Ref, "writable": src.Writable,
				})))
		}
		id := row.ID
		overrides := src.Overrides
		if overrides == nil {
			overrides = carried[id]
		}
		atts = append(atts, types.WorkspaceAttachment{
			SourceID: &id, Target: src.Target, Writable: src.Writable, Overrides: overrides,
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
