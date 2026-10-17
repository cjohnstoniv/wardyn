// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// archImageStore serves runner rows and records the image SetRunImage persists.
type archImageStore struct {
	*buildEventStore
	runners map[uuid.UUID]types.Runner
	image   string
	reads   int
}

func (s *archImageStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	s.reads++
	return s.runners[id], nil
}

func (s *archImageStore) RunnerArchCounts(context.Context) (map[string]int, error) {
	counts := map[string]int{}
	for _, rn := range s.runners {
		counts[rn.Arch]++
	}
	return counts, nil
}

func (s *archImageStore) SetRunImage(_ context.Context, _ uuid.UUID, image string) error {
	s.image = image
	return nil
}

var archTestImages = map[string]string{
	"base": "m/base:1", "claude-code": "m/cc:1", "claude-code+codex-cli": "m/set:1",
}

var archTestVariants = map[string]map[string]string{"arm64": {
	"base": "m/base:1-arm64", "claude-code": "m/cc:1-arm64", "claude-code+codex-cli": "m/set:1-arm64",
}}

// archImageServer is a server whose store holds one runner of the given arch.
func archImageServer(t *testing.T, arch string) (srv *Server, st *archImageStore, bound types.AgentRun) {
	t.Helper()
	rid := uuid.New()
	st = &archImageStore{buildEventStore: &buildEventStore{log: &buildEventLog{}}, runners: map[uuid.UUID]types.Runner{rid: {ID: rid, Arch: arch}}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.ImageBuilder = buildEventBuilder{log: st.log}
	cfg.AgentImages, cfg.AgentImageVariants = archTestImages, archTestVariants
	return New(cfg), st, types.AgentRun{ID: uuid.New(), RunnerID: &rid}
}

func archTestTools(ids ...string) []client.IncludedTool {
	var tools []client.IncludedTool
	for _, id := range ids {
		tools = append(tools, client.IncludedTool{ID: id, Kind: client.IncludedToolHarness})
	}
	return tools
}

// TestResolveCreateRunImage_ArchVariant: a run bound to a runner of an architecture with a `<key>@<arch>`
// entry starts from that entry, persisted as the run's image; any other run keeps the plain ref, and a
// BYOI, devcontainer or workspace base image is never swapped.
func TestResolveCreateRunImage_ArchVariant(t *testing.T) {
	legacy := createRunRequest{Agent: "claude-code"}
	baseOnly := createRunRequest{ModeContract: client.RunModeContractV09}
	oneHarness := createRunRequest{Tools: archTestTools("claude-code")}
	harnessSet := createRunRequest{Tools: archTestTools("claude-code", "codex-cli")}
	for _, tc := range []struct {
		name   string
		arch   string
		unbond bool
		req    createRunRequest
		ws     []types.Workspace
		want   string
	}{
		{name: "arm64 legacy agent", arch: "arm64", req: legacy, want: "m/cc:1-arm64"},
		{name: "arm64 no harness is the base key", arch: "arm64", req: baseOnly, want: "m/base:1-arm64"},
		{name: "arm64 one harness", arch: "arm64", req: oneHarness, want: "m/cc:1-arm64"},
		{name: "arm64 harness set", arch: "arm64", req: harnessSet, want: "m/set:1-arm64"},
		{name: "amd64 keeps the plain ref", arch: "amd64", req: legacy, want: "m/cc:1"},
		{name: "no reported arch keeps the plain ref", arch: "", req: legacy, want: "m/cc:1"},
		{name: "a run on no runner keeps the plain ref", arch: "arm64", unbond: true, req: legacy, want: "m/cc:1"},
		{name: "BYOI is never swapped", arch: "arm64", req: createRunRequest{Agent: "claude-code", Image: "ubuntu:24.04"}, want: "wardyn-byoi/"},
		{name: "devcontainer is never swapped", arch: "arm64", req: createRunRequest{Agent: "claude-code", DevcontainerRepo: "acme/widgets"}, want: "wardyn-devcontainer/"},
		{name: "workspace base image is never swapped", arch: "arm64", req: legacy,
			ws:   []types.Workspace{{ID: uuid.New(), BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"}}},
			want: "wardyn-byoi/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, run := archImageServer(t, tc.arch)
			if tc.unbond {
				run.RunnerID = nil
			}
			got, failed := srv.resolveCreateRunImage(context.Background(), tc.req, run, tc.ws)
			if failed {
				t.Fatal("resolveCreateRunImage failed")
			}
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
			if st.image != got {
				t.Errorf("persisted image = %q, want the one that ran, %q", st.image, got)
			}
		})
	}
}

// TestArchImage: only a known arch and key with an entry swaps the ref.
func TestArchImage(t *testing.T) {
	v := map[string]map[string]string{"arm64": {"base": "b-arm"}}
	for _, tc := range []struct{ image, key, arch, want string }{
		{"b", "base", "arm64", "b-arm"},
		{"b", "base", "amd64", "b"},
		{"b", "base", "", "b"},
		{"c", "other", "arm64", "c"},
		{"c", "", "arm64", "c"},
	} {
		if got := archImage(tc.image, tc.key, tc.arch, v); got != tc.want {
			t.Errorf("archImage(%q, %q, %q) = %q, want %q", tc.image, tc.key, tc.arch, got, tc.want)
		}
	}
	if got := archImage("b", "base", "arm64", nil); got != "b" {
		t.Errorf("no variants: %q", got)
	}
}

// TestRunArch_FollowsTheBoundRunner: a queued run rebound to a fallback runner of the other architecture
// takes that runner's variant, because the arch is read from the run's row at resolve time.
func TestRunArch_FollowsTheBoundRunner(t *testing.T) {
	srv, st, run := archImageServer(t, "amd64")
	other := uuid.New()
	st.runners[other] = types.Runner{ID: other, Arch: "arm64"}
	if got := srv.runArch(context.Background(), run); got != "amd64" {
		t.Fatalf("runArch = %q, want amd64", got)
	}
	run.RunnerID = &other // what StartQueuedRun's started row carries after a fallback
	if got := srv.runArch(context.Background(), run); got != "arm64" {
		t.Fatalf("after the rebind runArch = %q, want arm64", got)
	}
	img, failed := srv.resolveCreateRunImage(context.Background(), createRunRequest{Agent: "claude-code"}, run, nil)
	if failed || img != "m/cc:1-arm64" {
		t.Errorf("image = %q (failed %v), want the arm64 variant", img, failed)
	}
}

// TestFinishCreateRunLaunch_BoundArchRunnerWiring drives the launch itself: a run bound to a runner of
// the architecture wardynd is not takes that architecture's `@arch` image, persisted on the run row, and
// the setup status names the keys that architecture still has no image for.
func TestFinishCreateRunLaunch_BoundArchRunnerWiring(t *testing.T) {
	foreign := "arm64"
	if runtime.GOARCH == "arm64" {
		foreign = "amd64"
	}
	srv, st, run := archImageServer(t, foreign)
	srv.cfg.Runner = nil // the launch resolves and persists the image; nothing dispatches
	srv.cfg.AgentImageVariants = map[string]map[string]string{foreign: {"claude-code": "m/cc:1-" + foreign}}
	srv.finishCreateRunLaunch(context.Background(), createRunLaunch{req: createRunRequest{Agent: "claude-code"}, created: run})
	if want := "m/cc:1-" + foreign; st.image != want {
		t.Fatalf("persisted image = %q, want %q", st.image, want)
	}
	chk, ok := srv.runnerArchImagesRow(context.Background())
	if !ok || chk.ID != "runner_arch_images" || !strings.Contains(chk.Detail, "no "+foreign+" image: base, claude-code+codex-cli.") {
		t.Fatalf("setup row = %+v, %v; want the keys with no %s image (claude-code is covered)", chk, ok, foreign)
	}
}

// With no `@arch` entries at all the runner row is never read.
func TestRunArch_NoVariantsReadsNoRunner(t *testing.T) {
	srv, st, run := archImageServer(t, "arm64")
	srv.cfg.AgentImageVariants = nil
	if got := srv.runArch(context.Background(), run); got != "" || st.reads != 0 {
		t.Fatalf("runArch = %q after %d runner reads, want \"\" and none", got, st.reads)
	}
}
