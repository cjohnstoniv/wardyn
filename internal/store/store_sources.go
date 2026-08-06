// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// store_sources.go — the three-tier split's tier-1 (sources library) and
// tier-2 (base-image catalog) storage, plus the workspace HYDRATE pass that
// turns attachments back into the derived read-only view every pre-split
// consumer keeps reading (Sources/BaseImage/Profile/Status), and the folded
// EffectiveRequirements the run path consumes.

// ─── Sources (tier 1) ────────────────────────────────────────────────────────

const sourceCols = `id, kind, locator, ref, name, requirements, profile, status, ` +
	`active_run_id, created_at, updated_at`

func scanSource(row pgx.Row) (types.Source, error) {
	var src types.Source
	var kind, status string
	var reqRaw, profileRaw []byte
	err := row.Scan(&src.ID, &kind, &src.Locator, &src.Ref, &src.Name,
		&reqRaw, &profileRaw, &status, &src.ActiveRunID, &src.CreatedAt, &src.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Source{}, ErrNotFound
	}
	if err != nil {
		return types.Source{}, fmt.Errorf("store: scan source: %w", err)
	}
	src.Kind = types.SourceKind(kind)
	src.Status = types.WorkspaceStatus(status)
	if reqRaw != nil {
		_ = json.Unmarshal(reqRaw, &src.Requirements) // fail safe to "no contract"
	}
	src.Profile = profileRaw
	return src, nil
}

// sourceRequirementsParam marshals a requirements map for a jsonb param, or
// nil on empty/error — callers COALESCE nil back to '{}' in the SQL. Shared by
// the explicit-requirements writers below and the scan-seed fill (seed and a
// source's own requirements are the same concrete type).
func sourceRequirementsParam(m map[string]types.WorkspaceRequirement) []byte {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil // unreachable for this concrete type; fail-safe to "none"
	}
	return b
}

// UpsertSource inserts a library source or returns the existing row with the
// same identity (kind, locator, ref) — ONE statement, no read-then-write race:
// the UNIQUE constraint IS the dedupe rule. The no-op DO UPDATE lets RETURNING
// yield the surviving row either way. Identity fields must arrive
// CANONICALIZED (the api layer owns that: dirs trim trailing slashes, repo
// locators lowercase, refs trimmed) — the store stores what it is given.
func (s PG) UpsertSource(ctx context.Context, src types.Source) (types.Source, error) {
	q := `
		INSERT INTO sources (` + sourceCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (kind, locator, ref) DO UPDATE SET updated_at = now()
		RETURNING ` + sourceCols
	return scanSource(s.Pool.QueryRow(ctx, q,
		src.ID, string(src.Kind), src.Locator, src.Ref, src.Name,
		sourceRequirementsParam(src.Requirements), src.Profile, string(src.Status),
		src.ActiveRunID, src.CreatedAt, src.UpdatedAt,
	))
}

// GetSource returns the source for id, or ErrNotFound.
func (s PG) GetSource(ctx context.Context, id uuid.UUID) (types.Source, error) {
	return scanSource(s.Pool.QueryRow(ctx, `SELECT `+sourceCols+` FROM sources WHERE id = $1`, id))
}

// GetSourcesByIDs returns the sources for ids in ONE query, keyed by id — the
// hydrate pass's bulk read. Missing ids are simply absent from the map (a
// dangling attachment contributes nothing; the fold and the mount gate each
// handle that in their own register).
func (s PG) GetSourcesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.Source, error) {
	out := make(map[uuid.UUID]types.Source, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	srcs, err := collect(ctx, s.Pool, "list", "sources by id",
		`SELECT `+sourceCols+` FROM sources WHERE id = ANY($1)`, []any{ids}, scanSource)
	if err != nil {
		return nil, err
	}
	for _, src := range srcs {
		out[src.ID] = src
	}
	return out, nil
}

// ListSources returns the whole library, newest first.
func (s PG) ListSources(ctx context.Context) ([]types.Source, error) {
	return collect(ctx, s.Pool, "list", "sources",
		`SELECT `+sourceCols+` FROM sources ORDER BY created_at DESC, id DESC`, nil, scanSource)
}

// UpdateSourceConfig replaces a source's operator-editable fields (name +
// its OWN requirements contract) — scoped, never touching the scan-owned
// columns, in the SetWorkspaceRequirements tradition.
func (s PG) UpdateSourceConfig(ctx context.Context, id uuid.UUID, name string, reqs map[string]types.WorkspaceRequirement) (types.Source, error) {
	return scanSource(s.Pool.QueryRow(ctx, `
		UPDATE sources SET name=$1, requirements=$2, updated_at=now()
		WHERE id=$3 RETURNING `+sourceCols,
		name, sourceRequirementsParam(reqs), id))
}

// WorkspacesAttaching returns the names of workspaces whose attachments
// reference source id — the loud half of delete-in-use. One GIN probe
// (workspaces_attachments_gin). A dangling attachment would silently un-mount
// code and turn runs into unexplained 422s at the mount gate, so DELETE
// refuses with these names rather than orphaning.
func (s PG) WorkspacesAttaching(ctx context.Context, id uuid.UUID) ([]string, error) {
	return collect(ctx, s.Pool, "list", "workspaces attaching", `
		SELECT name FROM workspaces
		WHERE attachments @> jsonb_build_array(jsonb_build_object('source_id', $1::text))
		ORDER BY name`, []any{id.String()}, scanName)
}

// scanName scans a single "name" column — the shape of the two
// delete-in-use existence checks (WorkspacesAttaching, WorkspacesUsingBaseImage).
func scanName(row pgx.Row) (string, error) {
	var n string
	err := row.Scan(&n)
	return n, err
}

// DeleteSource removes a library source. detach=true first strips every
// workspace attachment referencing it (the ?force=1 escape); detach=false and
// the caller must have already checked WorkspacesAttaching (the handler 409s
// with the names — the store just enforces nothing dangles silently).
func (s PG) DeleteSource(ctx context.Context, id uuid.UUID, detach bool) error {
	if detach {
		if _, err := s.Pool.Exec(ctx, `
			UPDATE workspaces
			SET attachments = COALESCE((
				SELECT jsonb_agg(e) FROM jsonb_array_elements(attachments) e
				WHERE e->>'source_id' IS DISTINCT FROM $1
			), '[]'::jsonb), updated_at = now()
			WHERE attachments @> jsonb_build_array(jsonb_build_object('source_id', $1::text))`,
			id.String()); err != nil {
			return fmt.Errorf("store: detach source: %w", err)
		}
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM sources WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("store: delete source: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimSourceActiveRun fences a source's in-flight scan run — the exact job
// ClaimWorkspaceActiveRun did for whole-workspace scans before the retarget.
// Returns ErrConflict when another run already holds the slot.
func (s PG) ClaimSourceActiveRun(ctx context.Context, id, runID uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE sources SET active_run_id=$2, status='scanning', updated_at=now()
		WHERE id=$1 AND active_run_id IS NULL`, id, runID)
	if err != nil {
		return fmt.Errorf("store: claim source run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// ClearSourceActiveRun releases the fence (idempotent; only the named run's
// claim is cleared, so a stale clear can't stomp a newer claim).
func (s PG) ClearSourceActiveRun(ctx context.Context, id, runID uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE sources SET active_run_id=NULL, updated_at=now()
		WHERE id=$1 AND active_run_id=$2`, id, runID)
	if err != nil {
		return fmt.Errorf("store: clear source run: %w", err)
	}
	return nil
}

// SetSourceScanResult persists a scan outcome FENCED on the claiming run —
// SetWorkspaceScanResult's shape one table over: only the run that holds
// active_run_id may write, so a stale upload from a superseded run can never
// clobber a fresher result. `seed` is the scan's requirement discovery for
// the SOURCE's OWN contract, applied fill-missing-only in the same statement:
// jsonb `||` lets the RIGHT side win, so an existing row — an operator's
// edit, or an earlier scan's — is never overwritten by a re-scan.
func (s PG) SetSourceScanResult(ctx context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, runID uuid.UUID, seed map[string]types.WorkspaceRequirement) (types.Source, error) {
	return scanSource(s.Pool.QueryRow(ctx, `
		UPDATE sources
		SET profile=$1, status=$2, requirements=COALESCE($5::jsonb, '{}'::jsonb) || COALESCE(requirements, '{}'::jsonb),
		    active_run_id=NULL, updated_at=now()
		WHERE id=$3 AND active_run_id=$4
		RETURNING `+sourceCols,
		profile, string(status), id, runID, sourceRequirementsParam(seed)))
}

// SetSourceScanResultUnfenced persists a SYNCHRONOUS (inline local_dir) scan,
// which never claimed a run slot — there is no concurrent writer to fence
// against on that path, exactly as the workspace inline scan wrote directly.
// Same fill-missing-only seed semantics as the fenced writer.
func (s PG) SetSourceScanResultUnfenced(ctx context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, seed map[string]types.WorkspaceRequirement) (types.Source, error) {
	return scanSource(s.Pool.QueryRow(ctx, `
		UPDATE sources
		SET profile=$1, status=$2, requirements=COALESCE($4::jsonb, '{}'::jsonb) || COALESCE(requirements, '{}'::jsonb),
		    updated_at=now()
		WHERE id=$3 RETURNING `+sourceCols,
		profile, string(status), id, sourceRequirementsParam(seed)))
}

// ─── Base images (tier 2) ───────────────────────────────────────────────────

const baseImageCols = `id, kind, name, image, steps, created_at, updated_at`

func scanBaseImage(row pgx.Row) (types.BaseImageEntry, error) {
	var b types.BaseImageEntry
	var stepsRaw []byte
	err := row.Scan(&b.ID, &b.Kind, &b.Name, &b.Image, &stepsRaw, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.BaseImageEntry{}, ErrNotFound
	}
	if err != nil {
		return types.BaseImageEntry{}, fmt.Errorf("store: scan base image: %w", err)
	}
	if stepsRaw != nil {
		_ = json.Unmarshal(stepsRaw, &b.Steps)
	}
	return b, nil
}

// UpsertBaseImage inserts a catalog image or returns the row with the same
// identity (kind, image, steps) — the identity index is the dedupe rule. The
// CHECK constraint refuses 'recommended' structurally: that build is derived
// per-workspace and has no catalog identity.
func (s PG) UpsertBaseImage(ctx context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error) {
	var steps []byte
	if len(b.Steps) > 0 {
		steps, _ = json.Marshal(b.Steps)
	}
	q := `
		INSERT INTO base_images (` + baseImageCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (kind, image, (COALESCE(steps, '[]'::jsonb))) DO UPDATE SET updated_at = now()
		RETURNING ` + baseImageCols
	return scanBaseImage(s.Pool.QueryRow(ctx, q,
		b.ID, b.Kind, b.Name, b.Image, steps, b.CreatedAt, b.UpdatedAt))
}

// GetBaseImage returns the catalog row for id, or ErrNotFound.
func (s PG) GetBaseImage(ctx context.Context, id uuid.UUID) (types.BaseImageEntry, error) {
	return scanBaseImage(s.Pool.QueryRow(ctx, `SELECT `+baseImageCols+` FROM base_images WHERE id=$1`, id))
}

// ListBaseImages returns the whole catalog, newest first.
func (s PG) ListBaseImages(ctx context.Context) ([]types.BaseImageEntry, error) {
	return collect(ctx, s.Pool, "list", "base images",
		`SELECT `+baseImageCols+` FROM base_images ORDER BY created_at DESC, id DESC`, nil, scanBaseImage)
}

// WorkspacesUsingBaseImage is the catalog's delete-in-use check.
func (s PG) WorkspacesUsingBaseImage(ctx context.Context, id uuid.UUID) ([]string, error) {
	return collect(ctx, s.Pool, "list", "workspaces using base image",
		`SELECT name FROM workspaces WHERE base_image_id=$1 ORDER BY name`, []any{id}, scanName)
}

// DeleteBaseImage removes a catalog row; detach=true first drops every
// workspace reference (those workspaces fall back to the derived recommended
// build — NULL is the marker, so "detach" is honest, not destructive).
func (s PG) DeleteBaseImage(ctx context.Context, id uuid.UUID, detach bool) error {
	if detach {
		if _, err := s.Pool.Exec(ctx,
			`UPDATE workspaces SET base_image_id=NULL, updated_at=now() WHERE base_image_id=$1`, id); err != nil {
			return fmt.Errorf("store: detach base image: %w", err)
		}
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM base_images WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("store: delete base image: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ─── The verify-approve merge writer ─────────────────────────────────────────

// MergeWorkspaceRequirements ADDS rows to a workspace's requirements overlay
// atomically — the `jsonb ||` idiom SetWorkspaceRecordResult established — so
// the verify loop's approve-writes-the-row-now can never clobber a concurrent
// edit the way read-modify-write on the full-replace writer would. The WHERE
// clause carries the same key-count cap the PUT endpoint enforces, so a
// runaway session can't grow the contract unboundedly; at the cap it returns
// ErrConflict rather than silently dropping the row.
func (s PG) MergeWorkspaceRequirements(ctx context.Context, id uuid.UUID, add map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	if len(add) == 0 {
		return s.GetWorkspace(ctx, id)
	}
	patch, err := json.Marshal(add)
	if err != nil {
		return types.Workspace{}, fmt.Errorf("store: marshal requirements patch: %w", err)
	}
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx, `
		UPDATE workspaces
		SET requirements = COALESCE(requirements, '{}'::jsonb) || $2::jsonb, updated_at = now()
		WHERE id = $1
		  AND (SELECT count(*) FROM jsonb_object_keys(COALESCE(requirements, '{}'::jsonb))) < 256
		RETURNING `+wsCols, id, patch))
	if errors.Is(err, ErrNotFound) {
		// Distinguish "no such workspace" from "cap reached" for the caller.
		if _, gerr := s.GetWorkspace(ctx, id); gerr == nil {
			return types.Workspace{}, ErrConflict
		}
		return types.Workspace{}, ErrNotFound
	}
	if err != nil {
		return types.Workspace{}, err
	}
	return s.hydrated(ctx, ws)
}

// ─── Hydration: attachments → the derived view ──────────────────────────────

// hydrated materializes a workspace's derived read-only view. For a row whose
// composition lives in ATTACHMENTS (post-split), Sources/BaseImage/Profile/
// Status are computed from the attached source rows and the catalog; for a
// pre-split row (attachments empty, embedded sources column still the truth)
// everything passes through EXACTLY as stored — byte-identical behavior, the
// migration's guarantee. EffectiveRequirements is folded in BOTH cases (the
// zero-source fold is the overlay identity).
func (s PG) hydrated(ctx context.Context, ws types.Workspace) (types.Workspace, error) {
	out, err := s.hydrateAll(ctx, []types.Workspace{ws})
	if err != nil {
		return types.Workspace{}, err
	}
	return out[0], nil
}

// hydrateAll is the bulk form: ONE sources query and ONE base-images query for
// the union across every workspace — referencedWorkspaces already full-lists
// workspaces on run-create/preflight, so per-row queries would multiply a hot
// path. ponytail: recomputed per read; a profile_cache column keyed on source
// versions is the upgrade path if merges ever show hot.
func (s PG) hydrateAll(ctx context.Context, wss []types.Workspace) ([]types.Workspace, error) {
	// Union the ids.
	sourceIDs := map[uuid.UUID]struct{}{}
	imageIDs := map[uuid.UUID]struct{}{}
	for i := range wss {
		for _, att := range wss[i].Attachments {
			if att.SourceID != nil {
				sourceIDs[*att.SourceID] = struct{}{}
			}
		}
		if wss[i].BaseImageID != nil {
			imageIDs[*wss[i].BaseImageID] = struct{}{}
		}
	}
	var sources map[uuid.UUID]types.Source
	if len(sourceIDs) > 0 {
		ids := make([]uuid.UUID, 0, len(sourceIDs))
		for id := range sourceIDs {
			ids = append(ids, id)
		}
		var err error
		if sources, err = s.GetSourcesByIDs(ctx, ids); err != nil {
			return nil, err
		}
	}
	images := map[uuid.UUID]types.BaseImageEntry{}
	for id := range imageIDs {
		img, err := s.GetBaseImage(ctx, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		} else if err == nil {
			images[id] = img
		}
	}

	for i := range wss {
		hydrateWorkspace(&wss[i], sources, images)
	}
	return wss, nil
}

// hydrateWorkspace derives one workspace's view in place. Pure over its inputs.
func hydrateWorkspace(ws *types.Workspace, sources map[uuid.UUID]types.Source, images map[uuid.UUID]types.BaseImageEntry) {
	// The fold runs for EVERY row: pre-split rows get the overlay identity.
	ws.EffectiveRequirements = types.FoldWorkspaceContract(ws.Attachments, sources, ws.Requirements)

	if len(ws.Attachments) == 0 {
		// Pre-split row: the embedded columns are still the truth; the derived
		// mirrors keep rendering it for single-source consumers.
		deriveWorkspaceMirrors(ws)
		return
	}

	// Sources view, in attachment order.
	derived := make([]types.WorkspaceSource, 0, len(ws.Attachments))
	profiles := make([]workspacescan.WorkspaceProfile, 0, len(ws.Attachments))
	status := types.WorkspaceScanned // ephemeral-only ⇒ scanned (nothing to scan)
	sawSource := false
	for _, att := range ws.Attachments {
		if att.Ephemeral || att.SourceID == nil {
			derived = append(derived, types.WorkspaceSource{
				Type: types.WorkspaceSourceTypeEphemeral, Target: att.Target,
			})
			continue
		}
		src, found := sources[*att.SourceID]
		if !found {
			// Dangling: contribute nothing here; the mount gate is where this
			// surfaces loudly. Status reads error — a run against it WILL fail.
			status = types.WorkspaceError
			continue
		}
		sawSource = true
		switch src.Kind {
		case types.SourceLocalDir:
			derived = append(derived, types.WorkspaceSource{
				Type: types.WorkspaceSourceTypeLocalDir, Path: src.Locator,
				Target: att.Target, Writable: att.Writable,
			})
		case types.SourceRepo:
			derived = append(derived, types.WorkspaceSource{
				Type: types.WorkspaceSourceTypeRepo, Source: src.Locator,
				Ref: src.Ref, Target: att.Target,
			})
		}
		if len(src.Profile) > 0 {
			var p workspacescan.WorkspaceProfile
			if json.Unmarshal(src.Profile, &p) == nil {
				profiles = append(profiles, p)
			}
		}
		status = worseWorkspaceStatus(status, src.Status)
	}
	ws.Sources = derived

	// Profile: the merge of scanned attached sources — same field, same shape,
	// so list badges and pollers are none the wiser. An EPHEMERAL-ONLY
	// composition derives the deterministic empty profile (high confidence —
	// there is nothing ambiguous about "no source"), exactly what the legacy
	// scan stamped for it: nil here read as "no contract yet" and the wizard's
	// Requirements tabs never mounted on the default scratch-floor path.
	switch {
	case len(profiles) > 0:
		merged := workspacescan.MergeProfiles(profiles)
		if b, err := json.Marshal(merged); err == nil {
			ws.Profile = b
		}
	case !sawSource && status != types.WorkspaceError:
		empty := workspacescan.WorkspaceProfile{Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic}
		if b, err := json.Marshal(empty); err == nil {
			ws.Profile = b
		}
	default:
		ws.Profile = nil // real sources, none scanned yet
	}
	ws.Status = status

	// Base image: the catalog row when referenced; nil = recommended/derived.
	ws.BaseImage = nil
	if ws.BaseImageID != nil {
		if img, found := images[*ws.BaseImageID]; found {
			ws.BaseImage = &types.WorkspaceBaseImage{Kind: img.Kind, Image: img.Image, Steps: img.Steps}
		}
	}
	deriveWorkspaceMirrors(ws)
}

// statusRank orders the scan lifecycle for worseWorkspaceStatus: error >
// scanning > pending_scan > scanned.
var statusRank = map[types.WorkspaceStatus]int{
	types.WorkspaceError: 3, types.WorkspaceScanning: 2,
	types.WorkspacePendingScan: 1, types.WorkspaceScanned: 0,
}

// worseWorkspaceStatus orders the scan lifecycle: error > scanning >
// pending_scan > scanned. "Worse" wins so a workspace never reads readier
// than its least-ready attached source.
func worseWorkspaceStatus(a, b types.WorkspaceStatus) types.WorkspaceStatus {
	if statusRank[b] > statusRank[a] {
		return b
	}
	return a
}
