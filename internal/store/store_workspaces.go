// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The workspaces table (tier 3 of the three-tier split): its JSONB parameter
// helpers, the canonical wsCols column list, scanWorkspace, and the CRUD and
// scoped-writer methods PG exposes over a workspace row. Split out of store.go
// along the table seam — store.go had grown past the file-size lint and this
// was its largest single-table cluster, cohesive enough to move whole: nothing
// here is reachable except through a workspaces row.
//
// It is the write half of the pair store_sources.go completes: that file owns
// tiers 1 and 2 (the sources library and base-image catalog) plus the hydrate
// pass, and the scoped writers below call its s.hydrated to materialize the
// derived read-only view before returning. One workspace-row writer stays over
// there rather than here — MergeWorkspaceRequirements, the verify loop's
// overlay merge — because it belongs to the requirements seam it shares with
// the source tiers, not to this file. The two files stay separate because
// hydration is a read concern shared with pagination.go's ListWorkspacesPage,
// while everything here is a single-row statement against `workspaces`.
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

// ─── Workspace ───────────────────────────────────────────────────────────────

// jsonOrNull is the one body the nullable-JSONB param helpers below share:
// isEmpty ⇒ SQL NULL (never the literal JSON "null"), otherwise the marshalled
// bytes. WHICH emptiness counts is the caller's to state, because it differs
// per column — a nil-vs-empty distinction the store must round-trip is stated
// as `v == nil`, one the column collapses as `len(v) == 0`. A marshal error is
// unreachable for every concrete type routed through here (plain slices, maps
// and structs) and folds to NULL, the fail-safe each helper documented for
// itself. Helpers whose column is NOT NULL (workspaceSourcesParam,
// workspaceAttachmentsParam, both fail-safe to '[]') and marshalGroups (whose
// caller must see a marshal error, not a silently absent group snapshot) do
// NOT route through here.
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

// workspaceProfileParam converts a (possibly empty) json.RawMessage into the
// value pgx should bind for the nullable `profile` JSONB column: an empty
// RawMessage inserts SQL NULL (not yet scanned) rather than the literal JSON
// "null", so GetWorkspace/ListWorkspaces round-trip a pending_scan row with a
// nil Profile, not a 4-byte "null" blob.
func workspaceProfileParam(p json.RawMessage) any {
	if len(p) == 0 {
		return nil
	}
	return []byte(p)
}

// workspaceApprovedParam serializes the operator-owned approved-egress list
// for its JSONB column; empty ⇒ NULL.
func workspaceApprovedParam(domains []string) any {
	return jsonOrNull(domains, len(domains) == 0)
}

// workspaceDeniedParam is workspaceApprovedParam's mirror for the
// operator-owned denied-egress list; empty ⇒ NULL.
func workspaceDeniedParam(domains []string) any {
	return jsonOrNull(domains, len(domains) == 0)
}

// workspaceLLMCredParam serializes the operator-owned model/harness cred binding
// for its JSONB column; nil ⇒ NULL (no binding).
func workspaceLLMCredParam(c *types.WorkspaceLLMCred) any {
	return jsonOrNull(c, c == nil)
}

// workspaceSourcesParam serializes a workspace's source composition for its
// NOT NULL sources JSONB column. Unlike the other JSONB helpers here, this one
// NEVER returns nil: a nil/empty slice still marshals to the JSON array "[]"
// (never SQL NULL), matching the column's NOT NULL constraint (0029).
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

// workspaceBaseImageParam serializes a workspace's base-image choice for its
// nullable base_image JSONB column; nil ⇒ NULL (no explicit choice recorded).
func workspaceBaseImageParam(img *types.WorkspaceBaseImage) any {
	return jsonOrNull(img, img == nil)
}

// workspaceRequirementsParam serializes a workspace's requirements contract
// for its nullable requirements JSONB column; empty/nil ⇒ NULL, mirroring
// workspaceApprovedParam.
func workspaceRequirementsParam(reqs map[string]types.WorkspaceRequirement) any {
	return jsonOrNull(reqs, len(reqs) == 0)
}

// workspaceAttachmentsParam marshals a workspace's attachments for storage.
// Empty writes '[]' (the column is NOT NULL — 0031 backfills every row): an
// empty array IS the pre-split marker, and the hydrate pass falls back to the
// embedded sources column exactly as it would for NULL.
func workspaceAttachmentsParam(atts []types.WorkspaceAttachment) []byte {
	if len(atts) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(atts)
	if err != nil {
		return []byte("[]") // unreachable for this concrete type; fail-safe to the pre-split marker — NOT nil, the column is NOT NULL (matches workspaceSourcesParam)
	}
	return b
}

// wsCols is the canonical workspace column list (order matches scanWorkspace).
// denied_egress is appended LAST (not interleaved next to approved_egress):
// this const feeds both the INSERT list and every RETURNING/SELECT site, so
// appending is the only edit that cannot silently transpose it past another
// column of the same underlying type. owned_by (0048) is appended for the same
// reason, and — like denied_egress — is deliberately absent from
// UpdateWorkspace's SET clause: ownership is stamped once at create and must
// survive every composition edit.
const wsCols = `id, name, sources, base_image, requirements, profile, image_ref, ` +
	`built_profile_hash, approved_egress, active_run_id, status, created_at, updated_at, ` +
	`record_results, llm_cred, attachments, base_image_id, denied_egress, owned_by, ` +
	`egress_edited_at`

// CreateWorkspace inserts an onboarded workspace and returns the persisted
// row. Profile is internal/workspacescan's opaque WorkspaceProfile blob (nil
// until scanned).
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

// UpdateWorkspace replaces a workspace's editable composition (name, sources,
// base_image, requirements) and bumps updated_at, returning the persisted row.
// It is a FULL-column write (it also sets profile, image_ref,
// built_profile_hash, status and the other scan-owned columns) — WITH ONE
// DELIBERATE EXCEPTION: denied_egress is NOT in this SET clause, and must
// stay that way (see Workspace.DeniedEgress) — a permanent deny is an
// operator decision that survives a composition edit, unlike ApprovedEgress
// below. Every other column here is why callers must round-trip the fetched
// row. Returns ErrNotFound when no workspace has the given id.
//
// handleUpdateWorkspace does round-trip, and resets the scan-owned fields +
// ApprovedEgress itself when the composition changed — the persisted profile
// and egress approvals were reviewed against the OLD sources.
//
// egress_edited_at is CARRIED, not stamped. Migration 0055 rules out stamping
// here (a full-column write on an unrelated composition edit would suppress the
// boot heal for a decision nobody undid), and a pass-through is not a stamp: an
// edit that leaves the field alone rewrites the value it read. What it fixes is
// the opposite failure — omitting the column meant this, the THIRD durable
// writer of approved_egress, could CLEAR the allowlist while the stamp stayed
// where the last scoped setter left it, so ReconcileWorkspaceEgressDecisions
// re-widened the list on the next restart. handleUpdateWorkspace now sets the
// field when it clears; this is what persists it.
func (s PG) UpdateWorkspace(ctx context.Context, id uuid.UUID, ws types.Workspace) (types.Workspace, error) {
	q := `
		UPDATE workspaces
		SET name=$1, sources=$2, base_image=$3, requirements=$4,
			profile=$5, image_ref=$6, built_profile_hash=$7, approved_egress=$8,
			active_run_id=$9, status=$10, record_results=$11, llm_cred=$12,
			attachments=$13, base_image_id=$14, egress_edited_at=$15, updated_at=now()
		WHERE id=$16
		RETURNING ` + wsCols
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx, q,
		ws.Name, workspaceSourcesParam(ws.Sources), workspaceBaseImageParam(ws.BaseImage),
		workspaceRequirementsParam(ws.Requirements), workspaceProfileParam(ws.Profile), ws.ImageRef,
		ws.BuiltProfileHash, workspaceApprovedParam(ws.ApprovedEgress), ws.ActiveRunID,
		string(ws.Status), workspaceProfileParam(ws.RecordResults), workspaceLLMCredParam(ws.LLMCred),
		workspaceAttachmentsParam(ws.Attachments), ws.BaseImageID, ws.EgressEditedAt, id,
	))
}

// SetWorkspaceLLMCred replaces ONLY the operator-owned model/harness cred
// binding column (plus updated_at), returning the updated row. Scoped like
// SetWorkspaceApprovedEgress so it can never clobber a concurrently-persisted
// async scan. Pass nil to clear the binding.
func (s PG) SetWorkspaceLLMCred(ctx context.Context, id uuid.UUID, cred *types.WorkspaceLLMCred) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET llm_cred=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceLLMCredParam(cred), id))
}

// SetWorkspaceOwner replaces ONLY the owned_by column (plus updated_at),
// returning the updated row — the offboarding reassign (decision O6). Scoped
// like SetWorkspaceLLMCred above, and deliberately the ONLY writer of the
// column after CreateWorkspace stamps it: UpdateWorkspace's column list omits
// owned_by, so an ordinary workspace edit can never move ownership.
func (s PG) SetWorkspaceOwner(ctx context.Context, id uuid.UUID, owner string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET owned_by=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		owner, id))
}

// SetWorkspaceApprovedEgress replaces ONLY the operator-owned approved-egress
// column (plus updated_at and egress_edited_at), returning the updated row.
// Scoped on purpose: an approval must never clobber a concurrently-persisted
// scan (an async repo scan's profile/status land via the full-column
// UpdateWorkspace, and a read-modify-write here would silently revert them).
//
// egress_edited_at is what makes this the documented UNDO of an `always`
// decision rather than a change the next restart reverses: it records that the
// operator restated the list at this moment, and the boot heal
// (ReconcileWorkspaceEgressDecisions) skips any decision older than it. It is
// bumped even when domains is byte-identical to what is stored — "the operator
// looked at this list and confirmed it" is the fact being recorded, and a
// diff-conditional bump would make the heal depend on whether an idempotent
// save changed anything.
func (s PG) SetWorkspaceApprovedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET approved_egress=$1, egress_edited_at=now(), updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceApprovedParam(domains), id))
}

// qAddApprovedEgressDecision and qAddDeniedEgressDecision back
// AddWorkspaceEgressDecision — one static query per direction, chosen in Go by
// `allow` rather than interpolating a column name into SQL, so each remains a
// single auditable literal. Verified against a live Postgres (idempotent
// re-decide at a full cap, NULL-column start, and the cross-list move all
// round-trip correctly). Each query:
//
//   - appends host to ITS OWN list, deduped: the CASE leaves an already-listed
//     host's array untouched instead of re-appending it.
//   - COALESCEs both nullable columns to '[]'::jsonb before touching them
//     (neither 0010 nor 0040 gives either column a DEFAULT), so a brand-new
//     workspace's NULL columns behave as empty, not as a NULL propagating
//     through jsonb_array_length into a false "cap reached".
//   - guards the cap against the TARGET list ONLY — guarding the wrong column
//     would refuse a deny·always on a workspace that merely has 64 *approved*
//     hosts — and ORs in "host already present", so an idempotent re-decide on
//     an already-full list is never reported as "cap reached".
//   - unconditionally removes host from the OPPOSITE list via the jsonb `-`
//     (text) operator (a no-op if absent): deny beats allow everywhere the
//     proxy evaluates policy, so leaving a re-decided host on both lists would
//     make one direction a silent no-op.
//
// Both effects land in ONE UPDATE: two concurrent `always` decisions on
// different hosts cannot lose one to a read-modify-write, and a single
// decision can never half-apply (added-but-not-removed or vice versa).
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
// host: on allow it is added to approved_egress (capped at maxApprovedEgress,
// deduped) and removed from denied_egress; on deny the mirror. host is used
// verbatim — the caller normalizes and validates it (hostrules.ValidApprovedHost
// et al.; see internal/api's Phase 2 write-back), matching every other
// workspace writer in this file.
//
// maxApprovedEgress is passed in rather than respelled as a SQL literal here:
// the single Go const (internal/api/workspaces.go) also enforces the bulk
// PUT's cap, so the rule lives in exactly one place.
//
// Returns ErrConflict — not a bare "not found" — when id exists but the cap
// guard refused the write, distinguishing "no such workspace" from "cap
// reached" for the caller while keeping the same (types.Workspace, error)
// shape every sibling writer uses. Same disambiguation MergeWorkspaceRequirements
// uses for its own key-count cap.
func (s PG) AddWorkspaceEgressDecision(ctx context.Context, id uuid.UUID, host string, allow bool, maxApprovedEgress int) (types.Workspace, error) {
	q := qAddApprovedEgressDecision
	if !allow {
		q = qAddDeniedEgressDecision
	}
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx, q, id, host, maxApprovedEgress))
	ws, err = s.hydrateIfOK(ctx, ws, err)
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
	return ws, nil
}

// SetWorkspaceDeniedEgress replaces ONLY the operator-owned denied-egress
// column (plus updated_at), returning the updated row — denied_egress's
// mirror of SetWorkspaceApprovedEgress, backing the Phase 4 revocation PUT.
// Same anti-clobber discipline: a full-list replace can never clobber a
// concurrently-persisted async scan. Pass the FULL desired list — like
// SetWorkspaceApprovedEgress, this replaces rather than merges, and it stamps
// egress_edited_at for the same reason (see that method): removing a permanent
// DENY is an operator override the boot heal must not undo at the next restart.
func (s PG) SetWorkspaceDeniedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET denied_egress=$1, egress_edited_at=now(), updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceDeniedParam(domains), id))
}

// SetWorkspaceRequirements replaces ONLY the requirements-contract column
// (plus updated_at), returning the updated row. Scoped write, same
// anti-clobber discipline as SetWorkspaceApprovedEgress: it can never clobber
// a concurrently-persisted async scan (profile/status land via the full-column
// UpdateWorkspace, and a read-modify-write here would silently revert them).
// Pass the FULL desired map — like SetWorkspaceApprovedEgress, this replaces
// rather than merges; a caller adding one requirement to an existing set reads
// first, merges in Go, then calls this with the result.
func (s PG) SetWorkspaceRequirements(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET requirements=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceRequirementsParam(reqs), id))
}

// SetWorkspaceRecordResult atomically upserts ONE task's entry in the Record
// Mode record_results map (jsonb || merge — never a whole-map read-modify-
// write, so concurrent writers of DIFFERENT tasks can never lose each other's
// entries). When onlyIfStatus is non-empty the write applies only while the
// task's CURRENT stored status equals it (single-statement compare-and-set):
// a late streaming upload can never revert a completed capture, and a double
// capture no-ops. Returns applied=false (no error) on a guard miss.
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
// (possibly nil) to runID — the atomic serial-import-step gate. Two concurrent
// step launches that both observed the same free slot cannot both win: the
// loser gets applied=false and must NOT launch. Returns ErrNotFound only when
// the workspace does not exist.
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
// runID (conditional, single statement) — a terminal run's cleanup can never
// clobber a step that was concurrently launched and now owns the pointer.
func (s PG) ClearWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE workspaces SET active_run_id=NULL, updated_at=now() WHERE id=$1 AND active_run_id=$2`,
		id, runID)
	if err != nil {
		return false, fmt.Errorf("store: clear active run: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetWorkspaceBuiltImage scoped-writes ONLY the image cache columns (the
// build-once/reuse-many cache) — the anti-clobber discipline the other scoped
// writers established; the previous full-row cache write could revert every
// concurrently-persisted async field from a stale snapshot.
func (s PG) SetWorkspaceBuiltImage(ctx context.Context, id uuid.UUID, imageRef, builtHash string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET image_ref=$1, built_profile_hash=$2, updated_at=now() WHERE id=$3 RETURNING `+wsCols,
		imageRef, builtHash, id))
}

// SetWorkspaceImportState is the scoped writer the scan orchestrator uses to
// advance status + the in-flight run pointer without a full-row read-modify-
// write.
//
// FENCED (same shape as ClaimSourceActiveRun / SetSourceScanResult): the write is conditional on the
// import-step slot still holding expectedActive, so a caller that decided what
// to write from a STALE read cannot land it. Every caller here does check-then-
// act (read the workspace, decide, write), and this was the only unfenced
// workspace writer — a finalize/update racing a live scan/record run could
// overwrite the fresher state the concurrent run had just written, which is
// exactly the class of race the C001 finalize guard closed at one call site
// only. Pass expectedActive = the active_run_id observed in the read the
// decision came from (nil means "expected no in-flight run"); applied=false
// means the slot moved under the caller, which must then re-read rather than
// retry blindly. Returns ErrNotFound only when the workspace does not exist.
func (s PG) SetWorkspaceImportState(ctx context.Context, id uuid.UUID,
	status types.WorkspaceStatus, activeRunID *uuid.UUID, expectedActive *uuid.UUID) (types.Workspace, bool, error) {
	// IS NOT DISTINCT FROM (not `=`) so a nil expectedActive correctly matches a
	// NULL slot — `active_run_id = NULL` is never true in SQL.
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

// hydratedScan wraps scanWorkspace for the single-row writers: decode the row,
// then materialize the derived view (attachments → Sources/Profile/Status/
// BaseImage + the folded EffectiveRequirements).
func (s PG) hydratedScan(ctx context.Context, row pgx.Row) (types.Workspace, error) {
	ws, err := scanWorkspace(row)
	if err != nil {
		return ws, err
	}
	return s.hydrated(ctx, ws)
}

// scanWorkspace is the ONE reader for every workspaces column list in this
// package (wsCols feeds CreateWorkspace's RETURNING, GetWorkspace,
// ListWorkspacesPage, and every scoped Set*/Merge* writer's RETURNING). A new
// column is APPENDED to the end of wsCols and to the end of this Scan —
// appending is the only edit that cannot silently transpose two same-typed
// columns past the compiler.
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
	// sources is NOT NULL; malformed JSONB is unreachable via
	// workspaceSourcesParam, but fail safe to "no sources" rather than error.
	_ = json.Unmarshal(sourcesRaw, &ws.Sources)
	if baseImageRaw != nil {
		var bi types.WorkspaceBaseImage
		if json.Unmarshal(baseImageRaw, &bi) == nil {
			ws.BaseImage = &bi
		}
	}
	if requirementsRaw != nil {
		// Malformed JSONB is unreachable via workspaceRequirementsParam; on the
		// off chance, fail safe to "no requirements declared" rather than error.
		_ = json.Unmarshal(requirementsRaw, &ws.Requirements)
	}
	if approvedRaw != nil {
		// Malformed JSONB is unreachable via workspaceApprovedParam; on the
		// off chance, fail safe to "nothing approved" rather than error.
		_ = json.Unmarshal(approvedRaw, &ws.ApprovedEgress)
	}
	if deniedRaw != nil {
		// Malformed JSONB is unreachable via workspaceDeniedParam; on the
		// off chance, fail safe to "nothing denied" rather than error.
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
// READ-ONLY convenience mirrors of ws.Sources[0], for single-source workspaces
// only — the shape pkg/client and `wardyn workspace list` still render. A
// multi-source workspace has no single "the" kind/source/ref/target, so the
// mirrors are left zero rather than arbitrarily picking one. Derived purely
// from Sources[0] (never BaseImage): a migrated legacy 'container' workspace
// (now an ephemeral source + a custom base image) mirrors as
// Kind="ephemeral", not Kind="container" — the composition model has no
// single-field notion of "container" to reconstruct. These fields are NEVER
// persisted; they exist on the Go struct only after this derivation.
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
