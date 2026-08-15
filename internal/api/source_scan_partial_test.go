// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// partialScanStore serves a fixed set of local_dir sources to
// GetSourcesByIDs and records every SetSourceScanResultUnfenced call, so a
// test can see exactly which sources scanAttachedSources actually scanned
// before it stopped.
type partialScanStore struct {
	store.Store
	sources map[uuid.UUID]types.Source
	scanned []uuid.UUID // ids that reached a successful SetSourceScanResultUnfenced(..., WorkspaceScanned, ...)
}

func (s *partialScanStore) GetSourcesByIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]types.Source, error) {
	out := make(map[uuid.UUID]types.Source, len(ids))
	for _, id := range ids {
		if src, ok := s.sources[id]; ok {
			out[id] = src
		}
	}
	return out, nil
}

func (s *partialScanStore) SetSourceScanResultUnfenced(_ context.Context, id uuid.UUID, _ []byte, status types.WorkspaceStatus, _ map[string]types.WorkspaceRequirement) (types.Source, error) {
	if status == types.WorkspaceScanned {
		s.scanned = append(s.scanned, id)
	}
	return s.sources[id], nil
}

// TestScanAttachedSources_FailureAuditsWhatAlreadySucceeded is the
// bug-workspace-2 regression: scanAttachedSources used to return the moment
// source i failed its stat, with no audit event recording that sources
// 0..i-1 already scanned inline first (their side effect — the stat, the
// profile write — already landed regardless). The failure audit must name
// the failing source AND list what already succeeded before it.
func TestScanAttachedSources_FailureAuditsWhatAlreadySucceeded(t *testing.T) {
	h := newHarness(t)
	goodDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(goodDir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	goodID, badID := uuid.New(), uuid.New()
	fake := &partialScanStore{sources: map[uuid.UUID]types.Source{
		goodID: {ID: goodID, Kind: types.SourceLocalDir, Locator: goodDir},
		badID:  {ID: badID, Kind: types.SourceLocalDir, Locator: "/nonexistent/does-not-exist-" + uuid.NewString()},
	}}
	srv := New(baseTestConfig(h, fake))
	ws := types.Workspace{
		ID: uuid.New(),
		Attachments: []types.WorkspaceAttachment{
			{SourceID: &goodID}, {SourceID: &badID},
		},
	}

	req, err := http.NewRequest(http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/scan", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.scanAttachedSources(rec, req, ws)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	// The good source's side effect (its inline scan) actually landed.
	if len(fake.scanned) != 1 || fake.scanned[0] != goodID {
		t.Fatalf("scanned = %v, want exactly [%s] (the good dir scanned before the bad one failed)", fake.scanned, goodID)
	}
	// The failure audit must exist and name BOTH the failing source and the
	// one that already succeeded — not just the failing one in isolation.
	var found *types.AuditEvent
	for i := range h.audit.events {
		ev := &h.audit.events[i]
		if ev.Action == "workspace.scan" && ev.Outcome == "failure" {
			found = ev
		}
	}
	if found == nil {
		t.Fatal("no workspace.scan failure audit event recorded")
	}
	data := string(found.Data)
	if !strings.Contains(data, badID.String()) {
		t.Errorf("failure audit must name the failing source %s; data=%s", badID, data)
	}
	if !strings.Contains(data, goodID.String()) {
		t.Errorf("failure audit must record the already-succeeded source %s (partial progress), not just the failing one; data=%s", goodID, data)
	}
}
