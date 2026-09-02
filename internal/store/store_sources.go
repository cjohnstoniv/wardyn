// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// sourceRequirementsParam marshals a requirements map for a jsonb param. A nil
// map (a failed scan, or "no seed to apply") becomes SQL NULL — callers
// COALESCE nil back to '{}' in the SQL, and the scan-seed CASE below reads NULL
// as "leave requirements untouched". A non-nil map, empty or not, marshals to
// real JSON ("{}" for empty) so a scan that legitimately finds nothing still
// REBUILDS the scan_seeded subset instead of reading as a failed scan. Shared
// by the explicit-requirements writers below and the scan-seed fill (seed and a
// source's own requirements are the same concrete type).
func sourceRequirementsParam(m map[string]types.WorkspaceRequirement) []byte {
	b, _ := jsonOrNull(m, m == nil).([]byte)
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
		sourceRequirementsParam(src.Requirements), workspaceProfileParam(src.Profile), string(src.Status),
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
// (workspaces_attachments_gin). A dangling attachment would silently narrow a
// workspace to its remaining sources (no error, no mount-gate check for a
// source that used to be there), so DELETE refuses with these names rather
// than orphaning silently.
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

// workspacesOrphanedBySource returns the names of workspaces whose
// attachments consist ENTIRELY of source id — i.e. detaching id (DeleteSource
// with detach=true) would empty attachments to '[]'. hydrateWorkspace reads
// an empty attachments array as the PRE-SPLIT marker and falls back to the
// workspace's stale legacy `sources` column (store.go's
// workspaceAttachmentsParam doc), so the deleted source would silently
// reappear in the API/UI and the run mount gate would still admit it (STORE-1)
// — a workspace with an ephemeral attachment alongside this source is NOT
// orphaned (attachments stays non-empty), so it is excluded.
func (s PG) workspacesOrphanedBySource(ctx context.Context, id uuid.UUID) ([]string, error) {
	return collect(ctx, s.Pool, "list", "workspaces orphaned by source detach", `
		SELECT name FROM workspaces
		WHERE attachments @> jsonb_build_array(jsonb_build_object('source_id', $1::text))
		  AND NOT EXISTS (
		    SELECT 1 FROM jsonb_array_elements(attachments) e
		    WHERE e->>'source_id' IS DISTINCT FROM $1::text
		  )
		ORDER BY name`, []any{id.String()}, scanName)
}

// DeleteSource removes a library source. detach=true first strips every
// workspace attachment referencing it (the ?force=1 escape) — UNLESS doing so
// would leave a workspace with zero attachments (workspacesOrphanedBySource):
// 0029 makes a workspace a composition of one-or-more sources, and
// decodeWorkspaceRequest already guarantees every write keeps that true, so
// '[]' must stay unreachable (STORE-1). That check and the detach are two
// statements — narrows the window against a workspace attaching uniquely to
// this source between them, same as detach=false's own residual race below;
// neither closes it (STORE-2's finding on this exact file: READ COMMITTED
// against two independently-written tables can narrow a TOCTOU to one
// statement, never fully close it without an explicit lock on both sides).
func (s PG) DeleteSource(ctx context.Context, id uuid.UUID, detach bool) error {
	if detach {
		orphaned, err := s.workspacesOrphanedBySource(ctx, id)
		if err != nil {
			return fmt.Errorf("store: check source detach: %w", err)
		}
		if len(orphaned) > 0 {
			return fmt.Errorf("%w: force-deleting would leave workspace(s) with no attachments: %s",
				ErrConflict, strings.Join(orphaned, ", "))
		}
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
		tag, err := s.Pool.Exec(ctx, `DELETE FROM sources WHERE id=$1`, id)
		if err != nil {
			return fmt.Errorf("store: delete source: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	}
	// Non-force: NOT EXISTS is evaluated atomically WITH the DELETE, narrowing
	// — not closing (STORE-2) — the window against a concurrent attach: one
	// that already COMMITTED by the time this statement runs is always seen
	// (a caller's own WorkspacesAttaching probe can't be beaten by an attach
	// that lands and commits after it), but READ COMMITTED does not block on
	// a STILL-OPEN attach transaction writing the unrelated `workspaces`
	// table, so that one interleaving survives. Zero rows affected means
	// either "still attached" or "no such id" — the DELETE has already
	// refused either way, so a follow-up existence probe cannot reopen the
	// TOCTOU; it only picks which honest error to report (a genuinely-absent
	// id must still 404, not 409).
	tag, err := s.Pool.Exec(ctx, `
		DELETE FROM sources WHERE id=$1 AND NOT EXISTS (
			SELECT 1 FROM workspaces
			WHERE attachments @> jsonb_build_array(jsonb_build_object('source_id', $1::text))
		)`, id)
	if err != nil {
		return fmt.Errorf("store: delete source: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if perr := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sources WHERE id=$1)`, id).Scan(&exists); perr != nil {
			return fmt.Errorf("store: probe source existence: %w", perr)
		}
		if !exists {
			return ErrNotFound
		}
		return ErrConflict
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

// sourceScanRebuild is the provenance-aware requirements REBUILD both writers
// below apply, named once because it is the half that must never drift between
// them: $4 is the scan seed. NULL (a failed scan) leaves the contract
// untouched; otherwise the seed replaces the scan_seeded subset outright — so a
// name a rescan no longer finds is DROPPED — while every non-scan_seeded row an
// operator set survives, because jsonb_object_agg gathers those and the seed is
// concatenated onto their LEFT. $4 is the seed in both queries so this fragment
// reads identically either way.
const sourceScanRebuild = `requirements = CASE WHEN $4::jsonb IS NULL THEN requirements ELSE $4::jsonb || COALESCE(
		    (SELECT jsonb_object_agg(k, v) FROM jsonb_each(COALESCE(requirements,'{}'::jsonb)) e(k,v)
		      WHERE e.v->>'provenance' IS DISTINCT FROM 'scan_seeded'), '{}'::jsonb) END`

// The two scan-result writes: ONE static literal per path, selected in Go
// rather than by interpolating the fence into SQL — the same rule
// qAddApprovedEgressDecision / qAddDeniedEgressDecision keep, and for the same
// reason: each stays a single auditable query a reader can grep whole.
//
// The fence differs TWICE, which is why these are not one query with an
// optional clause: the fenced write is gated on the claim ($5) and also
// RELEASES it (active_run_id=NULL), while the unfenced path never claimed a
// slot, so it has no claim to check and must not clear one it never took.
const qSetSourceScanResultFenced = `
		UPDATE sources
		SET profile=$1, status=$2, ` + sourceScanRebuild + `,
		    active_run_id=NULL, updated_at=now()
		WHERE id=$3 AND active_run_id=$5
		RETURNING ` + sourceCols

const qSetSourceScanResultUnfenced = `
		UPDATE sources
		SET profile=$1, status=$2, ` + sourceScanRebuild + `,
		    updated_at=now()
		WHERE id=$3
		RETURNING ` + sourceCols

// SetSourceScanResult persists a scan outcome FENCED on the claiming run:
// only the run that holds active_run_id may write, so a stale upload from a
// superseded run can never clobber a fresher result. `seed` is the scan's
// requirement discovery for the SOURCE's OWN contract, applied as a
// provenance-aware REBUILD in the same statement: seed replaces the
// scan_seeded subset of requirements outright (so a name a rescan no longer
// finds is DROPPED, not stuck forever); non-scan_seeded rows — an operator's
// own edit — always win regardless of seed, because they land on the RIGHT
// side of jsonb `||`; a NULL seed (failed scan — workspace_run.go's
// launch-failure path passes nil) leaves the contract untouched. A non-nil but
// EMPTY seed (a rescan that legitimately finds nothing) still rebuilds: it
// marshals to '{}', not NULL — see sourceRequirementsParam.
func (s PG) SetSourceScanResult(ctx context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, runID uuid.UUID, seed map[string]types.WorkspaceRequirement) (types.Source, error) {
	return scanSource(s.Pool.QueryRow(ctx, qSetSourceScanResultFenced,
		profile, string(status), id, sourceRequirementsParam(seed), runID))
}

// SetSourceScanResultUnfenced persists a SYNCHRONOUS (inline local_dir) scan,
// which never claimed a run slot — there is no concurrent writer to fence
// against on that path, exactly as the workspace inline scan wrote directly.
// Same provenance-aware rebuild semantics as the fenced writer: rebuilds the
// scan_seeded subset; non-scan_seeded rows still win; NULL seed (failed scan)
// leaves the contract untouched.
func (s PG) SetSourceScanResultUnfenced(ctx context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, seed map[string]types.WorkspaceRequirement) (types.Source, error) {
	return scanSource(s.Pool.QueryRow(ctx, qSetSourceScanResultUnfenced,
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
//
// An identity hit does NOT touch name (same as UpsertSource's conflict
// clause below) — on purpose. This upsert is also the PASSTHROUGH path a
// workspace/run resolves its declared base-image spec through (sources.go's
// attachSourcesAndBaseImage-shaped callers), which always derives an
// auto-placeholder name (lastPathSegment(image)) with no rename intent
// whatsoever; if this conflict clause applied EXCLUDED.name unconditionally,
// every such passthrough call would silently rename an operator's
// custom-named catalog row back to that placeholder. See
// UpdateBaseImageName for the actual rename path (W7-S1-3).
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

// UpdateBaseImageName renames a catalog base-image row — the explicit, scoped
// rename path (W7-S1-3), mirroring UpdateSourceConfig above. handleCreateBaseImage
// is the only caller: on an identity hit where the REQUEST carried an explicit
// name (the Add dialog's re-POST-to-rename shape), never from UpsertBaseImage's
// own conflict clause, which passthrough callers share and must never let rename
// an operator's chosen name away from under them (see UpsertBaseImage's doc).
func (s PG) UpdateBaseImageName(ctx context.Context, id uuid.UUID, name string) (types.BaseImageEntry, error) {
	return scanBaseImage(s.Pool.QueryRow(ctx, `
		UPDATE base_images SET name=$1, updated_at=now()
		WHERE id=$2 RETURNING `+baseImageCols,
		name, id))
}

// GetBaseImage returns the catalog row for id, or ErrNotFound. NOT on the Store
// interface: no handler reads one image by id (the console lists them), so
// requiring it of every implementation bought nothing. Kept as a PG method
// because store_hydrate_pg_test.go round-trips through it.
func (s PG) GetBaseImage(ctx context.Context, id uuid.UUID) (types.BaseImageEntry, error) {
	return scanBaseImage(s.Pool.QueryRow(ctx, `SELECT `+baseImageCols+` FROM base_images WHERE id=$1`, id))
}

// GetBaseImagesByIDs is hydrateAll's own batched read, not a Store-interface
// method: its only caller is inside this package.
//
// GetBaseImagesByIDs returns the base images for ids in ONE query, keyed by
// id — hydrateAll's bulk read, mirroring GetSourcesByIDs. Missing ids are
// simply absent from the map (a dangling base_image_id contributes nothing;
// hydrateWorkspace leaves BaseImage nil for it).
func (s PG) GetBaseImagesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.BaseImageEntry, error) {
	out := make(map[uuid.UUID]types.BaseImageEntry, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	imgs, err := collect(ctx, s.Pool, "list", "base images by id",
		`SELECT `+baseImageCols+` FROM base_images WHERE id = ANY($1)`, []any{ids}, scanBaseImage)
	if err != nil {
		return nil, err
	}
	for _, img := range imgs {
		out[img.ID] = img
	}
	return out, nil
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
// detach=false narrows the in-use check and the delete to ONE statement —
// mirrors DeleteSource, including its residual race (STORE-2): a concurrent
// workspace UPDATE committing base_image_id=id between this statement's own
// NOT EXISTS check and its commit is caught by the base_image_id FK
// (0031:61) instead, surfacing as a raw 23503 — mapped to ErrConflict below
// so that loses race still answers a clean 409, not a raw Postgres 500.
func (s PG) DeleteBaseImage(ctx context.Context, id uuid.UUID, detach bool) error {
	if detach {
		if _, err := s.Pool.Exec(ctx,
			`UPDATE workspaces SET base_image_id=NULL, updated_at=now() WHERE base_image_id=$1`, id); err != nil {
			return fmt.Errorf("store: detach base image: %w", err)
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
	// Non-force: NOT EXISTS is evaluated atomically WITH the DELETE, the same
	// narrowed (not closed — STORE-2) guarantee DeleteSource makes. Zero rows
	// affected means either "still in use" or "no such id" — the DELETE has
	// already refused either way, so a follow-up existence probe cannot
	// reopen the TOCTOU; it only picks which honest error to report (a
	// genuinely-absent id must still 404, not 409).
	tag, err := s.Pool.Exec(ctx, `
		DELETE FROM base_images WHERE id=$1 AND NOT EXISTS (
			SELECT 1 FROM workspaces WHERE base_image_id=$1
		)`, id)
	if err != nil {
		// The residual race the doc above admits: a concurrent workspace
		// UPDATE committing base_image_id=id between this statement's own
		// NOT EXISTS check and its commit is caught by the FK (0031:61)
		// instead of NOT EXISTS — map that to the SAME clean 409 the
		// zero-rows branch below gives, not a raw Postgres 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ErrConflict
		}
		return fmt.Errorf("store: delete base image: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if perr := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM base_images WHERE id=$1)`, id).Scan(&exists); perr != nil {
			return fmt.Errorf("store: probe base image existence: %w", perr)
		}
		if !exists {
			return ErrNotFound
		}
		return ErrConflict
	}
	return nil
}

// ─── The verify-approve merge writer ─────────────────────────────────────────

// MergeWorkspaceRequirements ADDS rows to a workspace's requirements overlay
// atomically — the `jsonb ||` idiom SetWorkspaceRecordResult established — so
// the verify loop's approve-writes-the-row-now can never clobber a concurrent
// edit the way read-modify-write on the full-replace writer would. The WHERE
// clause carries the same key-count cap the PUT endpoint enforces, evaluated
// on the POST-merge total (existing keys merged with this patch) so a
// multi-key merge can't overshoot the cap in one jump; at the cap it returns
// ErrConflict rather than silently dropping rows or exceeding it.
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
		  AND (SELECT count(*) FROM jsonb_object_keys(COALESCE(requirements, '{}'::jsonb) || $2::jsonb)) <= 256
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
	var images map[uuid.UUID]types.BaseImageEntry
	if len(imageIDs) > 0 {
		ids := make([]uuid.UUID, 0, len(imageIDs))
		for id := range imageIDs {
			ids = append(ids, id)
		}
		var err error
		if images, err = s.GetBaseImagesByIDs(ctx, ids); err != nil {
			return nil, err
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
	identities := make([]string, 0, len(ws.Attachments)) // profiles[i]'s source locator, for MergeProfiles attribution
	status := types.WorkspaceScanned                     // ephemeral-only ⇒ scanned (nothing to scan)
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
				Target: att.Target, Writable: att.Writable, Overrides: att.Overrides,
			})
		case types.SourceRepo:
			derived = append(derived, types.WorkspaceSource{
				Type: types.WorkspaceSourceTypeRepo, Source: src.Locator,
				Ref: src.Ref, Target: att.Target, Overrides: att.Overrides,
			})
		}
		if len(src.Profile) > 0 {
			var p workspacescan.WorkspaceProfile
			if json.Unmarshal(src.Profile, &p) == nil {
				profiles = append(profiles, p)
				identities = append(identities, src.Locator)
			}
		}
		status = worseWorkspaceStatus(status, src.Status)
	}
	ws.Sources = derived

	// primaryIdentity names the source behind ws.Sources[0] — derived the SAME
	// way resolveWorkspaceImage (internal/api/workspace_run.go) reads "primary"
	// (primary.Sources[0]), so MergeProfiles attributes HasDevcontainer/
	// HasDockerfile from that exact attachment rather than merely the first one
	// that happened to scan (profiles[0]/identities[0] can name a DIFFERENT,
	// later attachment when the first is ephemeral or not yet scanned). Empty
	// when Sources[0] is ephemeral or there are no sources at all — matches no
	// identity, which MergeProfiles reads as "primary has no devcontainer".
	var primaryIdentity string
	if len(derived) > 0 {
		switch derived[0].Type {
		case types.WorkspaceSourceTypeLocalDir:
			primaryIdentity = derived[0].Path
		case types.WorkspaceSourceTypeRepo:
			primaryIdentity = derived[0].Source
		}
	}

	// Profile: the merge of scanned attached sources — same field, same shape,
	// so list badges and pollers are none the wiser. An EPHEMERAL-ONLY
	// composition derives the deterministic empty profile (high confidence —
	// there is nothing ambiguous about "no source"), exactly what the legacy
	// scan stamped for it: nil here read as "no contract yet" and the wizard's
	// Requirements tabs never mounted on the default scratch-floor path.
	switch {
	case len(profiles) > 0:
		merged := workspacescan.MergeProfiles(profiles, identities, primaryIdentity)
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
