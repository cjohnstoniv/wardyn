// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// source_scan.go — the tier-1 scan: a SOURCE scans itself. This is what
// structurally fixes the old whole-workspace scan's bug (a repo+dir workspace
// never scanned its dirs — the repo branch won on every call and the "later
// call" its comment promised re-entered the same branch). Each source owns its
// scan lifecycle: the fence is sources.active_run_id, the result lands on the
// source row, and a workspace's profile is the MERGE of its attached sources'
// profiles, computed at the store's hydrate pass.

// handleScanSource scans one library source.
//
//   - local_dir: host-side inline (bounded, read-only workspacescan.Scan),
//     exactly the old inline path one row over. A missing/non-dir path
//     persists status=error and 422s with the same diagnosis the workspace
//     scan gives — never a false green.
//
//   - repo: a governed clone-and-scan run, fenced on the SOURCE's own
//     active_run_id. 202 with scan_run_id; the profile lands when the run
//     uploads its facts (handleUploadScanResult's source lane).
//
//     POST /api/v1/sources/{id}/scan
func (s *Server) handleScanSource(w http.ResponseWriter, r *http.Request) {
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
	actorType, actor := actorFromRequest(r)

	switch src.Kind {
	case types.SourceLocalDir:
		profile, detail, ok := s.scanLocalDirSource(r.Context(), src)
		if !ok {
			s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
				"source.scan", id.String(), "failure", mustJSON(map[string]any{"detail": detail})))
			writeError(w, http.StatusUnprocessableEntity, detail)
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
			"source.scan", id.String(), "success", mustJSON(map[string]any{
				"confidence": profile.Confidence, "secret_reqs": len(profile.RequiredSecrets),
				"suggested_egress": len(profile.SuggestedEgress), "leak_findings": len(profile.LeakFindings),
			})))
		writeJSON(w, http.StatusOK, profile)

	case types.SourceRepo:
		if s.cfg.Runner == nil {
			writeError(w, http.StatusServiceUnavailable, "no runner configured to launch a governed scan run")
			return
		}
		run, lerr := s.launchSourceScanRun(r.Context(), actor, src)
		if errors.Is(lerr, store.ErrConflict) {
			writeError(w, http.StatusConflict, "a scan is already running for this source")
			return
		}
		if lerr != nil {
			s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
				"source.scan", id.String(), "failure", mustJSON(map[string]any{"detail": lerr.Error()})))
			writeError(w, http.StatusInternalServerError, "launch scan run: "+lerr.Error())
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor,
			"source.scan", id.String(), "success", mustJSON(map[string]any{"scan_run_id": run.ID.String()})))
		writeJSON(w, http.StatusAccepted, map[string]any{
			"scan_run_id": run.ID, "source_id": src.ID, "state": run.State,
			"detail": "governed scan run launched; the source profile updates when the scan completes",
		})

	default:
		writeError(w, http.StatusUnprocessableEntity, "source kind has nothing to scan")
	}
}

// scanLocalDirSource runs the bounded host-side scan for one directory source
// and persists the outcome on the source row. ok=false returns the 422 detail
// (already persisted as status=error) — the same never-a-false-green rule the
// workspace scan enforces, with the same sealed-daemon diagnosis.
func (s *Server) scanLocalDirSource(ctx context.Context, src types.Source) (workspacescan.WorkspaceProfile, string, bool) {
	fi, serr := os.Stat(src.Locator)
	if serr != nil || !fi.IsDir() {
		detail := localDirScanFailureDetail(src.Locator, serr == nil && !fi.IsDir(),
			os.Getenv("WARDYN_WORKSPACES_ROOT"), runningInContainer())
		_, _ = s.cfg.Store.SetSourceScanResultUnfenced(ctx, src.ID, src.Profile, types.WorkspaceError)
		return workspacescan.WorkspaceProfile{}, detail, false
	}
	profile := workspacescan.Scan(src.Locator)
	if _, err := s.cfg.Store.SetSourceScanResultUnfenced(ctx, src.ID, mustJSON(profile), types.WorkspaceScanned); err != nil {
		return workspacescan.WorkspaceProfile{}, "persist scan profile: " + err.Error(), false
	}
	return profile, "", true
}

// launchSourceScanRun is launchScanRun retargeted one tier down: the fence is
// the SOURCE's active_run_id (ClaimSourceActiveRun flips it to scanning), the
// run carries SourceID as its trusted linkage (never WorkspaceID — this run
// belongs to the library entry, not to any one aggregate attaching it), and
// the upload lands via handleUploadScanResult's source lane.
func (s *Server) launchSourceScanRun(ctx context.Context, actor string, src types.Source) (types.AgentRun, error) {
	url := repoCloneURL(src.Locator)
	if url == "" {
		return types.AgentRun{}, fmt.Errorf("repo %q has no derivable clone URL", src.Locator)
	}
	// Detach from request cancellation before the durable launch work (the
	// launchRecordRun rationale: a client that walks away must
	// not cancel it).
	ctx = context.WithoutCancel(ctx)

	runID := uuid.New()
	if err := s.cfg.Store.ClaimSourceActiveRun(ctx, src.ID, runID); err != nil {
		return types.AgentRun{}, err
	}
	// From here every failure releases the fence and restores the source's
	// prior status — a launch that never dispatched must not strand the row
	// in `scanning`.
	release := func(cause error) error {
		_ = s.cfg.Store.ClearSourceActiveRun(ctx, src.ID, runID)
		_, _ = s.cfg.Store.SetSourceScanResultUnfenced(ctx, src.ID, src.Profile, src.Status)
		return cause
	}

	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, actor, actor, internalAudience)
	if err != nil {
		return types.AgentRun{}, release(fmt.Errorf("mint run identity: %w", err))
	}
	cc := s.defaultFloorClass()
	now := s.cfg.Now().UTC()
	srcID := src.ID
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: actor,
		Agent: "claude-code", Task: "source scan",
		ConfinementClass: cc, State: types.RunPending, SPIFFEID: id.SPIFFEID,
		RunnerTarget:     s.cfg.RunnerTarget,
		SourceID:         &srcID,
		Repo:             src.Locator,
		AutoStopAfterSec: scanIdleCapSec,
	}
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return types.AgentRun{}, release(fmt.Errorf("create scan run: %w", err))
	}

	ghGrantID, sshGrants, gerr := s.workspaceSourceGrants(ctx, runID, now, url)
	if gerr != nil {
		return types.AgentRun{}, release(fmt.Errorf("create scan clone grants: %w", gerr))
	}
	scanPolicy := types.RunPolicySpec{
		MinConfinementClass: cc,
		AllowedDomains:      scanEgressDomains(url),
		AutoStopAfterSec:    scanIdleCapSec,
	}
	return s.dispatchAndSettle(ctx, created, dispatchParams{
		RunToken:           id.Token,
		Image:              agentImage("claude-code", s.cfg.AgentImages),
		Policy:             scanPolicy,
		FirstGitHubGrantID: ghGrantID,
		GitGrants:          gitBrokerGrant(url, ghGrantID),
		SSHGrants:          sshGrants,
	}), nil
}

// scanAttachedSources is the whole-workspace scan's attachment-backed branch:
// fan out over every attached non-ephemeral source — dirs inline, repos as
// their own governed runs. 202 when anything launched (the workspace's merged
// profile lands as the sources finish), 200 with the freshly-hydrated merge
// when everything was inline, 422 when any dir failed its stat (the same
// never-a-false-green rule, per source).
func (s *Server) scanAttachedSources(w http.ResponseWriter, r *http.Request, ws types.Workspace) {
	actorType, actor := actorFromRequest(r)
	sourceIDs := make([]uuid.UUID, 0, len(ws.Attachments))
	for _, att := range ws.Attachments {
		if att.SourceID != nil {
			sourceIDs = append(sourceIDs, *att.SourceID)
		}
	}
	sources, err := s.cfg.Store.GetSourcesByIDs(r.Context(), sourceIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load attached sources: "+err.Error())
		return
	}

	launched := []uuid.UUID{}
	for _, id := range sourceIDs {
		src, found := sources[id]
		if !found {
			// Dangling attachment: the mount gate reports this loudly at run
			// time; a scan simply has nothing to read.
			continue
		}
		switch src.Kind {
		case types.SourceLocalDir:
			if _, detail, ok := s.scanLocalDirSource(r.Context(), src); !ok {
				s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
					"workspace.scan", ws.ID.String(), "failure", mustJSON(map[string]any{"detail": detail})))
				writeError(w, http.StatusUnprocessableEntity, detail)
				return
			}
		case types.SourceRepo:
			if src.Status == types.WorkspaceScanning {
				continue // its own fence already has a run in flight
			}
			if s.cfg.Runner == nil {
				writeError(w, http.StatusServiceUnavailable, "no runner configured to launch a governed scan run")
				return
			}
			run, lerr := s.launchSourceScanRun(r.Context(), actor, src)
			if errors.Is(lerr, store.ErrConflict) {
				continue // raced another claim — that run covers it
			}
			if lerr != nil {
				s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
					"workspace.scan", ws.ID.String(), "failure", mustJSON(map[string]any{"detail": lerr.Error()})))
				writeError(w, http.StatusInternalServerError, "launch scan run: "+lerr.Error())
				return
			}
			launched = append(launched, run.ID)
		}
	}

	if len(launched) > 0 {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
			"workspace.scan", ws.ID.String(), "success", mustJSON(map[string]any{
				"sources": len(sourceIDs), "scan_run_ids": launched,
			})))
		writeJSON(w, http.StatusAccepted, map[string]any{
			"scan_run_ids": launched, "workspace_id": ws.ID,
			"detail": "governed scan run(s) launched; the workspace profile updates as each source's scan completes",
		})
		return
	}
	// Everything was inline (or ephemeral/dangling): re-read so the response
	// carries the freshly-merged profile the hydrate pass computes.
	fresh, err := s.cfg.Store.GetWorkspace(r.Context(), ws.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reload workspace: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
		"workspace.scan", ws.ID.String(), "success", mustJSON(map[string]any{"sources": len(sourceIDs)})))
	writeJSON(w, http.StatusOK, json.RawMessage(fresh.Profile))
}
