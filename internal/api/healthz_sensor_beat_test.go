// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countingBeatStore answers every read of the sensor's beat with ev and err,
// and counts the reads.
type countingBeatStore struct {
	store.Store
	ev    types.AuditEvent
	err   error
	reads atomic.Int32
}

func (s *countingBeatStore) LatestOrganisationAuditEvent(context.Context, string, types.ActorType, string) (types.AuditEvent, error) {
	s.reads.Add(1)
	return s.ev, s.err
}

// sensorBeatServer builds a server over st with a settable clock.
func sensorBeatServer(t *testing.T, st store.Store) (*Server, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Now().UTC()}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.Now = clock.Now
	return New(cfg), clock
}

// Every open console polls /healthz, so two probes inside sensorBeatCacheFor make one read of the
// sensor's beat, and the kept answer expires after it. With no node row the beat holds no rise in
// its counts, so it reads idle, not yet known.
func TestSensorBeatReadIsKeptAcrossProbes(t *testing.T) {
	st := &countingBeatStore{}
	srv, clock := sensorBeatServer(t, st)
	st.ev = groundtruth.HeartbeatEventWithDropped(0, 5, 0, nil)
	st.ev.Time = clock.Now()
	for range 2 {
		if got := srv.ebpfGroundtruthStatus(context.Background()); got["state"] != "idle" || got["reason"] != groundtruthNotYetKnown {
			t.Fatalf("state = %v, reason = %v; want idle, %q, from the kept beat", got["state"], got["reason"], groundtruthNotYetKnown)
		}
	}
	if n := st.reads.Load(); n != 1 {
		t.Fatalf("two probes inside %v made %d reads, want 1", sensorBeatCacheFor, n)
	}
	clock.advance(sensorBeatCacheFor)
	srv.ebpfGroundtruthStatus(context.Background())
	if n := st.reads.Load(); n != 2 {
		t.Errorf("a probe %v later made %d reads in all, want 2: the kept answer must expire", sensorBeatCacheFor, n)
	}
}

// A failed read is kept too, so a failing scan runs at most once per sensorBeatCacheFor, and every
// probe in between still says the beat could not be read.
func TestSensorBeatReadFailureIsKept(t *testing.T) {
	st := &countingBeatStore{err: errors.New("canceling statement due to statement timeout")}
	srv, _ := sensorBeatServer(t, st)
	for range 2 {
		if got := srv.ebpfGroundtruthStatus(context.Background()); got["state"] != "unavailable" || got["reason"] != groundtruthBeatReadFailed {
			t.Fatalf("state = %v, reason = %v after a failed read, want unavailable, %q", got["state"], got["reason"], groundtruthBeatReadFailed)
		}
	}
	if n := st.reads.Load(); n != 1 {
		t.Errorf("two probes after a failed read made %d reads, want 1", n)
	}
}

// enteredBeatStore says when a beat read reaches it, then stalls the read like
// blockingHeartbeatStore.
type enteredBeatStore struct {
	blockingHeartbeatStore
	entered chan struct{}
}

func (s enteredBeatStore) LatestOrganisationAuditEvent(ctx context.Context, action string, actorType types.ActorType, actor string) (types.AuditEvent, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	return s.blockingHeartbeatStore.LatestOrganisationAuditEvent(ctx, action, actorType, actor)
}

// A probe that arrives while another caller's read runs never waits on it: with no kept answer it
// says at once that the beat could not be read.
func TestSensorBeatReadInFlightIsNotWaitedOn(t *testing.T) {
	prev := sensorBeatReadTimeout
	sensorBeatReadTimeout = time.Minute // the first read stalls until released, not until its bound
	t.Cleanup(func() { sensorBeatReadTimeout = prev })
	released := make(chan struct{})
	st := enteredBeatStore{blockingHeartbeatStore{released: released}, make(chan struct{}, 1)}
	srv, _ := sensorBeatServer(t, st)

	first := make(chan map[string]any, 1)
	go func() { first <- srv.ebpfGroundtruthStatus(context.Background()) }()
	<-st.entered
	second := make(chan map[string]any, 1)
	go func() { second <- srv.ebpfGroundtruthStatus(context.Background()) }()
	select {
	case got := <-second:
		if got["state"] != "unavailable" || got["reason"] != groundtruthBeatReadFailed {
			t.Errorf("state = %v, reason = %v during another read with nothing kept, want unavailable, %q",
				got["state"], got["reason"], groundtruthBeatReadFailed)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("a probe waited on another caller's read")
	}
	close(released)
	<-first
}

// stallAfterFirstBeatStore answers the first beat read with ev, then says when a later read reaches
// it and stalls that read until released.
type stallAfterFirstBeatStore struct {
	store.Store
	ev       types.AuditEvent
	reads    atomic.Int32
	entered  chan struct{}
	released chan struct{}
}

func (s *stallAfterFirstBeatStore) LatestOrganisationAuditEvent(ctx context.Context, _ string, _ types.ActorType, _ string) (types.AuditEvent, error) {
	if s.reads.Add(1) == 1 {
		return s.ev, nil
	}
	s.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return types.AuditEvent{}, ctx.Err()
	case <-s.released:
		return s.ev, nil
	}
}

// A probe that arrives while another caller's read runs, with an older answer kept, gets that answer
// at once, past its 30 seconds too: never the could-not-be-read answer while a beat is kept.
func TestSensorBeatReadInFlightAnswersTheKeptBeat(t *testing.T) {
	prev := sensorBeatReadTimeout
	sensorBeatReadTimeout = time.Minute // the second read stalls until released, not until its bound
	t.Cleanup(func() { sensorBeatReadTimeout = prev })
	st := &stallAfterFirstBeatStore{entered: make(chan struct{}, 1), released: make(chan struct{})}
	srv, clock := sensorBeatServer(t, st)
	st.ev = groundtruth.HeartbeatEventWithDropped(0, 5, 0, nil)
	st.ev.Time = clock.Now()
	srv.ebpfGroundtruthStatus(context.Background())
	clock.advance(sensorBeatCacheFor)

	second := make(chan map[string]any, 1)
	go func() { second <- srv.ebpfGroundtruthStatus(context.Background()) }()
	<-st.entered
	third := make(chan map[string]any, 1)
	go func() { third <- srv.ebpfGroundtruthStatus(context.Background()) }()
	select {
	case got := <-third:
		if got["reason"] != groundtruthNotYetKnown || got["last_heartbeat"] != st.ev.Time.UTC().Format(time.RFC3339) {
			t.Errorf("during another read with a beat kept: %v, want the kept beat (reason %q)", got, groundtruthNotYetKnown)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("a probe waited on another caller's read")
	}
	close(st.released)
	<-second
}

// evidenceSensorStore keeps node rows as the Postgres upsert does, the evidence rule included, and
// holds no audit beat: one store shared by several servers, as replicas share the database.
type evidenceSensorStore struct {
	store.Store
	mu   sync.Mutex
	rows map[string]store.GroundtruthSensor
}

func (s *evidenceSensorStore) UpsertGroundtruthSensor(_ context.Context, g store.GroundtruthSensor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.rows[g.Node]
	if ok && old.LastBeat.After(g.LastBeat) {
		return nil
	}
	g.EvidenceAt = nil
	switch {
	case !ok:
	case g.ObservedTotal+g.DroppedUnmapped <= old.ObservedTotal+old.DroppedUnmapped:
		g.EvidenceAt = old.EvidenceAt
	case g.LastBeat.Sub(old.LastBeat) <= store.GroundtruthEvidenceWindow:
		at := g.LastBeat
		g.EvidenceAt = &at
	}
	if s.rows == nil {
		s.rows = map[string]store.GroundtruthSensor{}
	}
	s.rows[g.Node] = g
	return nil
}

func (s *evidenceSensorStore) ListGroundtruthSensors(_ context.Context, since time.Time) ([]store.GroundtruthSensor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.GroundtruthSensor
	for _, g := range s.rows {
		if g.LastBeat.After(since) {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b store.GroundtruthSensor) int { return b.LastBeat.Compare(a.LastBeat) })
	return out, nil
}

func (*evidenceSensorStore) LatestOrganisationAuditEvent(context.Context, string, types.ActorType, string) (types.AuditEvent, error) {
	return types.AuditEvent{}, store.ErrNotFound
}

// The ingest process keeps beating its lifetime counts after Tetragon or its export stops, so the
// verdict comes from when the node row last saw them rise. That time is on the shared row: with two
// replicas, one taking the sensor's beats and one only reading, the reader judges the same, and a
// server started later judges the same at once.
func TestHealthzEbpfEvidenceIsSharedAcrossServers(t *testing.T) {
	if store.GroundtruthEvidenceWindow != 15*time.Minute {
		t.Fatalf("GroundtruthEvidenceWindow = %v; the reason and the notes say 15 minutes", store.GroundtruthEvidenceWindow)
	}
	h := newHarness(t)
	st := &evidenceSensorStore{}
	clock := &testClock{now: time.Now().UTC()}
	server := func() *Server {
		cfg := baseTestConfig(h, st)
		cfg.Now = clock.Now
		return New(cfg)
	}
	ingest, reader := server(), server()
	tok := h.mintGroundtruthToken(t)
	beat := func(observed uint64) {
		t.Helper()
		body, _ := json.Marshal(groundtruthBatch{Events: []types.AuditEvent{groundtruth.HeartbeatEventWithDropped(0, observed, 0, healthyKinds)}})
		if w := do(t, ingest, http.MethodPost, "/api/v1/internal/groundtruth", tok, string(body)); w.Code != http.StatusAccepted {
			t.Fatalf("heartbeat post = %d, want 202; body=%s", w.Code, w.Body.String())
		}
	}
	want := func(srv *Server, step, state, reason string) {
		t.Helper()
		got := srv.ebpfGroundtruthStatus(context.Background())
		if r, _ := got["reason"].(string); got["state"] != state || r != reason {
			t.Fatalf("%s: state = %v, reason = %v; want %s, %q", step, got["state"], got["reason"], state, reason)
		}
	}

	beat(6)
	want(reader, "a first beat", "idle", groundtruthNotYetKnown)
	clock.advance(30 * time.Second)
	beat(7)
	want(reader, "counts that rose on the other server's beat", "healthy", "")
	for range 32 { // 16 minutes of beats with the same counts
		clock.advance(30 * time.Second)
		beat(7)
	}
	want(reader, "fresh beats with counts unchanged past the window", "idle", groundtruthNoRecentEvents)
	if got := reader.hostSensorCaveat(context.Background()); got != hostSensorQuietCaveat {
		t.Errorf("note = %q, want the no-new-events note", got)
	}
	want(server(), "a server started now", "idle", groundtruthNoRecentEvents)
	clock.advance(30 * time.Second)
	beat(8)
	want(reader, "counts that rose again", "healthy", "")
}
