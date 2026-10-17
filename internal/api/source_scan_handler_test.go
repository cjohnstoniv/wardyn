// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
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

// scanSourceStore serves a fixed set of sources and records what the scan
// persisted, so the handler tests can see both the answer and the side effect.
type scanSourceStore struct {
	store.Store
	sources  map[uuid.UUID]types.Source
	getErr   error
	listErr  error
	fresh    types.Workspace
	freshErr error
	statuses map[uuid.UUID]types.WorkspaceStatus
}

func (s *scanSourceStore) GetSource(_ context.Context, id uuid.UUID) (types.Source, error) {
	if s.getErr != nil {
		return types.Source{}, s.getErr
	}
	src, ok := s.sources[id]
	if !ok {
		return types.Source{}, store.ErrNotFound
	}
	return src, nil
}

func (s *scanSourceStore) GetSourcesByIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]types.Source, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := map[uuid.UUID]types.Source{}
	for _, id := range ids {
		if src, ok := s.sources[id]; ok {
			out[id] = src
		}
	}
	return out, nil
}

func (s *scanSourceStore) SetSourceScanResultUnfenced(_ context.Context, id uuid.UUID, _ []byte, status types.WorkspaceStatus, _ map[string]types.WorkspaceRequirement) (types.Source, error) {
	if s.statuses == nil {
		s.statuses = map[uuid.UUID]types.WorkspaceStatus{}
	}
	s.statuses[id] = status
	return s.sources[id], nil
}

func (s *scanSourceStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.fresh, s.freshErr
}

func (h *harness) auditOutcomes(action string) []string {
	var out []string
	for _, ev := range h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev.Outcome)
		}
	}
	return out
}

func TestHandleScanSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirID, missingID, repoID, oddID, badRepoID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	newSrv := func(t *testing.T, st *scanSourceStore, withRunner bool) (*harness, *Server) {
		t.Helper()
		h := newHarness(t)
		cfg := baseTestConfig(h, st)
		if withRunner {
			cfg.Runner = &fakeRunner{}
		}
		return h, New(cfg)
	}
	sources := func() map[uuid.UUID]types.Source {
		return map[uuid.UUID]types.Source{
			dirID:     {ID: dirID, Kind: types.SourceLocalDir, Locator: dir},
			missingID: {ID: missingID, Kind: types.SourceLocalDir, Locator: filepath.Join(dir, "gone")},
			repoID:    {ID: repoID, Kind: types.SourceRepo, Locator: "https://example.invalid/acme/widgets"},
			badRepoID: {ID: badRepoID, Kind: types.SourceRepo, Locator: "ftp://example.invalid/x"},
			oddID:     {ID: oddID, Kind: types.SourceKind("ephemeral")},
		}
	}
	scan := func(srv *Server, id string) *httptest.ResponseRecorder {
		return do(t, srv, http.MethodPost, "/api/v1/sources/"+id+"/scan", adminToken, "")
	}

	t.Run("malformed id", func(t *testing.T) {
		_, srv := newSrv(t, &scanSourceStore{sources: sources()}, false)
		if w := scan(srv, "not-a-uuid"); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonInvalidIDParam {
			t.Errorf("= %d %s, want 400 %s", w.Code, w.Body.String(), reasonInvalidIDParam)
		}
	})
	t.Run("unknown source", func(t *testing.T) {
		_, srv := newSrv(t, &scanSourceStore{sources: sources()}, false)
		if w := scan(srv, uuid.NewString()); w.Code != http.StatusNotFound || reasonOf(t, w) != reasonSourceNotFound {
			t.Errorf("= %d %s, want 404 %s", w.Code, w.Body.String(), reasonSourceNotFound)
		}
	})
	t.Run("store failure is a 500", func(t *testing.T) {
		_, srv := newSrv(t, &scanSourceStore{getErr: errors.New("db down")}, false)
		if w := scan(srv, dirID.String()); w.Code != http.StatusInternalServerError {
			t.Errorf("= %d %s, want 500", w.Code, w.Body.String())
		}
	})
	t.Run("local_dir scans inline and audits success", func(t *testing.T) {
		st := &scanSourceStore{sources: sources()}
		h, srv := newSrv(t, st, false)
		w := scan(srv, dirID.String())
		if w.Code != http.StatusOK {
			t.Fatalf("= %d %s, want 200", w.Code, w.Body.String())
		}
		var profile map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil || len(profile) == 0 {
			t.Errorf("body %q is not a profile (err %v)", w.Body.String(), err)
		}
		if st.statuses[dirID] != types.WorkspaceScanned {
			t.Errorf("persisted status = %q, want %q", st.statuses[dirID], types.WorkspaceScanned)
		}
		if got := h.auditOutcomes("source.scan"); len(got) != 1 || got[0] != "success" {
			t.Errorf("source.scan audit outcomes = %v, want [success]", got)
		}
	})
	t.Run("local_dir that is gone is 422, persisted as error, never a false green", func(t *testing.T) {
		st := &scanSourceStore{sources: sources()}
		h, srv := newSrv(t, st, false)
		w := scan(srv, missingID.String())
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonSourceScanFailed {
			t.Fatalf("= %d %s, want 422 %s", w.Code, w.Body.String(), reasonSourceScanFailed)
		}
		if st.statuses[missingID] != types.WorkspaceError {
			t.Errorf("persisted status = %q, want %q", st.statuses[missingID], types.WorkspaceError)
		}
		if got := h.auditOutcomes("source.scan"); len(got) != 1 || got[0] != "failure" {
			t.Errorf("source.scan audit outcomes = %v, want [failure]", got)
		}
	})
	t.Run("repo without a runner is 503", func(t *testing.T) {
		_, srv := newSrv(t, &scanSourceStore{sources: sources()}, false)
		if w := scan(srv, repoID.String()); w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonSourceScanNoRunner {
			t.Errorf("= %d %s, want 503 %s", w.Code, w.Body.String(), reasonSourceScanNoRunner)
		}
	})
	t.Run("repo with no derivable clone URL fails the launch and audits it", func(t *testing.T) {
		h, srv := newSrv(t, &scanSourceStore{sources: sources()}, true)
		if w := scan(srv, badRepoID.String()); w.Code != http.StatusInternalServerError {
			t.Errorf("= %d %s, want 500", w.Code, w.Body.String())
		}
		if got := h.auditOutcomes("source.scan"); len(got) != 1 || got[0] != "failure" {
			t.Errorf("source.scan audit outcomes = %v, want [failure]", got)
		}
	})
	t.Run("a kind with nothing to scan is 422", func(t *testing.T) {
		_, srv := newSrv(t, &scanSourceStore{sources: sources()}, false)
		if w := scan(srv, oddID.String()); w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonSourceScanUnsupportedKind {
			t.Errorf("= %d %s, want 422 %s", w.Code, w.Body.String(), reasonSourceScanUnsupportedKind)
		}
	})
}

func TestScanAttachedSourcesOutcomes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirID, repoID, scanningID, dangling := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	base := func() *scanSourceStore {
		return &scanSourceStore{
			sources: map[uuid.UUID]types.Source{
				dirID:      {ID: dirID, Kind: types.SourceLocalDir, Locator: dir},
				repoID:     {ID: repoID, Kind: types.SourceRepo, Locator: "https://example.invalid/acme/widgets"},
				scanningID: {ID: scanningID, Kind: types.SourceRepo, Locator: "https://example.invalid/acme/busy", Status: types.WorkspaceScanning},
			},
			fresh: types.Workspace{Profile: json.RawMessage(`{"merged":true}`)},
		}
	}
	attach := func(ids ...uuid.UUID) types.Workspace {
		ws := types.Workspace{ID: uuid.New()}
		for _, id := range ids {
			ws.Attachments = append(ws.Attachments, types.WorkspaceAttachment{SourceID: &id})
		}
		// An ephemeral attachment names no source and has nothing to scan.
		ws.Attachments = append(ws.Attachments, types.WorkspaceAttachment{})
		return ws
	}
	run := func(t *testing.T, st *scanSourceStore, withRunner bool, ws types.Workspace) (*harness, *httptest.ResponseRecorder) {
		t.Helper()
		h := newHarness(t)
		cfg := baseTestConfig(h, st)
		if withRunner {
			cfg.Runner = &fakeRunner{}
		}
		w := httptest.NewRecorder()
		New(cfg).scanAttachedSources(w, httptest.NewRequest(http.MethodPost, "/scan", nil), ws)
		return h, w
	}

	t.Run("inline scans, a dangling attachment and a scanning repo return the merged profile", func(t *testing.T) {
		st := base()
		h, w := run(t, st, false, attach(dirID, scanningID, dangling))
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"merged":true}` {
			t.Fatalf("= %d %s, want 200 with the freshly merged profile", w.Code, w.Body.String())
		}
		if st.statuses[dirID] != types.WorkspaceScanned || len(st.statuses) != 1 {
			t.Errorf("persisted statuses = %v, want only the dir scanned", st.statuses)
		}
		if got := h.auditOutcomes("workspace.scan"); len(got) != 1 || got[0] != "success" {
			t.Errorf("workspace.scan audit outcomes = %v, want [success]", got)
		}
	})
	t.Run("a repo with no runner stops the fan-out with 503 and audits what already scanned", func(t *testing.T) {
		st := base()
		h, w := run(t, st, false, attach(dirID, repoID))
		if w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonSourceScanNoRunner {
			t.Fatalf("= %d %s, want 503 %s", w.Code, w.Body.String(), reasonSourceScanNoRunner)
		}
		if got := h.auditOutcomes("workspace.scan"); len(got) != 1 || got[0] != "failure" {
			t.Errorf("workspace.scan audit outcomes = %v, want [failure]", got)
		}
	})
	t.Run("attached-source load failure is a 500", func(t *testing.T) {
		st := base()
		st.listErr = errors.New("db down")
		if _, w := run(t, st, false, attach(dirID)); w.Code != http.StatusInternalServerError {
			t.Errorf("= %d %s, want 500", w.Code, w.Body.String())
		}
	})
	t.Run("reload failure after an all-inline scan is a 500", func(t *testing.T) {
		st := base()
		st.freshErr = errors.New("db down")
		if _, w := run(t, st, false, attach(dirID)); w.Code != http.StatusInternalServerError {
			t.Errorf("= %d %s, want 500", w.Code, w.Body.String())
		}
	})
}
