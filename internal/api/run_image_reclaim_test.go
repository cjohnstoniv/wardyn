// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reclaimStoreFake answers one run and a workspace list, and records a cleared sandbox ref.
type reclaimStoreFake struct {
	store.Store
	run          types.AgentRun
	getErr       error
	workspaces   []types.Workspace
	workspaceErr error
	setRefErr    error
}

func (s *reclaimStoreFake) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, s.getErr
}

func (s *reclaimStoreFake) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return s.workspaces, s.workspaceErr
}

func (s *reclaimStoreFake) SetSandboxRef(_ context.Context, _ uuid.UUID, ref string) error {
	if s.setRefErr != nil {
		return s.setRefErr
	}
	s.run.SandboxRef = ref
	return nil
}

// newReclaimServer is a server whose runner removes images and whose store answers st.
func newReclaimServer(t *testing.T, st *reclaimStoreFake) (*Server, *imageRemoverRunner) {
	t.Helper()
	h := newHarness(t)
	rr := &imageRemoverRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rr
	return New(cfg), rr
}

func removedImages(rr *imageRemoverRunner) []string {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return slices.Clone(rr.removed)
}

func TestReclaimRunImage_RemovesOnlyTheRunsOwnTag(t *testing.T) {
	id := uuid.New()
	hex64 := strings.Repeat("a", 64)
	for _, tc := range []struct {
		image  string
		remove bool
	}{
		{"wardyn-byoi/" + id.String() + ":latest", true},
		{"wardyn-devcontainer/" + id.String() + ":latest", true},
		{"wardyn-byoi/" + uuid.NewString() + ":latest", false},
		{"wardyn-workspace/w:built", false},
		{"ghcr.io/x/y:1", false},
		{"wardyn-byoi/" + id.String() + ":other", false},
		{"wardyn-byoi/reuse:" + hex64, false},
		{"wardyn-devcontainer/reuse:" + hex64, false},
		{"", false},
	} {
		t.Run(tc.image, func(t *testing.T) {
			st := &reclaimStoreFake{run: types.AgentRun{ID: id, State: types.RunCompleted, Image: tc.image}}
			srv, rr := newReclaimServer(t, st)
			srv.reclaimRunImageAfterTeardown(context.Background(), id)
			srv.WaitBackground()
			got := removedImages(rr)
			if tc.remove && !slices.Equal(got, []string{tc.image}) {
				t.Errorf("removed %v; want [%s]", got, tc.image)
			}
			if !tc.remove && len(got) != 0 {
				t.Errorf("removed %v; want nothing", got)
			}
		})
	}
}

func TestReclaimRunImage_KeepsATagALaunchHolds(t *testing.T) {
	id := uuid.New()
	tag := "wardyn-byoi/" + id.String() + ":latest"
	st := &reclaimStoreFake{run: types.AgentRun{ID: id, State: types.RunCompleted, Image: tag}}
	srv, rr := newReclaimServer(t, st)
	launch, release := runner.HoldImages(context.Background())
	runner.HoldImage(launch, tag)
	srv.reclaimRunImageAfterTeardown(context.Background(), id)
	srv.WaitBackground()
	if got := removedImages(rr); len(got) != 0 {
		t.Fatalf("removed %v while a launch held the tag", got)
	}
	release()
	srv.reclaimRunImageAfterTeardown(context.Background(), id)
	srv.WaitBackground()
	if got := removedImages(rr); !slices.Equal(got, []string{tag}) {
		t.Fatalf("removed %v after the launch released it; want [%s]", got, tag)
	}
}

func TestReclaimRunImage_SkipsALiveRunOrOneWithARef(t *testing.T) {
	id := uuid.New()
	tag := "wardyn-byoi/" + id.String() + ":latest"
	for name, st := range map[string]*reclaimStoreFake{
		"running":    {run: types.AgentRun{ID: id, State: types.RunRunning, Image: tag}},
		"has a ref":  {run: types.AgentRun{ID: id, State: types.RunKilled, Image: tag, SandboxRef: "sbx"}},
		"read fails": {run: types.AgentRun{ID: id, State: types.RunCompleted, Image: tag}, getErr: errors.New("db down")},
	} {
		t.Run(name, func(t *testing.T) {
			srv, rr := newReclaimServer(t, st)
			srv.reclaimRunImageAfterTeardown(context.Background(), id)
			srv.WaitBackground()
			if got := removedImages(rr); len(got) != 0 {
				t.Errorf("removed %v; want nothing", got)
			}
		})
	}
}

func TestClearSandboxRef_ReclaimsTheRunsOwnImage(t *testing.T) {
	id := uuid.New()
	tag := "wardyn-byoi/" + id.String() + ":latest"
	st := &reclaimStoreFake{run: types.AgentRun{ID: id, State: types.RunKilled, Image: tag, SandboxRef: "sbx"}}
	srv, rr := newReclaimServer(t, st)
	if err := srv.clearSandboxRef(context.Background(), id); err != nil {
		t.Fatalf("clearSandboxRef: %v", err)
	}
	srv.WaitBackground()
	if got := removedImages(rr); !slices.Equal(got, []string{tag}) {
		t.Fatalf("removed %v; want [%s] once", got, tag)
	}

	failing := &reclaimStoreFake{run: types.AgentRun{ID: id, State: types.RunKilled, Image: tag, SandboxRef: "sbx"}, setRefErr: fmt.Errorf("write failed")}
	srv, rr = newReclaimServer(t, failing)
	if err := srv.clearSandboxRef(context.Background(), id); err == nil {
		t.Fatal("clearSandboxRef reported success for a failed write")
	}
	srv.WaitBackground()
	if got := removedImages(rr); len(got) != 0 {
		t.Errorf("removed %v after a failed clear; want nothing", got)
	}
}

func TestReclaimRunImage_KeepsATagAWorkspaceRowRecords(t *testing.T) {
	id := uuid.New()
	tag := "wardyn-byoi/" + id.String() + ":latest"
	run := types.AgentRun{ID: id, State: types.RunCompleted, Image: tag}
	for _, tc := range []struct {
		name   string
		st     *reclaimStoreFake
		remove bool
	}{
		{"a workspace records it", &reclaimStoreFake{run: run, workspaces: []types.Workspace{{ID: uuid.New(), ImageRef: tag}}}, false},
		{"the list fails", &reclaimStoreFake{run: run, workspaceErr: errors.New("db down")}, false},
		{"a workspace names another ref", &reclaimStoreFake{run: run, workspaces: []types.Workspace{{ID: uuid.New(), ImageRef: "wardyn-byoi/other:latest"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rr := newReclaimServer(t, tc.st)
			srv.reclaimRunImageAfterTeardown(context.Background(), id)
			srv.WaitBackground()
			got := removedImages(rr)
			if tc.remove && !slices.Equal(got, []string{tag}) {
				t.Errorf("removed %v; want [%s] once", got, tag)
			}
			if !tc.remove && len(got) != 0 {
				t.Errorf("removed %v; want nothing", got)
			}
		})
	}
}
