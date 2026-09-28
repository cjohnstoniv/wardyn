// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The workspaces table (tier 3 of the three-tier split): JSONB parameter
// helpers, the wsCols column list, scanWorkspace, and the CRUD and
// scoped-writer methods PG exposes over a workspace row. store_sources.go is
// the read/hydrate half of the pair (tiers 1-2 plus s.hydrated).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// jsonOrNull is the shared body for the nullable-JSONB param helpers below:
// isEmpty ⇒ SQL NULL, otherwise the marshalled bytes (a marshal error also
// folds to NULL). NOT NULL columns (workspaceSourcesParam,
// workspaceAttachmentsParam) fail-safe to '[]' instead and skip this helper.
func jsonOrNull[T any](v T, isEmpty bool) any {
	if isEmpty {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// workspaceProfileParam binds the nullable `profile` JSONB column: an empty
// RawMessage inserts SQL NULL (not yet scanned), never literal JSON "null".
func workspaceProfileParam(p json.RawMessage) any {
	if len(p) == 0 {
		return nil
	}
	return []byte(p)
}

// workspaceApprovedParam serializes the approved-egress list; empty ⇒ NULL.
func workspaceApprovedParam(domains []string) any {
	return jsonOrNull(domains, len(domains) == 0)
}

// workspaceDeniedParam mirrors workspaceApprovedParam for denied-egress.
func workspaceDeniedParam(domains []string) any {
	return jsonOrNull(domains, len(domains) == 0)
}

// workspaceLLMCredParam serializes the model/harness cred binding; nil ⇒ NULL.
func workspaceLLMCredParam(c *types.WorkspaceLLMCred) any {
	return jsonOrNull(c, c == nil)
}

// workspaceSourcesParam serializes for the NOT NULL sources JSONB column:
// unlike the other helpers here, it never returns nil — a nil/empty slice
// still marshals to "[]" (migration 0029).
func workspaceSourcesParam(sources []types.WorkspaceSource) any {
	if sources == nil {
		sources = []types.WorkspaceSource{}
	}
	b, err := json.Marshal(sources)
	if err != nil {
		return []byte("[]") // unreachable for this concrete type; fail-safe
	}
	return b
}

// workspaceBaseImageParam serializes the base-image choice; nil ⇒ NULL.
func workspaceBaseImageParam(img *types.WorkspaceBaseImage) any {
	return jsonOrNull(img, img == nil)
}

// workspaceRequirementsParam serializes the requirements contract; empty/nil ⇒ NULL.
func workspaceRequirementsParam(reqs map[string]types.WorkspaceRequirement) any {
	return jsonOrNull(reqs, len(reqs) == 0)
}

// workspaceAttachmentsParam marshals attachments for storage. Empty writes
// '[]' (NOT NULL, migration 0031) — the pre-split marker the hydrate pass
// falls back on exactly as it would for NULL.
func workspaceAttachmentsParam(atts []types.WorkspaceAttachment) []byte {
	if len(atts) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(atts)
	if err != nil {
		return []byte("[]") // unreachable; fail-safe to the pre-split marker
	}
	return b
}

// wsCols is the canonical workspace column list (order matches scanWorkspace).
// New columns are always APPENDED, never interleaved — this feeds both the
// INSERT list and every RETURNING/SELECT site, and appending is the only edit
// that cannot silently transpose two same-typed columns. owned_by and
// denied_egress are also deliberately absent from UpdateWorkspace's SET
// clause: both survive a composition edit rather than being overwritten.
const wsCols = `id, name, sources, base_image, requirements, profile, image_ref, ` +
	`built_profile_hash, approved_egress, active_run_id, status, created_at, updated_at, ` +
	`record_results, llm_cred, attachments, base_image_id, denied_egress, owned_by, ` +
	`egress_edited_at`

// CreateWorkspace inserts an onboarded workspace and returns the persisted row.
func (s PG) CreateWorkspace(ctx context.Context, ws types.Workspace) (types.Workspace, error) {
	q := `
		INSERT INTO workspaces (` + wsCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING ` + wsCols
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx, q,
		ws.ID, ws.Name, workspaceSourcesParam(ws.Sources), workspaceBaseImageParam(ws.BaseImage),
		workspaceRequirementsParam(ws.Requirements), workspaceProfileParam(ws.Profile), ws.ImageRef,
		ws.BuiltProfileHash, workspaceApprovedParam(ws.ApprovedEgress), ws.ActiveRunID,
		string(ws.Status), ws.CreatedAt, ws.UpdatedAt, workspaceProfileParam(ws.RecordResults),
		workspaceLLMCredParam(ws.LLMCred), workspaceAttachmentsParam(ws.Attachments), ws.BaseImageID,
		workspaceDeniedParam(ws.DeniedEgress), ws.OwnedBy, ws.EgressEditedAt,
	))
}

// GetWorkspace returns the workspace for id, or ErrNotFound.
func (s PG) GetWorkspace(ctx context.Context, id uuid.UUID) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx, `SELECT `+wsCols+` FROM workspaces WHERE id = $1`, id))
}

// ListWorkspaces returns all workspaces in reverse creation order. The slice
// is empty (never nil) when no workspaces exist.
func (s PG) ListWorkspaces(ctx context.Context) ([]types.Workspace, error) {
	return s.ListWorkspacesPage(ctx, Page{})
}

// UpdateWorkspace replaces a workspace's editable composition via a
// FULL-column write of every scan-owned field (profile, image_ref,
// built_profile_hash, status, etc.), WITH ONE DELIBERATE EXCEPTION:
// denied_egress is NOT in this SET clause (see Workspace.DeniedEgress) — a
// permanent deny survives a composition edit, unlike ApprovedEgress. Callers
// must round-trip the fetched row. Returns ErrNotFound for an unknown id.
//
// egress_edited_at is passed through unless stampEgressEdit marks this write
// as the operator action the column records: unconditional stamping would
// suppress the boot heal for a decision nobody undid, while plain omission
// let this writer clear approved_egress without moving the stamp, so
// ReconcileWorkspaceEgressDecisions re-widened the list on restart. The
// stamp is now() from the database, not wardynd's clock, since it's compared
// against approvals.decided_at and clock skew there fails OPEN.
func (s PG) UpdateWorkspace(ctx context.Context, id uuid.UUID, ws types.Workspace, stampEgressEdit bool) (types.Workspace, error) {
	q := `
		UPDATE workspaces
		SET name=$1, sources=$2, base_image=$3, requirements=$4,
			profile=$5, image_ref=$6, built_profile_hash=$7, approved_egress=$8,
			active_run_id=$9, status=$10, record_results=$11, llm_cred=$12,
			attachments=$13, base_image_id=$14,
			egress_edited_at=CASE WHEN $15::bool THEN now() ELSE $16::timestamptz END,
			updated_at=now()
		WHERE id=$17
		RETURNING ` + wsCols
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx, q,
		ws.Name, workspaceSourcesParam(ws.Sources), workspaceBaseImageParam(ws.BaseImage),
		workspaceRequirementsParam(ws.Requirements), workspaceProfileParam(ws.Profile), ws.ImageRef,
		ws.BuiltProfileHash, workspaceApprovedParam(ws.ApprovedEgress), ws.ActiveRunID,
		string(ws.Status), workspaceProfileParam(ws.RecordResults), workspaceLLMCredParam(ws.LLMCred),
		workspaceAttachmentsParam(ws.Attachments), ws.BaseImageID, stampEgressEdit, ws.EgressEditedAt, id,
	))
}

// SetWorkspaceLLMCred replaces ONLY the model/harness cred binding column
// (plus updated_at), scoped so it can never clobber a concurrently-persisted
// async scan. Pass nil to clear the binding.
func (s PG) SetWorkspaceLLMCred(ctx context.Context, id uuid.UUID, cred *types.WorkspaceLLMCred) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET llm_cred=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceLLMCredParam(cred), id))
}

// SetWorkspaceOwner replaces ONLY the owned_by column (plus updated_at) — the
// offboarding reassign (decision O6). Deliberately the ONLY writer of the
// column after CreateWorkspace: UpdateWorkspace omits owned_by, so an
// ordinary edit can never move ownership.
func (s PG) SetWorkspaceOwner(ctx context.Context, id uuid.UUID, owner string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET owned_by=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		owner, id))
}

// SetWorkspaceApprovedEgress replaces ONLY the approved-egress column (plus
// updated_at and egress_edited_at), scoped so it can never clobber a
// concurrently-persisted async scan. egress_edited_at always bumps, even for
// an identical list, since ReconcileWorkspaceEgressDecisions treats it as
// "the operator confirmed this at this moment".
func (s PG) SetWorkspaceApprovedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET approved_egress=$1, egress_edited_at=now(), updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceApprovedParam(domains), id))
}

// qAddApprovedEgressDecision and qAddDeniedEgressDecision back
// AddWorkspaceEgressDecision, one static query per direction (chosen in Go
// by `allow`, never interpolated) so each stays a single auditable literal.
// Each appends host to its own list (deduped), removes it from the opposite
// list (deny beats allow), and guards the cap against the target list only,
// ORed with "already present" so an idempotent re-decide on a full list
// never errors. Both effects land in ONE UPDATE, so a decision never half-applies.
const (
	qAddApprovedEgressDecision = `
		UPDATE workspaces
		SET approved_egress = CASE
				WHEN COALESCE(approved_egress,'[]'::jsonb) @> to_jsonb($2::text) THEN approved_egress
				ELSE COALESCE(approved_egress,'[]'::jsonb) || to_jsonb($2::text)
			END,
			denied_egress = COALESCE(denied_egress,'[]'::jsonb) - $2::text,
			updated_at = now()
		WHERE id = $1
		  AND (
			jsonb_array_length(COALESCE(approved_egress,'[]'::jsonb)) < $3
			OR COALESCE(approved_egress,'[]'::jsonb) @> to_jsonb($2::text)
		  )
		RETURNING ` + wsCols

	qAddDeniedEgressDecision = `
		UPDATE workspaces
		SET denied_egress = CASE
				WHEN COALESCE(denied_egress,'[]'::jsonb) @> to_jsonb($2::text) THEN denied_egress
				ELSE COALESCE(denied_egress,'[]'::jsonb) || to_jsonb($2::text)
			END,
			approved_egress = COALESCE(approved_egress,'[]'::jsonb) - $2::text,
			updated_at = now()
		WHERE id = $1
		  AND (
			jsonb_array_length(COALESCE(denied_egress,'[]'::jsonb)) < $3
			OR COALESCE(denied_egress,'[]'::jsonb) @> to_jsonb($2::text)
		  )
		RETURNING ` + wsCols
)

// AddWorkspaceEgressDecision records one `always`-scoped egress decision for
// host: on allow it is added to approved_egress (capped, deduped) and removed
// from denied_egress; on deny the mirror. Returns ErrConflict, not a bare
// "not found", when id exists but the cap guard refused the write.
func (s PG) AddWorkspaceEgressDecision(ctx context.Context, id uuid.UUID, host string, allow bool, maxApprovedEgress int) (types.Workspace, error) {
	q := qAddApprovedEgressDecision
	if !allow {
		q = qAddDeniedEgressDecision
	}
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx, q, id, host, maxApprovedEgress))
	ws, err = s.hydrateIfOK(ctx, ws, err)
	if errors.Is(err, ErrNotFound) {
		// Distinguish "no such workspace" from "cap reached".
		if _, gerr := s.GetWorkspace(ctx, id); gerr == nil {
			return types.Workspace{}, ErrConflict
		}
		return types.Workspace{}, ErrNotFound
	}
	if err != nil {
		return types.Workspace{}, err
	}
	return ws, nil
}

// SetWorkspaceDeniedEgress is denied_egress's mirror of
// SetWorkspaceApprovedEgress: same anti-clobber scoping, same full-list
// replace, same egress_edited_at stamp (removing a permanent DENY is an
// operator override the boot heal must not undo).
func (s PG) SetWorkspaceDeniedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET denied_egress=$1, egress_edited_at=now(), updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceDeniedParam(domains), id))
}

// SetWorkspaceRequirements replaces ONLY the requirements-contract column
// (plus updated_at), scoped like SetWorkspaceApprovedEgress. Pass the FULL
// desired map — this replaces rather than merges.
func (s PG) SetWorkspaceRequirements(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET requirements=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceRequirementsParam(reqs), id))
}

// SetWorkspaceRecordResult atomically upserts ONE task's entry in the Record
// Mode record_results map (jsonb merge, so concurrent writers of DIFFERENT
// tasks never lose each other's entries). When onlyIfStatus is non-empty the
// write is a compare-and-set against the task's stored status. Returns
// applied=false (no error) on a guard miss.
func (s PG) SetWorkspaceRecordResult(ctx context.Context, id uuid.UUID,
	taskKey string, result json.RawMessage, onlyIfStatus string) (types.Workspace, bool, error) {
	q := `UPDATE workspaces
		SET record_results = COALESCE(record_results,'{}'::jsonb) || jsonb_build_object($2::text, $3::jsonb),
			updated_at=now()
		WHERE id=$1`
	args := []any{id, taskKey, string(result)}
	if onlyIfStatus != "" {
		q += ` AND COALESCE(record_results->$2->>'status','') = $4`
		args = append(args, onlyIfStatus)
	}
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx, q+` RETURNING `+wsCols, args...))
	ws, err = s.hydrateIfOK(ctx, ws, err)
	if errors.Is(err, ErrNotFound) && onlyIfStatus != "" {
		// Distinguish guard-miss from a missing workspace.
		ws, gerr := s.GetWorkspace(ctx, id)
		if gerr != nil {
			return types.Workspace{}, false, gerr
		}
		return ws, false, nil
	}
	if err != nil {
		return types.Workspace{}, false, err
	}
	return ws, true, nil
}

// ClaimWorkspaceActiveRun compare-and-sets active_run_id from expected
// (possibly nil) to runID, the serial-import-step gate: of two concurrent
// launches racing the same slot, the loser gets applied=false and must NOT launch.
func (s PG) ClaimWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID, expected *uuid.UUID) (types.Workspace, bool, error) {
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET active_run_id=$2, updated_at=now()
		 WHERE id=$1 AND active_run_id IS NOT DISTINCT FROM $3 RETURNING `+wsCols,
		id, runID, expected))
	ws, err = s.hydrateIfOK(ctx, ws, err)
	if errors.Is(err, ErrNotFound) {
		ws, gerr := s.GetWorkspace(ctx, id)
		if gerr != nil {
			return types.Workspace{}, false, gerr
		}
		return ws, false, nil
	}
	if err != nil {
		return types.Workspace{}, false, err
	}
	return ws, true, nil
}

// ClearWorkspaceActiveRun clears active_run_id ONLY while it still points at
// runID, so a terminal run's cleanup can never clobber a step that was
// concurrently launched and now owns the pointer.
func (s PG) ClearWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE workspaces SET active_run_id=NULL, updated_at=now() WHERE id=$1 AND active_run_id=$2`,
		id, runID)
	if err != nil {
		return false, fmt.Errorf("store: clear active run: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetWorkspaceBuiltImage scoped-writes ONLY the image cache columns
// (build-once/reuse-many), same anti-clobber discipline as the other scoped
// writers.
func (s PG) SetWorkspaceBuiltImage(ctx context.Context, id uuid.UUID, imageRef, builtHash string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET image_ref=$1, built_profile_hash=$2, updated_at=now() WHERE id=$3 RETURNING `+wsCols,
		imageRef, builtHash, id))
}

// SetWorkspaceImportState is the scoped writer the scan orchestrator uses to
// advance status + the in-flight run pointer. Fenced on expectedActive (the
// active_run_id observed in the read the decision came from; nil means "no
// in-flight run"), so a caller deciding from a STALE read cannot land it.
// applied=false means the slot moved under the caller, which must re-read
// rather than retry blindly.
func (s PG) SetWorkspaceImportState(ctx context.Context, id uuid.UUID,
	status types.WorkspaceStatus, activeRunID *uuid.UUID, expectedActive *uuid.UUID) (types.Workspace, bool, error) {
	// IS NOT DISTINCT FROM (not `=`): `active_run_id = NULL` is never true in SQL.
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET status=$1, active_run_id=$2, updated_at=now()
		 WHERE id=$3 AND active_run_id IS NOT DISTINCT FROM $4 RETURNING `+wsCols,
		string(status), activeRunID, id, expectedActive))
	ws, err = s.hydrateIfOK(ctx, ws, err)
	if errors.Is(err, ErrNotFound) {
		// Distinguish a guard miss (slot moved) from a missing workspace.
		cur, gerr := s.GetWorkspace(ctx, id)
		if gerr != nil {
			return types.Workspace{}, false, gerr
		}
		return cur, false, nil
	}
	if err != nil {
		return types.Workspace{}, false, err
	}
	return ws, true, nil
}

// DeleteWorkspace removes a workspace by id. Returns ErrNotFound when no row matched.
func (s PG) DeleteWorkspace(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("store: delete workspace: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// hydrateIfOK hydrates ws when err is nil — the mixed-return writers' shim.
func (s PG) hydrateIfOK(ctx context.Context, ws types.Workspace, err error) (types.Workspace, error) {
	if err != nil {
		return ws, err
	}
	return s.hydrated(ctx, ws)
}

// hydratedScan wraps scanWorkspace for the single-row writers: decode, then
// materialize the derived (hydrated) view.
func (s PG) hydratedScan(ctx context.Context, row pgx.Row) (types.Workspace, error) {
	ws, err := scanWorkspace(row)
	if err != nil {
		return ws, err
	}
	return s.hydrated(ctx, ws)
}

// scanWorkspace is the ONE reader for every workspaces column list in this
// package. A new column is APPENDED to the end of wsCols and of this Scan —
// the only edit that cannot silently transpose two same-typed columns.
func scanWorkspace(row pgx.Row) (types.Workspace, error) {
	var ws types.Workspace
	var status string
	var sourcesRaw, baseImageRaw, requirementsRaw, profileRaw, approvedRaw, recordRaw, llmCredRaw, attachmentsRaw, deniedRaw []byte
	err := row.Scan(
		&ws.ID, &ws.Name, &sourcesRaw, &baseImageRaw, &requirementsRaw,
		&profileRaw, &ws.ImageRef, &ws.BuiltProfileHash, &approvedRaw, &ws.ActiveRunID,
		&status, &ws.CreatedAt, &ws.UpdatedAt, &recordRaw, &llmCredRaw,
		&attachmentsRaw, &ws.BaseImageID, &deniedRaw, &ws.OwnedBy, &ws.EgressEditedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Workspace{}, ErrNotFound
	}
	if err != nil {
		return types.Workspace{}, fmt.Errorf("store: scan workspace: %w", err)
	}
	ws.Status = types.WorkspaceStatus(status)
	if profileRaw != nil {
		ws.Profile = json.RawMessage(profileRaw)
	}
	if recordRaw != nil {
		ws.RecordResults = json.RawMessage(recordRaw)
	}
	// Malformed JSONB is unreachable via the param helpers; fail safe to zero
	// value rather than error.
	_ = json.Unmarshal(sourcesRaw, &ws.Sources)
	if baseImageRaw != nil {
		var bi types.WorkspaceBaseImage
		if json.Unmarshal(baseImageRaw, &bi) == nil {
			ws.BaseImage = &bi
		}
	}
	if requirementsRaw != nil {
		_ = json.Unmarshal(requirementsRaw, &ws.Requirements)
	}
	if approvedRaw != nil {
		_ = json.Unmarshal(approvedRaw, &ws.ApprovedEgress)
	}
	if deniedRaw != nil {
		_ = json.Unmarshal(deniedRaw, &ws.DeniedEgress)
	}
	if llmCredRaw != nil {
		var c types.WorkspaceLLMCred
		if json.Unmarshal(llmCredRaw, &c) == nil {
			ws.LLMCred = &c
		}
	}
	if attachmentsRaw != nil {
		_ = json.Unmarshal(attachmentsRaw, &ws.Attachments) // fail safe: pre-split view
	}
	return ws, nil
}

// deriveWorkspaceMirrors populates ws.Kind/Source/Ref/DefaultTarget as
// READ-ONLY convenience mirrors of ws.Sources[0], for single-source
// workspaces only (the shape pkg/client and `wardyn workspace list` render);
// a multi-source workspace is left zero rather than picking one. These
// fields are NEVER persisted — they exist only after this derivation.
func deriveWorkspaceMirrors(ws *types.Workspace) {
	if len(ws.Sources) != 1 {
		return
	}
	src := ws.Sources[0]
	ws.Kind = types.WorkspaceKind(src.Type)
	ws.Ref = src.Ref
	ws.DefaultTarget = src.Target
	switch src.Type {
	case types.WorkspaceSourceTypeLocalDir:
		ws.Source = src.Path
	case types.WorkspaceSourceTypeRepo:
		ws.Source = src.Source
		// ephemeral carries neither Path nor Source; ws.Source stays "".
	}
}
