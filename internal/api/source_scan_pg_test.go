// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestScanWorkspace_RepoPlusDir_ScansEverySource is the Wave-3 gate: the old
// whole-workspace scan's 3-branch switch let the FIRST matching branch win, so
// a repo+dir workspace launched the repo's governed scan and NEVER ran the
// dirs' inline scan (the "later call" its comment promised re-entered the same
// repo branch). The per-source fan-out must scan EVERY attached source in one
// call: dirs inline (profile lands immediately), repos as governed runs
// (202 + scan_run_ids), each fenced on its OWN sources.active_run_id.
func TestScanWorkspace_RepoPlusDir_ScansEverySource(t *testing.T) {
	fr := &fakeRunner{}
	srv, pool := pgHarnessWithRunner(t, fr)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	// Repo FIRST — the exact ordering the legacy switch turned into "dir never
	// scans". The slug is unique per run: the library dedupes on canonical
	// identity, so a fixed slug would resolve to the row a PRIOR test run left
	// fenced `scanning` and the fan-out would (correctly) skip it.
	suffix := uuid.NewString()[:8]
	wsName := "repo-plus-dir-" + suffix
	body := fmt.Sprintf(`{"name":%q,"sources":[
		{"type":"repo","source":"acme/widgets-%s"},
		{"type":"local_dir","path":%q,"writable":true}
	]}`, wsName, suffix, dir)
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create workspace: code = %d, body=%s", w.Code, w.Body.String())
	}
	var ws types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}
	if len(ws.Attachments) != 2 || ws.Attachments[0].SourceID == nil || ws.Attachments[1].SourceID == nil {
		t.Fatalf("want 2 library-backed attachments, got %+v", ws.Attachments)
	}
	repoSrcID, dirSrcID := *ws.Attachments[0].SourceID, *ws.Attachments[1].SourceID

	w = do(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/scan", adminToken, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("scan: code = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ScanRunIDs []uuid.UUID `json:"scan_run_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode scan response: %v", err)
	}
	if len(resp.ScanRunIDs) != 1 {
		t.Fatalf("want exactly one governed scan run (the repo), got %v", resp.ScanRunIDs)
	}

	// THE bug assertion: the dir source scanned INLINE in the same call —
	// status scanned, profile present — while the repo's governed run is in
	// flight. Legacy behavior left the dir pending_scan forever.
	var dirStatus, repoStatus string
	var dirProfile []byte
	var repoActive *uuid.UUID
	if err := pool.QueryRow(t.Context(),
		`SELECT status, profile FROM sources WHERE id = $1`, dirSrcID).Scan(&dirStatus, &dirProfile); err != nil {
		t.Fatalf("read dir source: %v", err)
	}
	if err := pool.QueryRow(t.Context(),
		`SELECT status, active_run_id FROM sources WHERE id = $1`, repoSrcID).Scan(&repoStatus, &repoActive); err != nil {
		t.Fatalf("read repo source: %v", err)
	}
	if dirStatus != string(types.WorkspaceScanned) || len(dirProfile) == 0 {
		t.Fatalf("dir source: status=%q profile=%dB — the repo branch won again", dirStatus, len(dirProfile))
	}
	// Discovery lands on the SOURCE's own contract: the dir's write path seeds
	// as an optional scan_seeded row (the workspace level only aggregates).
	var dirReqs map[string]types.WorkspaceRequirement
	var reqsRaw []byte
	if err := pool.QueryRow(t.Context(),
		`SELECT COALESCE(requirements, '{}'::jsonb) FROM sources WHERE id = $1`, dirSrcID).Scan(&reqsRaw); err != nil {
		t.Fatalf("read dir contract: %v", err)
	}
	if err := json.Unmarshal(reqsRaw, &dirReqs); err != nil {
		t.Fatalf("decode dir contract: %v", err)
	}
	if row := dirReqs["write:"+dir]; row.Level != "optional" || row.Provenance != "scan_seeded" {
		t.Errorf("dir scan did not seed its own write row: %+v", dirReqs)
	}
	if repoStatus != string(types.WorkspaceScanning) || repoActive == nil || *repoActive != resp.ScanRunIDs[0] {
		t.Fatalf("repo source: status=%q active_run_id=%v, want scanning fenced on %s", repoStatus, repoActive, resp.ScanRunIDs[0])
	}

	// The governed run carries the TRUSTED source linkage (never WorkspaceID —
	// the scan belongs to the library entry, not one aggregate attaching it).
	w = do(t, srv, http.MethodGet, "/api/v1/runs/"+resp.ScanRunIDs[0].String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get scan run: code = %d, body=%s", w.Code, w.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Task != "source scan" || run.SourceID == nil || *run.SourceID != repoSrcID || run.WorkspaceID != nil {
		t.Fatalf("scan run linkage: task=%q source_id=%v workspace_id=%v, want source scan → %s", run.Task, run.SourceID, run.WorkspaceID, repoSrcID)
	}

	// Rescan while the repo scan is in flight: its fence skips relaunch (no
	// second run), the dir simply rescans inline — 200, not a duplicate 202.
	w = do(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/scan", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("rescan during in-flight repo scan: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
