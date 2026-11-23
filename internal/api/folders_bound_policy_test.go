// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A folder a run's policy names as a mount is checked where its attached workspaces hold it: by its text
// when they bind it to one runner, on this host otherwise. The inline and overlay arms leave that to the
// seed for a run that attaches workspaces.

// seedStore is the store a seed reads: each workspace by id and the whole list.
type seedStore struct {
	store.Store
	ws []types.Workspace
}

func (s seedStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return s.ws, nil }

func (s seedStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	for _, ws := range s.ws {
		if ws.ID == id {
			return ws, nil
		}
	}
	return types.Workspace{}, store.ErrNotFound
}

// serverLinkToEtc is a path this host resolves into /etc, so the host check refuses it and the text check passes it.
func serverLinkToEtc(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.Symlink("/etc", p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSeed_PolicyMountIsCheckedWhereItsAttachedHoldersPutIt(t *testing.T) {
	link, other := serverLinkToEtc(t, "proj"), serverLinkToEtc(t, "elsewhere")
	rid, rid2 := uuid.New(), uuid.New()
	named := func(name string, srcs ...types.WorkspaceSource) types.Workspace {
		return types.Workspace{ID: uuid.New(), Name: name, OwnedBy: ownerMemberSub, Sources: srcs}
	}
	bound := named("bound", boundSource(link, rid))
	unbound := named("unbound", types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: link})
	second := named("second", boundSource(link, rid2))
	elsewhere := named("elsewhere", boundSource(other, rid))

	for _, c := range []struct {
		name     string
		attached []types.Workspace
		source   string
		target   string
		refused  bool
	}{
		{"one attached workspace binds the folder", []types.Workspace{bound}, link, composerWorkspaceTarget, false},
		{"the attached workspace holds it on this host", []types.Workspace{unbound}, link, composerWorkspaceTarget, true},
		{"bound and unbound holders", []types.Workspace{bound, unbound}, link, composerWorkspaceTarget, true},
		{"two runners", []types.Workspace{bound, second}, link, composerWorkspaceTarget, true},
		{"a policy folder no attached workspace holds", []types.Workspace{bound}, other, "/home/agent/other", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			all := append([]types.Workspace{elsewhere}, c.attached...)
			s := &Server{cfg: Config{Store: seedStore{ws: all}}}
			ro := true
			spec := types.RunPolicySpec{MinConfinementClass: types.CC1, WorkspaceMounts: []types.WorkspaceMount{{Source: c.source, Target: c.target, ReadOnly: &ro}}}
			var req createRunRequest
			for _, ws := range c.attached {
				req.Workspaces = append(req.Workspaces, client.WorkspaceSelection{WorkspaceID: ws.ID.String()})
			}
			_, refusal := s.seedRequestWorkspace(context.Background(), &spec, &req)
			if !c.refused {
				if refusal != nil {
					t.Fatalf("refused %d %+v, want the folder checked by its text", refusal.status, refusal.body)
				}
				return
			}
			if refusal == nil {
				t.Fatal("not refused, want the folder resolved on this host")
			}
			if refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonWorkspaceSeedPolicyConflict || !strings.Contains(refusal.body.Error, "/etc") {
				t.Errorf("refusal = %d %+v, want 422 %s naming /etc", refusal.status, refusal.body, reasonWorkspaceSeedPolicyConflict)
			}
			if c.source == other && !strings.Contains(refusal.body.Error, "workspace_mounts[1]") {
				t.Errorf("refusal = %s, want workspace_mounts[1]: the seeded folder passed on its text, the policy folder did not", refusal.body.Error)
			}
		})
	}
}

func TestValidateRunPolicyBeforeSeed_ResolvesOnlyARunThatAttachesNoWorkspace(t *testing.T) {
	id := uuid.New()
	attached := uuid.NewString()
	requests := []struct {
		name   string
		req    createRunRequest
		attach bool
	}{
		{"no workspace", createRunRequest{}, false},
		{"an id the seed drops", createRunRequest{Workspaces: []client.WorkspaceSelection{{WorkspaceID: "not-a-uuid"}}}, false},
		{"a workspace in workspaces", createRunRequest{Workspaces: []client.WorkspaceSelection{{WorkspaceID: attached}}}, true},
		{"workspace_id", createRunRequest{WorkspaceID: &id}, true},
	}
	ro := true
	for _, src := range []string{serverLinkToEtc(t, "proj"), "/etc/ssh"} {
		spec := types.RunPolicySpec{MinConfinementClass: types.CC1, WorkspaceMounts: []types.WorkspaceMount{{Source: src, Target: composerWorkspaceTarget, ReadOnly: &ro}}}
		for _, c := range requests {
			err := validateRunPolicyBeforeSeed(c.req, spec)
			wantErr := !c.attach || strings.HasPrefix(src, "/etc/")
			if wantErr {
				if err == nil || !strings.Contains(err.Error(), "/etc") {
					t.Errorf("%s, source %s: err = %v, want a refusal naming /etc", c.name, src, err)
				}
			} else if err != nil {
				t.Errorf("%s, source %s: err = %v, want the spelling accepted until the seed", c.name, src, err)
			}
		}
	}
}

func TestInBoundFolder(t *testing.T) {
	const f = "/Users/wardyn-bound-member/src/app"
	r1, r2 := uuid.New(), uuid.New()
	local := func(p string) types.WorkspaceSource {
		return types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: p}
	}
	for _, c := range []struct {
		name string
		srcs []types.WorkspaceSource
		path string
		want bool
	}{
		{"the folder itself", []types.WorkspaceSource{boundSource(f, r1)}, f, true},
		{"a path inside", []types.WorkspaceSource{boundSource(f, r1)}, f + "/out/logs", true},
		{"a shared text prefix is not inside", []types.WorkspaceSource{boundSource(f, r1)}, f + "-other/out", false},
		{"the folder's parent", []types.WorkspaceSource{boundSource(f, r1)}, "/Users/wardyn-bound-member/src", false},
		{"an unbound folder", []types.WorkspaceSource{local(f)}, f + "/out", false},
		{"nested folders on one runner", []types.WorkspaceSource{boundSource(f, r1), boundSource(f+"/out", r1)}, f + "/out/x", true},
		{"a nested unbound folder", []types.WorkspaceSource{boundSource(f, r1), local(f + "/out")}, f + "/out/x", false},
		{"nested folders on two runners", []types.WorkspaceSource{boundSource(f, r1), boundSource(f+"/out", r2)}, f + "/out/x", false},
		{"a repo source", []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: f}}, f, false},
	} {
		if got := inBoundFolder(types.Workspace{Sources: c.srcs}, c.path); got != c.want {
			t.Errorf("%s: inBoundFolder(%q) = %v, want %v", c.name, c.path, got, c.want)
		}
	}
}
