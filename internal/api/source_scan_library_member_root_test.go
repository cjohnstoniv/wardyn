// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The library door names no owner. A folder inside a member workspace root that stands as a link to a
// folder outside it is refused there as a member's own scan refuses it, and nothing is read: a scan
// by path would follow the link and put the outside folder's names into every workspace that
// attaches the source. The refused folder's row says error, and its audit row carries the reason.
// A plain folder inside the root is scanned.
func TestHandleScanSource_AFolderInAMemberRootIsNotReadThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root, inside := memberProjectRoot(t)
	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".env.example"), []byte("OUTSIDE_TOKEN=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	linkID, plainID := uuid.New(), uuid.New()
	st := &scanSourceStore{sources: map[uuid.UUID]types.Source{
		linkID:  {ID: linkID, Kind: types.SourceLocalDir, Locator: link},
		plainID: {ID: plainID, Kind: types.SourceLocalDir, Locator: inside},
	}}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/sources/"+linkID.String()+"/scan", adminToken, "")
	if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonWorkspaceSourceMountNotAllowed {
		t.Fatalf("a linked folder in a member root = %d %s, want 422 %s", w.Code, w.Body.String(), reasonWorkspaceSourceMountNotAllowed)
	}
	if got := st.statuses[linkID]; got != types.WorkspaceError {
		t.Fatalf("the linked folder's scan wrote status %q, want %q", got, types.WorkspaceError)
	}
	if !slices.ContainsFunc(h.audit.snapshot(), func(ev types.AuditEvent) bool {
		var data struct{ Detail, Reason string }
		_ = json.Unmarshal(ev.Data, &data)
		return ev.Action == "source.scan" && ev.Outcome == "failure" && ev.Target == linkID.String() &&
			data.Reason == reasonWorkspaceSourceMountNotAllowed && strings.Contains(data.Detail, "member workspace mount")
	}) {
		t.Errorf("no failed source.scan audit row names the refused source with reason %s and its sentence", reasonWorkspaceSourceMountNotAllowed)
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/sources/"+plainID.String()+"/scan", adminToken, ""); w.Code != http.StatusOK || st.statuses[plainID] != types.WorkspaceScanned {
		t.Fatalf("a plain folder in a member root = %d %s (status %q), want 200 and scanned", w.Code, w.Body.String(), st.statuses[plainID])
	}
}

// An operator's own workspace is the same door for such a folder: its scan lands on the folder's one
// library row, which a member's workspace attaching the same folder reads too. The link is refused
// there as well and nothing is read, and a plain folder inside the root is scanned.
func TestScanAttachedSources_AnOperatorWorkspaceFolderInAMemberRootIsNotReadThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root, inside := memberProjectRoot(t)
	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".env.example"), []byte("OUTSIDE_TOKEN=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		dir  string
		code int
	}{
		"a link out of the root": {link, http.StatusUnprocessableEntity},
		"a plain folder":         {inside, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			id := uuid.New()
			fake := &partialScanStore{sources: map[uuid.UUID]types.Source{id: {ID: id, Kind: types.SourceLocalDir, Locator: tc.dir}}}
			cfg := baseTestConfig(newHarness(t), fake)
			cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
			srv := New(cfg)
			ws := types.Workspace{ID: uuid.New(), Attachments: []types.WorkspaceAttachment{{SourceID: &id}}} // OwnedBy "": an operator's
			rec := httptest.NewRecorder()
			srv.scanAttachedSources(rec, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/scan", nil), ws)
			if rec.Code != tc.code {
				t.Fatalf("code = %d, want %d; body=%s", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code == http.StatusOK {
				if !slices.Equal(fake.scanned, []uuid.UUID{id}) {
					t.Errorf("scanned = %v, want the plain folder", fake.scanned)
				}
				return
			}
			if reasonOf(t, rec) != reasonWorkspaceSourceMountNotAllowed || len(fake.scanned) != 0 {
				t.Errorf("reason %s, scanned %v; want %s and nothing read", rec.Body.String(), fake.scanned, reasonWorkspaceSourceMountNotAllowed)
			}
		})
	}
}
