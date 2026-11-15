// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
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

// staleHolder is a member workspace named payroll that attaches source id, owned by alice-sub.
func staleHolder(id uuid.UUID) types.Workspace {
	return types.Workspace{ID: uuid.New(), Name: "payroll", OwnedBy: "alice-sub", Attachments: []types.WorkspaceAttachment{{SourceID: &id}}}
}

// The library door holds a folder to every member whose workspace attaches it. With the member roots
// removed since onboarding no root holds the folder, which a scan by its path would read through its
// link, so it is refused with the workspace and its owner named for the operator, the row says error,
// and the audit row carries the sentence. The same folder that no member workspace attaches is still
// read by its path, and a workspace list that cannot be read reads nothing.
func TestHandleScanSource_AWorkspaceWhoseOwnersRootsNoLongerHoldTheFolderBlocksTheScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root, _ := memberProjectRoot(t)
	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	st := &scanSourceStore{sources: map[uuid.UUID]types.Source{id: {ID: id, Kind: types.SourceLocalDir, Locator: link}}, workspaces: []types.Workspace{staleHolder(id)}}
	h := newHarness(t)
	srv := New(baseTestConfig(h, st)) // no member roots set: removed since onboarding

	w := do(t, srv, http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", adminToken, "")
	if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonWorkspaceSourceMountNotAllowed {
		t.Fatalf("a folder a stale member workspace attaches = %d %s, want 422 %s", w.Code, w.Body.String(), reasonWorkspaceSourceMountNotAllowed)
	}
	for _, want := range []string{"payroll", "alice-sub", "none set"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("refusal %s does not name %q", w.Body.String(), want)
		}
	}
	if st.statuses[id] != types.WorkspaceError {
		t.Errorf("status %q, want %q", st.statuses[id], types.WorkspaceError)
	}
	if !slices.ContainsFunc(h.audit.snapshot(), func(ev types.AuditEvent) bool {
		var data struct{ Detail, Reason string }
		_ = json.Unmarshal(ev.Data, &data)
		return ev.Action == "source.scan" && ev.Outcome == "failure" && ev.Target == id.String() && strings.Contains(data.Detail, "payroll")
	}) {
		t.Error("no failed source.scan audit row names the workspace that blocks the scan")
	}

	st.workspaces, st.statuses = nil, nil
	if w := do(t, srv, http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", adminToken, ""); w.Code != http.StatusOK || st.statuses[id] != types.WorkspaceScanned {
		t.Fatalf("the same folder no member workspace attaches = %d %s (status %q), want 200 scanned by its path", w.Code, w.Body.String(), st.statuses[id])
	}

	st.statuses, st.wsListErr = nil, errors.New("store down")
	if w := do(t, srv, http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", adminToken, ""); w.Code != http.StatusInternalServerError || st.statuses[id] != "" {
		t.Fatalf("an unreadable workspace list = %d %s (status %q), want 500 and nothing written", w.Code, w.Body.String(), st.statuses[id])
	}
}

// A member's scan of their own workspace is blocked by another person's workspace that attaches the
// same folder outside that person's roots. The member is told a workspace whose owner's roots no
// longer hold the folder blocks it, not its name or owner; an operator scanning the same workspace
// reads both, and so does the audit row.
func TestScanAttachedSources_AnotherPersonsStaleWorkspaceIsNamedOnlyToAnOperator(t *testing.T) {
	root, inside := memberProjectRoot(t)
	id := uuid.New()
	mine := types.Workspace{ID: uuid.New(), Name: "mine", OwnedBy: ownerMemberSub, Attachments: []types.WorkspaceAttachment{{SourceID: &id}}}
	for name, tc := range map[string]struct {
		req   *http.Request
		named bool
	}{
		"the member":  {httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+mine.ID.String()+"/scan", nil).WithContext(memberCtxFor(ownerMemberSub)), false},
		"an operator": {httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+mine.ID.String()+"/scan", nil).WithContext(adminCtx()), true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			fake := &partialScanStore{sources: map[uuid.UUID]types.Source{id: {ID: id, Kind: types.SourceLocalDir, Locator: inside}}, workspaces: []types.Workspace{mine, staleHolder(id)}}
			cfg := baseTestConfig(h, fake)
			cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}, RootsByPrincipal: map[string][]string{"alice-sub": {filepath.Join(filepath.Dir(root), "alice-now")}}}
			rec := httptest.NewRecorder()
			New(cfg).scanAttachedSources(rec, tc.req, mine)
			if rec.Code != http.StatusUnprocessableEntity || reasonOf(t, rec) != reasonWorkspaceSourceMountNotAllowed || len(fake.scanned) != 0 {
				t.Fatalf("code = %d %s, scanned %v; want 422 %s and nothing read", rec.Code, rec.Body.String(), fake.scanned, reasonWorkspaceSourceMountNotAllowed)
			}
			body := rec.Body.String()
			if named := strings.Contains(body, "payroll") && strings.Contains(body, "alice-sub"); named != tc.named {
				t.Errorf("refusal %s names the workspace and its owner = %v, want %v", body, named, tc.named)
			}
			if !tc.named && !strings.Contains(body, "an operator can see which workspace in the audit log") {
				t.Errorf("refusal %s does not send the member to an operator", body)
			}
			if !slices.ContainsFunc(h.audit.snapshot(), func(ev types.AuditEvent) bool {
				return ev.Action == "workspace.scan" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), "payroll")
			}) {
				t.Error("no failed workspace.scan audit row names the workspace that blocks the scan")
			}
		})
	}
}

// The library door opens a folder inside a member root one folder at a time from the top, as the
// member's door does: a folder on the way that the server can pass through but not read refuses the
// scan, and nothing is read. An open of the checked path that follows links would pass and read it.
func TestHandleScanSource_AFolderUnderAPassThroughFolderIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions that bind the scanning user")
	}
	root, inside := memberProjectRoot(t)
	if err := os.Chmod(root, 0o311); err != nil { // pass through, no read
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	id := uuid.New()
	st := &scanSourceStore{sources: map[uuid.UUID]types.Source{id: {ID: id, Kind: types.SourceLocalDir, Locator: inside}}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
	w := do(t, New(cfg), http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", adminToken, "")
	if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonWorkspaceSourceMountNotAllowed || st.statuses[id] == types.WorkspaceScanned {
		t.Fatalf("code = %d %s (status %q), want 422 %s and nothing scanned", w.Code, w.Body.String(), st.statuses[id], reasonWorkspaceSourceMountNotAllowed)
	}
}
