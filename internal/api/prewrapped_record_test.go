// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// prewrapRecordStore serves one ready prewrapped row and records the arches a run is given.
type prewrapRecordStore struct {
	*recordLLMModeStore
	row types.BaseImageEntry
}

func (s *prewrapRecordStore) GetBaseImageByRef(_ context.Context, _ string, ref string) (types.BaseImageEntry, error) {
	if ref != s.row.Image {
		return types.BaseImageEntry{}, store.ErrNotFound
	}
	return s.row, nil
}

func (s *prewrapRecordStore) SetRunImageArches(_ context.Context, id uuid.UUID, arches []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	r.ImageArches = arches
	s.runs[id] = r
	return nil
}

// A recording and a confined replay of a workspace on a prewrapped image run it as is, and are placed by
// its checked architectures like any run on it: recorded on the run before dispatch (design final §3).
func TestLaunchRecordRun_PrewrappedImageCarriesItsArches(t *testing.T) {
	const ref = "ghcr.io/acme/sdk@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	for _, confined := range []bool{false, true} {
		for _, base := range []*types.WorkspaceBaseImage{{Kind: types.BaseImageKindPrewrapped, Image: ref}, nil} {
			h := newHarness(t)
			ws := scanSeededPrivateWorkspace(types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: t.TempDir()})
			ws.BaseImage = base
			fake := &prewrapRecordStore{recordLLMModeStore: newRecordLLMModeStore(ws), row: types.BaseImageEntry{
				Kind: types.BaseImageKindPrewrapped, Image: ref,
				Check: &types.BaseImageCheck{Status: types.BaseImageCheckReady, AcceptedArches: []string{"amd64"}},
			}}
			cfg := baseTestConfig(h, fake)
			fr := &fakeRunner{}
			cfg.Runner = fr
			cfg.Broker = h.broker
			cfg.RequireOperatorSetEgress = false
			run, _, err := New(cfg).launchRecordRun(context.Background(), "alice@example.com", ws, "build", "build", confined)
			if err != nil {
				t.Fatalf("confined=%v launchRecordRun: %v", confined, err)
			}
			fr.mu.Lock()
			spec, calls := fr.lastSpec, fr.createCalls
			fr.mu.Unlock()
			stored, _ := fake.GetRun(context.Background(), run.ID)
			want := []string(nil)
			if base != nil {
				want = []string{"amd64"}
				if spec.Image != ref {
					t.Errorf("confined=%v image = %q, want %s as is", confined, spec.Image, ref)
				}
			}
			if calls != 1 || !slices.Equal(spec.ImageArches, want) || !slices.Equal(stored.ImageArches, want) {
				t.Errorf("confined=%v base=%v: %d dispatches, spec arches %v, run arches %v, want 1 and %v",
					confined, base != nil, calls, spec.ImageArches, stored.ImageArches, want)
			}
		}
	}
}
