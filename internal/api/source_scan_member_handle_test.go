// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A member's folder is opened where it is checked, one folder at a time from the top: a folder on
// the way that the server can pass through but not read refuses the scan with the member refusal,
// and nothing is read. A scan that read the folder by its path would pass the check and read it.
func TestScanAttachedSources_MemberSourceUnderAPassThroughFolderIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions that bind the scanning user")
	}
	root, inside := memberProjectRoot(t)
	if err := os.Chmod(root, 0o311); err != nil { // pass through, no read
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	h := newHarness(t)
	id := uuid.New()
	fake := &partialScanStore{sources: map[uuid.UUID]types.Source{id: {ID: id, Kind: types.SourceLocalDir, Locator: inside}}}
	cfg := baseTestConfig(h, fake)
	cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
	srv := New(cfg)
	ws := types.Workspace{ID: uuid.New(), OwnedBy: ownerMemberSub, Attachments: []types.WorkspaceAttachment{{SourceID: &id}}}
	rec := httptest.NewRecorder()
	srv.scanAttachedSources(rec, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/scan", nil), ws)
	if rec.Code != http.StatusUnprocessableEntity || reasonOf(t, rec) != reasonWorkspaceSourceMountNotAllowed {
		t.Fatalf("code = %d, want 422 %s; body=%s", rec.Code, reasonWorkspaceSourceMountNotAllowed, rec.Body.String())
	}
	if len(fake.scanned) != 0 {
		t.Fatalf("scanned = %v, want nothing read", fake.scanned)
	}
}
