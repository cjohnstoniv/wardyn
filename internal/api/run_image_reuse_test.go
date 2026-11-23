// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reusingBuilder is an image builder that can answer a wrap from an earlier build. It records every call.
type reusingBuilder struct {
	fakeImageBuilder
	mu sync.Mutex
	// reuse is what FinalizeBaseReused answers; announce makes it call reuse.Announce when it builds; holdRef is
	// held through reuse.Hold first.
	reuse    bool
	announce bool
	holdRef  string

	reusedBases, reusedScopes []string
	plainTags, devTags        []string
}

const reusedFakeRef = "wardyn-byoi/reuse:fake"

func (b *reusingBuilder) FinalizeBaseReused(_ context.Context, base string, reuse ImageReuse, _ io.Writer) (BuiltImage, bool, error) {
	b.mu.Lock()
	b.reusedBases = append(b.reusedBases, base)
	b.reusedScopes = append(b.reusedScopes, reuse.Scope)
	b.mu.Unlock()
	if b.holdRef != "" {
		reuse.Hold(b.holdRef)
	}
	if !b.reuse && b.announce {
		reuse.Announce()
	}
	return BuiltImage{Ref: reusedFakeRef, ID: "sha256:" + strings.Repeat("c", 64)}, b.reuse, nil
}

func (b *reusingBuilder) FinalizeBase(_ context.Context, _, tag string, _ io.Writer, _ bool) (BuiltImage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.plainTags = append(b.plainTags, tag)
	return BuiltImage{Ref: tag, ID: "sha256:" + strings.Repeat("d", 64)}, nil
}

func (b *reusingBuilder) BuildDevcontainer(_ context.Context, _, _, tag string, _ io.Writer) (BuiltImage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.devTags = append(b.devTags, tag)
	return BuiltImage{Ref: tag, ID: "sha256:" + strings.Repeat("e", 64)}, nil
}

// reuseServer is a server over b with one PENDING run created by createdBy.
func reuseServer(t *testing.T, b *reusingBuilder, createdBy string) (*Server, *harness, types.AgentRun) {
	t.Helper()
	h := newHarness(t)
	st := &pinStore{createDetachStore: newCreateDetachStore()}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = b
	run := newFinalizeRun()
	run.State, run.CreatedBy = types.RunPending, createdBy
	if _, err := st.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	return New(cfg), h, run
}

func byoiRequest(run types.AgentRun) createRunRequest {
	return createRunRequest{Agent: run.Agent, Image: "ubuntu:24.04"}
}

func TestRunImageReuse_BYOIAsksTheReusingBuilderWithTheCreator(t *testing.T) {
	b := &reusingBuilder{}
	srv, _, run := reuseServer(t, b, "sub-a")
	image, _, failed := srv.resolveCreateRunImage(t.Context(), byoiRequest(run), run, nil)
	if failed {
		t.Fatal("the BYOI wrap failed")
	}
	if len(b.reusedBases) != 1 || b.reusedBases[0] != "ubuntu:24.04" || b.reusedScopes[0] != "sub-a" {
		t.Fatalf("FinalizeBaseReused calls = bases %q scopes %q, want one for ubuntu:24.04 and sub-a", b.reusedBases, b.reusedScopes)
	}
	if len(b.plainTags) != 0 {
		t.Errorf("plain FinalizeBase ran for %q", b.plainTags)
	}
	if image != reusedFakeRef {
		t.Errorf("image = %q, want the builder's %q", image, reusedFakeRef)
	}
}

func TestRunImageReuse_RunWithNoCreatorBuildsUnderItsOwnTag(t *testing.T) {
	b := &reusingBuilder{}
	srv, _, run := reuseServer(t, b, "")
	if _, _, failed := srv.resolveCreateRunImage(t.Context(), byoiRequest(run), run, nil); failed {
		t.Fatal("the BYOI wrap failed")
	}
	if want := "wardyn-byoi/" + run.ID.String() + ":latest"; len(b.plainTags) != 1 || b.plainTags[0] != want {
		t.Fatalf("plain FinalizeBase tags = %q, want [%s]", b.plainTags, want)
	}
	if len(b.reusedBases) != 0 {
		t.Errorf("the reusing builder was asked for %q with no creator", b.reusedBases)
	}
}

func TestRunImageReuse_DevcontainerStillBuildsUnderItsOwnTag(t *testing.T) {
	for _, ref := range []string{"", "main", strings.Repeat("ab", 20)} {
		t.Run("ref "+ref, func(t *testing.T) {
			b := &reusingBuilder{reuse: true}
			srv, h, run := reuseServer(t, b, "sub-a")
			req := createRunRequest{Agent: run.Agent, DevcontainerRepo: "acme/widgets", DevcontainerRef: ref}
			if _, _, failed := srv.resolveCreateRunImage(t.Context(), req, run, nil); failed {
				t.Fatal("the devcontainer build failed")
			}
			if want := "wardyn-devcontainer/" + run.ID.String() + ":latest"; len(b.devTags) != 1 || b.devTags[0] != want {
				t.Fatalf("BuildDevcontainer tags = %q, want [%s]", b.devTags, want)
			}
			if len(b.reusedBases) != 0 {
				t.Errorf("a devcontainer build asked the reusing builder: %q", b.reusedBases)
			}
			ev := waitForRecAudit(t, h.audit, run.ID, "run.build", "success")
			if strings.Contains(string(ev.Data), "cache_hit") {
				t.Errorf("run.build row %s carries cache_hit", ev.Data)
			}
		})
	}
}

func TestRunImageReuse_ReusedBuildIsAuditedAsACacheHit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reused bool
	}{{"reused", true}, {"built", false}} {
		t.Run(tc.name, func(t *testing.T) {
			b := &reusingBuilder{reuse: tc.reused}
			srv, h, run := reuseServer(t, b, "sub-a")
			if _, _, failed := srv.resolveCreateRunImage(t.Context(), byoiRequest(run), run, nil); failed {
				t.Fatal("the BYOI wrap failed")
			}
			ev := waitForRecAudit(t, h.audit, run.ID, "run.build", "success")
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			hit, has := data["cache_hit"]
			if tc.reused && hit != true || !tc.reused && has {
				t.Errorf("row %s: cache_hit = %v (present %v), want hit=%v", ev.Data, hit, has, tc.reused)
			}
			if data["image"] != reusedFakeRef || data["image_id"] != "sha256:"+strings.Repeat("c", 64) {
				t.Errorf("row %s: want image %q and the builder's image_id", ev.Data, reusedFakeRef)
			}
		})
	}
}

func TestRunImageReuse_ThePinIsTheReusedBase(t *testing.T) {
	srv, _, _, run := pinFixture(t, pinnedVerdict(admitPinned))
	b := &reusingBuilder{}
	srv.cfg.ImageBuilder = b
	if _, _, failed := srv.resolveCreateRunImage(t.Context(), createRunRequest{Agent: run.Agent, Image: admitTagRef}, run, nil); failed {
		t.Fatal("the pinned BYOI wrap failed")
	}
	if len(b.reusedBases) != 1 || b.reusedBases[0] != admitPinned {
		t.Fatalf("reused base = %q, want the pin %q", b.reusedBases, admitPinned)
	}
}

func TestRunImageReuse_HoldIsTheLaunchHold(t *testing.T) {
	const ref = "wardyn-byoi/reuse:t"
	b := &reusingBuilder{holdRef: ref, reuse: true}
	srv, _, run := reuseServer(t, b, "sub-a")
	ctx, release := runner.HoldImages(context.Background())
	if _, _, failed := srv.resolveCreateRunImage(ctx, byoiRequest(run), run, nil); failed {
		t.Fatal("the BYOI wrap failed")
	}
	removed := 0
	remove := func(context.Context, string) error { removed++; return nil }
	if held, err := runner.RemoveUnlessHeld(context.Background(), ref, remove); err != nil || !held || removed != 0 {
		t.Fatalf("while the launch runs: held=%v err=%v removed=%d, want held and not removed", held, err, removed)
	}
	release()
	if held, err := runner.RemoveUnlessHeld(context.Background(), ref, remove); err != nil || held || removed != 1 {
		t.Fatalf("after the launch: held=%v err=%v removed=%d, want removed", held, err, removed)
	}
}

func TestRunImageReuse_AnnouncesOnlyWhenItBuilds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reused bool
		want   []string
	}{{"build", false, []string{buildingLine}}, {"reuse", true, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, log := buildAnnounceServer(t)
			srv.cfg.ImageBuilder = &reusingBuilder{reuse: tc.reused, announce: true}
			run := newFinalizeRun()
			run.CreatedBy = "sub-a"
			if _, _, failed := srv.resolveCreateRunImage(context.Background(), byoiRequest(run), run, nil); failed {
				t.Fatal("the BYOI wrap failed")
			}
			if got := log.events(); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("events = %q, want %q", got, tc.want)
			}
		})
	}
}
