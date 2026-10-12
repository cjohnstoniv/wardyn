// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// lostSwapStore answers every compare-and-swap with applied=false: another
// build moved image_ref first.
type lostSwapStore struct{ store.Store }

func (lostSwapStore) SetWorkspaceBuiltImage(context.Context, uuid.UUID, string, string, string, *types.ImageBuild) (bool, error) {
	return false, nil
}

// errSwapStore fails every compare-and-swap with a store error.
type errSwapStore struct{ store.Store }

func (errSwapStore) SetWorkspaceBuiltImage(context.Context, uuid.UUID, string, string, string, *types.ImageBuild) (bool, error) {
	return false, errors.New("x")
}

// TestResolveWorkspaceImage_WriteErrorServesItsImageAndKeepsTheOldTag: a store
// error on the swap is non-fatal, rides the run.build row as cache_warn, and
// leaves the old tag alone — nothing was swapped.
func TestResolveWorkspaceImage_WriteErrorServesItsImageAndKeepsTheOldTag(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, errSwapStore{})
	cfg.ImageBuilder = &lostSwapBuilder{}
	rr := &imageRemoverRunner{fakeRunner: &fakeRunner{}}
	cfg.Runner = rr
	srv := New(cfg)
	generated := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	ws := types.Workspace{ID: uuid.New(), Profile: mustJSON(generated), ImageRef: "wardyn-workspace/old-id:stale", BuiltProfileHash: "stale"}
	runID := uuid.New()

	got, ok := srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil)
	if !ok || got == "" || got == ws.ImageRef {
		t.Fatalf("resolveWorkspaceImage = (%q, %v), want the lane's own built image", got, ok)
	}
	ev := waitForRecAudit(t, h.audit, runID, "run.build", "success")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["cache_warn"] != "x" {
		t.Errorf("run.build data = %v, want cache_warn=x", data)
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.removed) != 0 {
		t.Errorf("ImageRemove saw %v after a failed write, want no call", rr.removed)
	}
}

// TestResolveWorkspaceImage_LostSwapServesItsOwnImage: a run-launch build that
// loses the swap serves its own run, audits cache_warn "superseded", and must
// not reclaim the old tag — the winning build may now own it (design §7).
func TestResolveWorkspaceImage_LostSwapServesItsOwnImage(t *testing.T) {
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
		HasDevcontainer: true,
	}
	const oldRef = "wardyn-workspace/old-id:stale"
	repoSrc := []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Ref: "main"}}
	generated := profile
	generated.HasDevcontainer = false
	for _, tc := range []struct {
		name string
		ws   types.Workspace
	}{
		{"generated", types.Workspace{Profile: mustJSON(generated)}},
		{"byoi", types.Workspace{BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"}}},
		{"repo", types.Workspace{Sources: repoSrc, Profile: mustJSON(profile)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := baseTestConfig(h, lostSwapStore{})
			cfg.ImageBuilder = &lostSwapBuilder{}
			rr := &imageRemoverRunner{fakeRunner: &fakeRunner{}}
			cfg.Runner = rr
			srv := New(cfg)
			ws := tc.ws
			ws.ID = uuid.New()
			ws.ImageRef = oldRef // stale: its hash matches nothing, so the lane rebuilds
			ws.BuiltProfileHash = "stale"
			runID := uuid.New()

			got, ok := srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil)
			if !ok || got == "" || got == oldRef {
				t.Fatalf("resolveWorkspaceImage = (%q, %v), want the lane's own built image", got, ok)
			}
			ev := waitForRecAudit(t, h.audit, runID, "run.build", "success")
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["cache_warn"] != "superseded" || data["image"] != got {
				t.Errorf("run.build data = %v, want cache_warn=superseded and image=%q", data, got)
			}
			rr.mu.Lock()
			defer rr.mu.Unlock()
			if len(rr.removed) != 0 {
				t.Errorf("ImageRemove saw %v after a lost swap, want no call", rr.removed)
			}
		})
	}
}

// lostSwapBuilder builds all three lanes without a daemon.
type lostSwapBuilder struct{ capturingByoiImageBuilder }

func (b *lostSwapBuilder) BuildFromDevcontainerFiles(_ context.Context, _ map[string]string, tag string, _ io.Writer) (BuiltImage, error) {
	return BuiltImage{Ref: "built/" + tag}, nil
}

func (b *lostSwapBuilder) BuildDevcontainer(_ context.Context, _, _, tag string, _ io.Writer) (BuiltImage, error) {
	return BuiltImage{Ref: "built/" + tag}, nil
}
