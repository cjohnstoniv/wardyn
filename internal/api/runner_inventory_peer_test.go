// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldElsewhere is a runners page whose runners another replica holds: each caps read of one asks lookup for its
// holder, which counts the reads per runner.
type heldElsewhere struct {
	mu     sync.Mutex
	reads  map[uuid.UUID]int
	view   *remote.HolderView
	trans  *remote.PeerTransport
	routes map[uuid.UUID]remote.PeerRoute
}

func newHeldElsewhere(t *testing.T, lookup func(ctx context.Context, id uuid.UUID) error) *heldElsewhere {
	t.Helper()
	h := &heldElsewhere{reads: map[uuid.UUID]int{}, routes: map[uuid.UUID]remote.PeerRoute{}}
	h.view = remote.NewHolderView("this-replica", func(context.Context) (map[uuid.UUID]remote.PeerRoute, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := make(map[uuid.UUID]remote.PeerRoute, len(h.routes))
		for id, r := range h.routes {
			out[id] = r
		}
		return out, nil
	})
	h.trans = remote.NewPeerTransport(remote.PeerConfig{}, "this-replica", func(ctx context.Context, id uuid.UUID) (remote.PeerRoute, bool, error) {
		h.mu.Lock()
		h.reads[id]++
		r := h.routes[id]
		h.mu.Unlock()
		if err := lookup(ctx, id); err != nil {
			return remote.PeerRoute{}, false, err
		}
		return r, true, nil
	})
	return h
}

// hold registers runner id on srv as held by the other replica.
func (h *heldElsewhere) hold(t *testing.T, srv *Server, id uuid.UUID) {
	t.Helper()
	h.mu.Lock()
	h.routes[id] = remote.PeerRoute{ReplicaID: "holder-replica", PeerURL: "https://holder.invalid", SessionID: uuid.New()}
	h.mu.Unlock()
	if err := h.view.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	l := &remote.Link{}
	l.UsePeer(id, h.view, h.trans)
	srv.runnerStreams.subs[id] = runnerEndpoint{sub: remote.New(id.String(), l, remote.Options{}), link: l}
}

func (h *heldElsewhere) readsOf(id uuid.UUID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads[id]
}

// holdHere registers runner id on srv with caps held in this process.
func holdHere(t *testing.T, srv *Server, id uuid.UUID, caps runnerwire.Caps) {
	t.Helper()
	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
}

// A runners page reads each runner's caps once, for its capacity, folders, macOS VM and usage alike; a failed read
// from the replica holding one runner leaves that runner's fields empty and every other row whole.
func TestRunnersPageReadsCapsOncePerRunner(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	h := newHeldElsewhere(t, func(context.Context, uuid.UUID) error { return errors.New("the holder is unreachable") })
	caps := runnerwire.Caps{CPUMillisMax: 4000, MemoryMiBMax: 8192, Folders: true, MacOSVM: &runnerwire.MacVMCaps{MinCPUs: 4}}
	here1 := f.addRunner(srv, "alice", "here-1", types.RunnerClaimed, time.Hour)
	away1 := f.addRunner(srv, "alice", "away-1", types.RunnerClaimed, time.Hour)
	away2 := f.addRunner(srv, "alice", "away-2", types.RunnerClaimed, time.Hour)
	here2 := f.addRunner(srv, "alice", "here-2", types.RunnerClaimed, time.Hour)
	holdHere(t, srv, here1.ID, caps)
	holdHere(t, srv, here2.ID, caps)
	h.hold(t, srv, away1.ID)
	h.hold(t, srv, away2.ID)

	alice := memberModeSSOSession(t, "alice", "alice@example.com", oidc.RoleUser, false)
	views := viewIDs(runnerViewsOf(t, doSSO(t, srv, http.MethodGet, "/api/v1/me/runners", alice, "")))
	for _, id := range []uuid.UUID{away1.ID, away2.ID} {
		if n := h.readsOf(id); n != 1 {
			t.Errorf("runner %s: %d caps reads for one page, want 1", id, n)
		}
		if v := views[id]; v.Capacity != nil || v.Folders || v.MacOSVM != nil || v.Usage.Reported != nil {
			t.Errorf("runner %s with a failed caps read shows caps: %+v", id, v)
		}
	}
	for _, id := range []uuid.UUID{here1.ID, here2.ID} {
		if v := views[id]; v.Capacity == nil || !v.Folders || v.MacOSVM == nil || v.Usage.Reported == nil {
			t.Errorf("runner %s lost its caps beside a failed read: %+v", id, v)
		}
	}
}

// A runners page whose caller goes away stops every caps read it has pending: each one's wait ends with the
// request, not with the read's own limit.
func TestRunnersPageCapsReadsEndWithTheRequest(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	started := make(chan struct{}, 8)
	var mu sync.Mutex
	var ends []error
	h := newHeldElsewhere(t, func(ctx context.Context, _ uuid.UUID) error {
		started <- struct{}{}
		<-ctx.Done()
		mu.Lock()
		ends = append(ends, context.Cause(ctx))
		mu.Unlock()
		return ctx.Err()
	})
	var rows []types.Runner
	for _, name := range []string{"away-1", "away-2", "away-3"} {
		row := f.addRunner(srv, "alice", name, types.RunnerClaimed, time.Hour)
		h.hold(t, srv, row.ID)
		rows = append(rows, row)
	}
	rs, ok := srv.cfg.Store.(store.RunnerInventoryStore)
	if !ok {
		t.Fatal("the test store keeps no runner inventory")
	}
	ctx, cancel := context.WithCancel(t.Context())
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/runners", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.runnerViews(httptest.NewRecorder(), r, rs, rows, false)
	}()
	<-started
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(ends) != len(rows) {
		t.Fatalf("%d caps reads ended, want one per runner (%d)", len(ends), len(rows))
	}
	for i, err := range ends {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("caps read %d ended with %v, want the request's cancel", i, err)
		}
	}
	for _, row := range rows {
		if n := h.readsOf(row.ID); n != 1 {
			t.Errorf("runner %s: %d caps reads, want 1", row.ID, n)
		}
	}
}
