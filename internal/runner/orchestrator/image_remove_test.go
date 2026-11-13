// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// imageRemovingSubstrate is a substrate with a local image store, as the Docker one is.
type imageRemovingSubstrate struct {
	*fakeSubstrate
	removed []string
}

func (i *imageRemovingSubstrate) ImageRemove(_ context.Context, ref string) error {
	i.removed = append(i.removed, ref)
	return nil
}

// The daemon's Orchestrator reclaims a superseded image through the first local substrate that can, and
// a deployment with none removes nothing.
func TestOrchestrator_ImageRemove_ReachesTheLocalImageStore(t *testing.T) {
	k8s := &fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC3}}
	docker := &imageRemovingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}
	if err := New(k8s, docker).ImageRemove(context.Background(), "wardyn-workspace/w:old"); err != nil {
		t.Fatalf("ImageRemove: %v", err)
	}
	if !slices.Equal(docker.removed, []string{"wardyn-workspace/w:old"}) {
		t.Fatalf("the Docker substrate removed %v, want the superseded ref", docker.removed)
	}
	if err := New(k8s).ImageRemove(context.Background(), "wardyn-workspace/w:old"); err != nil {
		t.Fatalf("a deployment with no local image store: %v, want nothing removed and no error", err)
	}
}
