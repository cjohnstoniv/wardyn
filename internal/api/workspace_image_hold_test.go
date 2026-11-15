// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run launch holds the workspace image it selected, a cached one or one it just built, until its dispatch
// returns: reclaiming that image as superseded keeps it while the launch holds it, and removes it once the
// launch has released it.
func TestRemoveStaleImage_KeepsAnImageALaunchHolds(t *testing.T) {
	h := newHarness(t)
	rr := &imageRemoverRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, &resolveImageStoreFake{})
	cfg.Runner = rr
	srv := New(cfg)
	removed := func() []string { rr.mu.Lock(); defer rr.mu.Unlock(); return slices.Clone(rr.removed) }
	const cached, built = "wardyn-workspace/w:cached", "wardyn-workspace/w:built"

	launch, release := runner.HoldImages(context.Background())
	if !srv.cachedImageStillPresent(launch, cached) {
		t.Fatal("a runner that cannot check presence reported the cached image absent")
	}
	rec := &types.ImageBuild{Image: types.ImageBuildImage{Ref: built}}
	if _, err := srv.commitBuiltImage(launch, types.Workspace{ID: uuid.New()}, "", rec, map[string]any{}, false); err != nil {
		t.Fatal(err)
	}
	srv.removeStaleImage(context.Background(), cached, "wardyn-workspace/w:next")
	srv.removeStaleImage(context.Background(), built, "wardyn-workspace/w:next")
	if got := removed(); len(got) != 0 {
		t.Fatalf("removed %v while the launch that selected them was dispatching", got)
	}
	release()
	srv.removeStaleImage(context.Background(), cached, "wardyn-workspace/w:next")
	srv.removeStaleImage(context.Background(), built, "wardyn-workspace/w:next")
	if got := removed(); !slices.Equal(got, []string{cached, built}) {
		t.Fatalf("removed %v after the launch released them, want %v", got, []string{cached, built})
	}
}
