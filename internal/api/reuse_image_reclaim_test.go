// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reuseReclaimBuilder is an ImageBuilder (nil: never called) that lists and removes reuse images from memory.
type reuseReclaimBuilder struct {
	ImageBuilder
	images      []ReuseImage
	listErr     error
	removeErr   map[string]error
	panicList   bool
	mu          sync.Mutex
	removed     []string
	listsCalled int
}

func (b *reuseReclaimBuilder) ListReuseImages(context.Context) ([]ReuseImage, error) {
	b.mu.Lock()
	b.listsCalled++
	b.mu.Unlock()
	if b.panicList {
		panic("list blew up")
	}
	return b.images, b.listErr
}

func (b *reuseReclaimBuilder) RemoveReuseImage(_ context.Context, ref string) error {
	if err := b.removeErr[ref]; err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removed = append(b.removed, ref)
	return nil
}

func (b *reuseReclaimBuilder) lists() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listsCalled
}

func (b *reuseReclaimBuilder) removedRefs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.removed...)
}

// reuseUseStore answers the two reads the reclaim makes; every other Store method would panic on the nil embed.
type reuseUseStore struct {
	store.Store
	rows       []store.RunImageUse
	useErr     error
	useCalls   int
	workspaces []types.Workspace
	wsErr      error
}

func (s *reuseUseStore) RunImageUse(context.Context, []string) ([]store.RunImageUse, error) {
	s.useCalls++
	return s.rows, s.useErr
}

func (s *reuseUseStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return s.workspaces, s.wsErr
}

// plainWorkspaceStore has no RunImageUse, so the reclaim has nothing to read the runs with.
type plainWorkspaceStore struct{ store.Store }

func (plainWorkspaceStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, nil
}

var (
	reclaimNow    = time.Now().UTC().Truncate(time.Second)
	reclaimOld    = reclaimNow.Add(-8 * 24 * time.Hour)
	reclaimRecent = reclaimNow.Add(-6 * 24 * time.Hour)
)

const reclaimRef = "wardyn-byoi/reuse:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func reuseReclaimServer(t *testing.T, st store.Store) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Now = func() time.Time { return reclaimNow }
	return New(cfg), h
}

func reclaimAudits(h *harness) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "image.reuse.reclaim" {
			out = append(out, ev)
		}
	}
	return out
}

func oldUnused(ref string) (*reuseReclaimBuilder, *reuseUseStore) {
	return &reuseReclaimBuilder{images: []ReuseImage{{Ref: ref, Created: reclaimOld}}},
		&reuseUseStore{rows: []store.RunImageUse{{Ref: ref, LastCreated: reclaimOld}}}
}

func TestReclaimReuseImages_RemovesAnOldUnusedTagAndAuditsIt(t *testing.T) {
	rb, st := oldUnused(reclaimRef)
	srv, h := reuseReclaimServer(t, st)
	if err := srv.reclaimReuseImages(t.Context(), rb, reclaimNow); err != nil {
		t.Fatalf("reclaimReuseImages: %v", err)
	}
	if got := rb.removedRefs(); len(got) != 1 || got[0] != reclaimRef {
		t.Fatalf("removed %v, want [%s]", got, reclaimRef)
	}
	evs := reclaimAudits(h)
	if len(evs) != 1 {
		t.Fatalf("got %d image.reuse.reclaim rows, want 1", len(evs))
	}
	ev := evs[0]
	if ev.RunID != nil || ev.ActorType != types.ActorSystem || ev.Actor != "wardyn/reuse-image-sweeper" || ev.Target != "reuse_images" || ev.Outcome != "success" {
		t.Errorf("row = %+v", ev)
	}
	if want := `{"failed":0,"max_age_days":7,"removed":["` + reclaimRef + `"]}`; string(ev.Data) != want {
		t.Errorf("data = %s, want %s", ev.Data, want)
	}
}

func TestReclaimReuseImages_KeepsWhatIsStillUsed(t *testing.T) {
	cases := []struct {
		name      string
		created   time.Time
		use       *store.RunImageUse
		workspace bool
		wantUse   int
	}{
		{"the image is recent", reclaimRecent, &store.RunImageUse{Ref: reclaimRef, LastCreated: reclaimOld}, false, 0},
		{"no run recorded it", reclaimOld, nil, false, 1},
		{"a run is live", reclaimOld, &store.RunImageUse{Ref: reclaimRef, LastCreated: reclaimOld, InUse: true}, false, 1},
		{"a run was created recently", reclaimOld, &store.RunImageUse{Ref: reclaimRef, LastCreated: reclaimRecent}, false, 1},
		{"a run was created exactly at the age", reclaimOld, &store.RunImageUse{Ref: reclaimRef, LastCreated: reclaimNow.Add(-reuseImageMaxAge)}, false, 1},
		{"a workspace names it", reclaimOld, &store.RunImageUse{Ref: reclaimRef, LastCreated: reclaimOld}, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rb := &reuseReclaimBuilder{images: []ReuseImage{{Ref: reclaimRef, Created: c.created}}}
			st := &reuseUseStore{}
			if c.use != nil {
				st.rows = []store.RunImageUse{*c.use}
			}
			if c.workspace {
				st.workspaces = []types.Workspace{{ImageRef: reclaimRef}}
			}
			srv, h := reuseReclaimServer(t, st)
			if err := srv.reclaimReuseImages(t.Context(), rb, reclaimNow); err != nil {
				t.Fatalf("reclaimReuseImages: %v", err)
			}
			if got := rb.removedRefs(); len(got) != 0 {
				t.Errorf("removed %v, want nothing", got)
			}
			if evs := reclaimAudits(h); len(evs) != 0 {
				t.Errorf("wrote %d rows, want none", len(evs))
			}
			if st.useCalls != c.wantUse {
				t.Errorf("RunImageUse called %d times, want %d", st.useCalls, c.wantUse)
			}
		})
	}
}

func TestReclaimReuseImages_AFailedReadRemovesNothing(t *testing.T) {
	boom := errors.New("boom")
	for name, mod := range map[string]func(*reuseReclaimBuilder, *reuseUseStore){
		"list":      func(rb *reuseReclaimBuilder, _ *reuseUseStore) { rb.listErr = boom },
		"use":       func(_ *reuseReclaimBuilder, st *reuseUseStore) { st.useErr = boom },
		"workspace": func(_ *reuseReclaimBuilder, st *reuseUseStore) { st.wsErr = boom },
	} {
		t.Run(name, func(t *testing.T) {
			rb, st := oldUnused(reclaimRef)
			mod(rb, st)
			srv, h := reuseReclaimServer(t, st)
			if err := srv.reclaimReuseImages(t.Context(), rb, reclaimNow); err == nil {
				t.Error("reclaimReuseImages = nil, want the read's error")
			}
			if got := rb.removedRefs(); len(got) != 0 {
				t.Errorf("removed %v, want nothing", got)
			}
			if evs := reclaimAudits(h); len(evs) != 0 {
				t.Errorf("wrote %d rows, want none", len(evs))
			}
		})
	}
}

func TestReclaimReuseImages_KeepsATagALaunchHolds(t *testing.T) {
	rb, st := oldUnused(reclaimRef)
	srv, h := reuseReclaimServer(t, st)
	hctx, release := runner.HoldImages(t.Context())
	t.Cleanup(release)
	runner.HoldImage(hctx, reclaimRef)
	if err := srv.reclaimReuseImages(t.Context(), rb, reclaimNow); err != nil {
		t.Fatalf("held pass: %v", err)
	}
	if got := rb.removedRefs(); len(got) != 0 || len(reclaimAudits(h)) != 0 {
		t.Fatalf("a held tag: removed %v, rows %d, want nothing", got, len(reclaimAudits(h)))
	}
	release()
	if err := srv.reclaimReuseImages(t.Context(), rb, reclaimNow); err != nil {
		t.Fatalf("released pass: %v", err)
	}
	if got := rb.removedRefs(); len(got) != 1 || len(reclaimAudits(h)) != 1 {
		t.Fatalf("after release: removed %v, rows %d, want one of each", got, len(reclaimAudits(h)))
	}
}

func TestReclaimReuseImages_AStoreWithoutTheReadRemovesNothing(t *testing.T) {
	rb, _ := oldUnused(reclaimRef)
	srv, h := reuseReclaimServer(t, plainWorkspaceStore{})
	if err := srv.reclaimReuseImages(t.Context(), rb, reclaimNow); err != nil {
		t.Fatalf("reclaimReuseImages: %v", err)
	}
	if rb.lists() != 0 || len(rb.removedRefs()) != 0 || len(reclaimAudits(h)) != 0 {
		t.Errorf("lists=%d removed=%v rows=%d, want none", rb.lists(), rb.removedRefs(), len(reclaimAudits(h)))
	}
}

func TestReclaimReuseImages_AFailedRemovalIsCountedAndTheRestGo(t *testing.T) {
	r1 := "wardyn-byoi/reuse:" + strings.Repeat("1", 64)
	r2 := "wardyn-byoi/reuse:" + strings.Repeat("2", 64)
	rb := &reuseReclaimBuilder{
		images:    []ReuseImage{{Ref: r1, Created: reclaimOld}, {Ref: r2, Created: reclaimOld}},
		removeErr: map[string]error{r1: errors.New("in use")},
	}
	st := &reuseUseStore{rows: []store.RunImageUse{{Ref: r1, LastCreated: reclaimOld}, {Ref: r2, LastCreated: reclaimOld}}}
	srv, h := reuseReclaimServer(t, st)
	if err := srv.reclaimReuseImages(t.Context(), rb, reclaimNow); err != nil {
		t.Fatalf("reclaimReuseImages: %v", err)
	}
	if got := rb.removedRefs(); len(got) != 1 || got[0] != r2 {
		t.Fatalf("removed %v, want [%s]", got, r2)
	}
	evs := reclaimAudits(h)
	if len(evs) != 1 {
		t.Fatalf("got %d rows, want 1", len(evs))
	}
	if want := `{"failed":1,"max_age_days":7,"removed":["` + r2 + `"]}`; string(evs[0].Data) != want {
		t.Errorf("data = %s, want %s", evs[0].Data, want)
	}
}

func TestReuseImageReclaimLoop_PassesAtOnceThenStopsWithItsContext(t *testing.T) {
	rb := &reuseReclaimBuilder{}
	srv, _ := reuseReclaimServer(t, &reuseUseStore{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.reuseImageReclaimLoop(ctx, rb, time.Hour)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for rb.lists() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no pass within 2s of starting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop did not stop within 1s of its context ending")
	}
}

func TestReuseImageReclaimTick_ContainsAPanic(t *testing.T) {
	rb := &reuseReclaimBuilder{panicList: true}
	srv, _ := reuseReclaimServer(t, &reuseUseStore{})
	srv.reuseImageReclaimTick(t.Context(), rb)
	if rb.lists() != 1 {
		t.Fatalf("lists = %d, want 1 (the panic was reached)", rb.lists())
	}
}
