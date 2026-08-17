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
		profile, aiRan, aiChanged, detail, ok := s.scanLocalDirSource(r.Context(), src)
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
				// AI-advisor discriminator, same shape uploadSourceScanResult records
				// for a repo source (scanresult.go) — W9-S1-6.
				"ai_advisor": aiRan, "ai_changed": aiChanged,
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

// seedSourceRequirements derives the requirement rows a scan DISCOVERS for
// the source's OWN contract — the wizard's client-side seeding rules moved to
// where they belong (the tier that was scanned): a detected secret is
// required unless the scan itself flagged it optional; an auto-allowed egress
// host is required (already reachable — the row just states it as contract);
// a directory's own write path is optional (mounted read-only until a run
// asks for more). Provenance scan_seeded throughout — the fold's trust
// boundary makes such rows poison auto-granting, so discovery can never mint
// a credential. The store REBUILDS the scan_seeded subset of the contract from
// this seed on every successful scan (a name a rescan no longer finds is
// dropped, not stuck forever); operator-edited/non-scan_seeded rows always win
// regardless; a nil seed (a failed scan) leaves the contract untouched.
func seedSourceRequirements(kind types.SourceKind, locator string, p workspacescan.WorkspaceProfile) map[string]types.WorkspaceRequirement {
	seed := map[string]types.WorkspaceRequirement{}
	for _, sec := range p.RequiredSecrets {
		// A scan reports the ENV-VAR name the code reads ("AWS_DEFAULT_REGION");
		// a secret: row names an entry in Wardyn's secret STORE, whose grammar
		// (secretNameRE) can never hold that shape — map onto the storable name
		// (compose.go's normalize-to-storable rule), skip what nothing maps.
		name := sanitizeSecretName(sec.Name)
		if name == "" {
			continue
		}
		level := "required"
		if sec.Optional {
			level = "optional"
		}
		seed["secret:"+name] = types.WorkspaceRequirement{Level: level, Provenance: "scan_seeded"}
	}
	for _, host := range p.EgressDomains {
		seed["egress:"+host] = types.WorkspaceRequirement{Level: "required", Provenance: "scan_seeded"}
	}
	if kind == types.SourceLocalDir && locator != "" {
		seed["write:"+locator] = types.WorkspaceRequirement{Level: "optional", Provenance: "scan_seeded"}
	}
	return seed
}

// scanLocalDirSource runs the bounded host-side scan for one directory source
// and persists the outcome on the source row. ok=false returns the 422 detail
// (already persisted as status=error) — the same never-a-false-green rule the
// workspace scan enforces, with the same sealed-daemon diagnosis.
//
// Consults the SAME opt-in ADVISORY AI gap-fill the sandboxed repo-scan upload
// lane does (applyScanAIAdvisor, scanresult.go) — WARDYN_SCAN_AI_ADVISOR
// previously only ever ran for a repo source, silently never firing for a
// local_dir one even though the flag's own help text makes no such
// distinction (W9-S1-6). aiRan/aiChanged mirror uploadSourceScanResult's own
// audit discriminator for the caller to record.
func (s *Server) scanLocalDirSource(ctx context.Context, src types.Source) (profile workspacescan.WorkspaceProfile, aiRan, aiChanged bool, detail string, ok bool) {
	fi, serr := os.Stat(src.Locator)
	if serr != nil || !fi.IsDir() {
		detail := localDirScanFailureDetail(src.Locator, serr == nil && !fi.IsDir(),
			os.Getenv("WARDYN_WORKSPACES_ROOT"), runningInContainer())
		_, _ = s.cfg.Store.SetSourceScanResultUnfenced(ctx, src.ID, src.Profile, types.WorkspaceError, nil)
		return workspacescan.WorkspaceProfile{}, false, false, detail, false
	}
	facts := workspacescan.CollectFacts(src.Locator)
	profile = workspacescan.DeriveProfile(facts)
	profile, aiRan, aiChanged = s.applyScanAIAdvisor(ctx, facts, profile)
	if _, err := s.cfg.Store.SetSourceScanResultUnfenced(ctx, src.ID, mustJSON(profile), types.WorkspaceScanned, seedSourceRequirements(src.Kind, src.Locator, profile)); err != nil {
		return workspacescan.WorkspaceProfile{}, false, false, "persist scan profile: " + err.Error(), false
	}
	return profile, aiRan, aiChanged, "", true
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
		_, _ = s.cfg.Store.SetSourceScanResultUnfenced(ctx, src.ID, src.Profile, src.Status, nil)
		return cause
	}

	cc := s.defaultFloorClass()
	srcID := src.ID
	run, token, err := s.newStepRun(ctx, runID, actor, "source scan", cc, func(run *types.AgentRun) {
		run.SourceID = &srcID
		run.Repo = src.Locator
		run.AutoStopAfterSec = scanIdleCapSec
	})
	if err != nil {
		return types.AgentRun{}, release(err)
	}
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return types.AgentRun{}, release(fmt.Errorf("create scan run: %w", err))
	}

	ghGrantID, sshGrants, gerr := s.workspaceSourceGrants(ctx, runID, run.CreatedAt, url)
	if gerr != nil {
		return types.AgentRun{}, release(fmt.Errorf("create scan clone grants: %w", gerr))
	}
	// DECISION (Phase 4): a workspace's DeniedEgress deliberately does NOT reach
	// this policy, unlike confinedEgressDomains/workspace_run.go. src is a
	// LIBRARY-tier Source (source_scan.go's package doc), not a Workspace, and
	// it carries no back-reference to one: a Source is attached to zero, one, or
	// many workspaces via THEIR Attachments (one-directional), so there is no
	// single well-defined workspace whose DeniedEgress would even apply here —
	// unlike a workspace step run, which always has exactly one trusted
	// run.WorkspaceID. Resolving "the" owning workspace would mean a fresh store
	// scan per launch (indexWorkspacesBySource) for an ambiguous, possibly
	// multi-valued answer. And unlike confinedEgressDomains's deliberately WIDE
	// setup allowlist, AllowedDomains here is already minimal by construction
	// (scanEgressDomains: just this one repo's own clone host, GitHub excluded
	// entirely — it routes through the broker) — there is essentially nothing
	// for a deny to subtract from. If a per-source deny is ever wanted, it
	// belongs on Source itself (mirroring DeniedEgress), not borrowed from an
	// attaching workspace.
	scanPolicy := types.RunPolicySpec{
		MinConfinementClass: cc,
		AllowedDomains:      scanEgressDomains(url),
		AutoStopAfterSec:    scanIdleCapSec,
	}
	return s.dispatchAndSettle(ctx, created, dispatchParams{
		RunToken:           token,
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

	// launched/scanned accumulate what already succeeded before a failing
	// source aborts the fan-out (bug-workspace-2): the moment source i fails,
	// every source 0..i-1 already scanned inline or already launched a
	// governed run keeps that side effect regardless — dirs already read,
	// runs already dispatched and billing/consuming egress. Without this,
	// the failure audit named only the ONE failing source, with no record
	// that N others actually succeeded first — a genuine gap for whoever
	// later asks "did source X get scanned" and finds no success event for
	// it, only the unrelated failure of source Y.
	launched := []uuid.UUID{}
	scanned := []uuid.UUID{}
	partialAudit := func(failedSourceID uuid.UUID, detail string) {
		data := map[string]any{"detail": detail, "failed_source_id": failedSourceID}
		if len(scanned) > 0 {
			data["scanned_source_ids"] = scanned
		}
		if len(launched) > 0 {
			data["scan_run_ids"] = launched
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
			"workspace.scan", ws.ID.String(), "failure", mustJSON(data)))
	}
	for _, id := range sourceIDs {
		src, found := sources[id]
		if !found {
			// Dangling attachment: the mount gate reports this loudly at run
			// time; a scan simply has nothing to read.
			continue
		}
		switch src.Kind {
		case types.SourceLocalDir:
			if _, _, _, detail, ok := s.scanLocalDirSource(r.Context(), src); !ok {
				partialAudit(id, detail)
				writeError(w, http.StatusUnprocessableEntity, detail)
				return
			}
			scanned = append(scanned, id)
		case types.SourceRepo:
			if src.Status == types.WorkspaceScanning {
				continue // its own fence already has a run in flight
			}
			if s.cfg.Runner == nil {
				partialAudit(id, "no runner configured to launch a governed scan run")
				writeError(w, http.StatusServiceUnavailable, "no runner configured to launch a governed scan run")
				return
			}
			run, lerr := s.launchSourceScanRun(r.Context(), actor, src)
			if errors.Is(lerr, store.ErrConflict) {
				continue // raced another claim — that run covers it
			}
			if lerr != nil {
				partialAudit(id, lerr.Error())
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
