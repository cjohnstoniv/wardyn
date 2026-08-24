// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
)

type fakeLister struct {
	containers []managedContainer
	calls      int
}

func (f *fakeLister) managedContainers(ctx context.Context) ([]managedContainer, error) {
	f.calls++
	return f.containers, nil
}

// fakeWatcher stands in for the `docker events` stream: it announces a fixed
// set of containers, then blocks until ctx is done (as the real stream does).
type fakeWatcher struct {
	announce []managedContainer
	seen     chan struct{}
}

func (f *fakeWatcher) watchContainers(ctx context.Context, add func(managedContainer)) error {
	for _, mc := range f.announce {
		add(mc)
	}
	if f.seen != nil {
		close(f.seen)
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestDockerCorrelator_FullAndShortID(t *testing.T) {
	run := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	full := "0123456789abcdef0123456789abcdef" // 32 chars
	lister := &fakeLister{containers: []managedContainer{{ID: full, RunID: run}}}
	c := newDockerCorrelator(lister, nil, time.Minute)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	if r, ok := c.RunForContainer(full); !ok || r != run {
		t.Errorf("full id: got %v/%v, want %v", r, ok, run)
	}
	if r, ok := c.RunForContainer(full[:12]); !ok || r != run {
		t.Errorf("short id: got %v/%v, want %v", r, ok, run)
	}
	// A longer-than-short prefix Tetragon might emit.
	if r, ok := c.RunForContainer(full[:20]); !ok || r != run {
		t.Errorf("prefix id: got %v/%v, want %v", r, ok, run)
	}
	if _, ok := c.RunForContainer("ffffffffffff"); ok {
		t.Error("unknown id should not resolve")
	}
	if _, ok := c.RunForContainer(""); ok {
		t.Error("empty id should not resolve")
	}
}

func TestDockerCorrelator_OnMissRefreshThrottled(t *testing.T) {
	run := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	lister := &fakeLister{} // starts empty
	c := newDockerCorrelator(lister, nil, time.Hour)
	_ = c.Refresh(context.Background())
	callsAfterFirst := lister.calls

	// A miss should NOT trigger a refresh because the throttle gap is huge.
	if _, ok := c.RunForContainer("newcontainer"); ok {
		t.Error("should miss on empty index")
	}
	if lister.calls != callsAfterFirst {
		t.Errorf("on-miss refresh should be throttled; calls went %d -> %d", callsAfterFirst, lister.calls)
	}

	// Now make the container available and use a zero throttle: a miss should
	// refresh and then resolve.
	lister.containers = []managedContainer{{ID: "newcontainer000000000000", RunID: run}}
	c2 := newDockerCorrelator(lister, nil, 0)
	_ = c2.Refresh(context.Background())
	lister.containers = []managedContainer{{ID: "appears-later-00000000000", RunID: run}}
	if r, ok := c2.RunForContainer("appears-later-00000000000"); !ok || r != run {
		t.Errorf("on-miss refresh should pick up a new container: got %v/%v", r, ok)
	}
}

func TestParseLabels(t *testing.T) {
	m := parseLabels("wardyn.managed=true,wardyn.run-id=33333333-3333-3333-3333-333333333333,wardyn.component=agent")
	if m["wardyn.component"] != "agent" {
		t.Errorf("component = %q, want agent", m["wardyn.component"])
	}
	if m["wardyn.run-id"] != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("run-id = %q", m["wardyn.run-id"])
	}
	if m["wardyn.managed"] != "true" {
		t.Errorf("managed = %q, want true", m["wardyn.managed"])
	}
	if got := parseLabels(""); len(got) != 0 {
		t.Errorf("empty labels = %v, want empty", got)
	}
}

// ── W24-S1-1 regression: unmapped host-wide events must not leak ───────────

// fakeGTCorrelator implements groundtruth.Correlator with a single known
// mapping, so a synthetic exec line can be made to resolve (or not) without
// pulling in the docker-CLI/index-refresh machinery exercised above.
type fakeGTCorrelator struct {
	id  string
	run uuid.UUID
}

func (f fakeGTCorrelator) RunForContainer(id string) (uuid.UUID, bool) {
	if id == f.id {
		return f.run, true
	}
	return uuid.Nil, false
}

// TestUnmappedHostEventNotForwardedByDefault is the W24-S1-1 regression:
// Tetragon is a HOST sensor and observes every process on the box, not only
// Wardyn's. Before this fix, an exec event from a container the docker
// correlator does not recognise (a non-Wardyn container, or a bare host
// process) was still forwarded to the control plane's undeletable audit log /
// SIEM fanout with run_id NULL + correlation=unmapped — a secret/PII leak of
// workloads Wardyn does not own. Red-first: verified empirically against base
// 6d76911 (pre-gatedMapper processLine, unmodified) — the equivalent scenario
// there forwards the unmapped event and this assertion fails.
func TestUnmappedHostEventNotForwardedByDefault(t *testing.T) {
	run := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	corr := fakeGTCorrelator{id: "wardyncontainerid0000", run: run}

	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	sink := newEventSink(srv.URL, "tok", 64, 8, 20*time.Millisecond, srv.Client())

	// The real production wiring (run() in main.go): gatedMapper around the
	// Tetragon mapper, allowUnmapped left at its zero value (false) — secure
	// by default, no opt-in flag set.
	mapper := &gatedMapper{inner: groundtruth.NewMapper(corr)}

	const unmappedBin = "/usr/bin/nonwardyn-marker"
	const mappedBin = "/usr/bin/wardyn-marker"
	unmappedLine := []byte(`{"process_exec":{"process":{"binary":"` + unmappedBin + `","docker":"othercontainerid0000"}}}`)
	mappedLine := []byte(`{"process_exec":{"process":{"binary":"` + mappedBin + `","docker":"wardyncontainerid0000"}}}`)

	processLine(unmappedLine, mapper, sink)
	processLine(mappedLine, mapper, sink)

	// processLine is synchronous; close() drains + flushes the buffered batch
	// and blocks until the worker exits, so no polling is needed.
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sink.close(closeCtx)

	bodyContains := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bodies {
			if strings.Contains(b, want) {
				return true
			}
		}
		return false
	}
	if bodyContains(unmappedBin) {
		t.Error("unmapped host-wide event was forwarded to the audit sink by default (W24-S1-1 regression)")
	}
	if !bodyContains(mappedBin) {
		t.Error("run-mapped event must still be forwarded")
	}
}

// TestGatedMapper_ForwardsUnmappedWhenOptedIn proves the escape hatch: with
// allowUnmapped set (-forward-unmapped-host-events /
// WARDYN_GROUNDTRUTH_FORWARD_UNMAPPED_HOST_EVENTS=true in main.go's run()),
// an unmapped event is forwarded exactly as before this fix — full-host
// detection coverage remains available to an operator who explicitly wants
// it, visible (run_id NULL, correlation=unmapped) rather than silently
// dropped.
func TestGatedMapper_ForwardsUnmappedWhenOptedIn(t *testing.T) {
	mapper := &gatedMapper{inner: groundtruth.NewMapper(nil), allowUnmapped: true}
	line := []byte(`{"process_exec":{"process":{"binary":"/usr/bin/nonwardyn","docker":"unknown0000"}}}`)
	ev, ok := mapper.MapLine(line)
	if !ok {
		t.Fatal("unmapped event must still be emitted when opted in")
	}
	if ev.RunID != nil {
		t.Errorf("run id = %v, want nil for an unmapped event", ev.RunID)
	}
}

// ── E1/E2 regression: the frozen counter (short-lived containers) ───────────

// TestDockerCorrelator_ResolvesAfterContainerExits is half of the frozen-counter
// regression. Refresh used to REPLACE the index with the current `docker ps`
// snapshot, so the moment a run's container exited (and teardown removed it)
// every kernel event still coming down the lagging Tetragon tail resolved
// unmapped, was gated, and moved no counter at all. Entries must now outlive
// their container by containerRetention.
func TestDockerCorrelator_ResolvesAfterContainerExits(t *testing.T) {
	run := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	full := "aaaabbbbccccddddeeeeffff00001111"
	lister := &fakeLister{containers: []managedContainer{{ID: full, RunID: run}}}
	c := newDockerCorrelator(lister, nil, time.Minute)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The container exits and teardown removes it: it is gone from the listing.
	lister.containers = nil
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, ok := c.RunForContainer(full); !ok || r != run {
		t.Errorf("exited container must still correlate within the retention window: got %v/%v, want %v", r, ok, run)
	}
	if r, ok := c.RunForContainer(full[:31]); !ok || r != run { // the id form this Tetragon build emits
		t.Errorf("truncated id after exit: got %v/%v, want %v", r, ok, run)
	}

	// Past the retention window the entry is swept, so the index cannot grow
	// without bound.
	c.retain = 0
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.RunForContainer(full); ok {
		t.Error("entry must be swept once it is older than the retention window")
	}
}

// TestDockerCorrelator_WatchLearnsShortLivedContainer is the other half: a
// container that starts and exits BETWEEN two polls is never in any snapshot at
// all (the live one-shot repro in local/gt-diagnosis.md). Only the docker event
// stream can bind it, and it must be bound without any successful listing.
func TestDockerCorrelator_WatchLearnsShortLivedContainer(t *testing.T) {
	run := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	full := "1111222233334444555566667777888"
	w := &fakeWatcher{
		announce: []managedContainer{{ID: full, RunID: run}},
		seen:     make(chan struct{}),
	}
	// The lister NEVER reports the container — as `docker ps` never did.
	c := newDockerCorrelator(&fakeLister{}, w, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.Watch(ctx) }()
	select {
	case <-w.seen:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never ran")
	}

	if r, ok := c.RunForContainer(full[:12]); !ok || r != run {
		t.Errorf("event-stream container must correlate: got %v/%v, want %v", r, ok, run)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return on context cancellation")
	}
}

// TestManagedFromDockerEventLine pins the `docker events --format '{{json .}}'`
// shape the watcher parses: the labels ride on Actor.Attributes, so a create
// event alone carries everything the index needs (no follow-up inspect, which
// would race the container's removal). Non-agent containers stay out.
func TestManagedFromDockerEventLine(t *testing.T) {
	const line = `{"status":"create","id":"c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00",` +
		`"Type":"container","Action":"create","Actor":{"ID":"c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00",` +
		`"Attributes":{"image":"wardyn/agent","wardyn.managed":"true","wardyn.component":"agent",` +
		`"wardyn.run-id":"77777777-7777-7777-7777-777777777777"}},"scope":"local"}`
	var row dockerEventLine
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatal(err)
	}
	mc, ok := managedFromLabels(row.ID, row.Actor.Attributes)
	if !ok {
		t.Fatal("agent container event must yield an index entry")
	}
	if mc.RunID != uuid.MustParse("77777777-7777-7777-7777-777777777777") {
		t.Errorf("run id = %v", mc.RunID)
	}
	if mc.ID != row.Actor.ID {
		t.Errorf("container id = %q", mc.ID)
	}

	// The proxy sidecar's own kernel events are not the agent's behaviour.
	row.Actor.Attributes["wardyn.component"] = "proxy"
	if _, ok := managedFromLabels(row.ID, row.Actor.Attributes); ok {
		t.Error("proxy container must not enter the index")
	}
}

// TestGatedMapper_CountsUnmappedDrops is the visibility half: a gated event used
// to move NO counter (not observed, not dropped), so "the sensor saw nothing"
// and "the sensor saw plenty and correlated none" were the same
// observed_total==0 on /healthz. The gate must count what it refuses.
func TestGatedMapper_CountsUnmappedDrops(t *testing.T) {
	run := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	mapper := &gatedMapper{inner: groundtruth.NewMapper(fakeGTCorrelator{id: "knowncontainerid0000", run: run})}

	if _, ok := mapper.MapLine([]byte(`{"process_exec":{"process":{"binary":"/bin/sh","docker":"unknowncontainer0000"}}}`)); ok {
		t.Fatal("unmapped event must still be gated")
	}
	if got := mapper.droppedUnmappedCount(); got != 1 {
		t.Errorf("dropped_unmapped = %d, want 1 after one gated event", got)
	}

	if _, ok := mapper.MapLine([]byte(`{"process_exec":{"process":{"binary":"/bin/sh","docker":"knowncontainerid0000"}}}`)); !ok {
		t.Fatal("correlated event must be forwarded")
	}
	if got := mapper.droppedUnmappedCount(); got != 1 {
		t.Errorf("dropped_unmapped = %d, want 1 — a correlated event is not a drop", got)
	}

	// An unrecorded event KIND is not a correlation failure and must not inflate
	// the counter.
	if _, ok := mapper.MapLine([]byte(`{"process_exit":{"process":{"binary":"/bin/sh"}}}`)); ok {
		t.Fatal("unrecorded kind must not map")
	}
	if got := mapper.droppedUnmappedCount(); got != 1 {
		t.Errorf("dropped_unmapped = %d, want 1 — an unrecorded kind is not a gated drop", got)
	}
}
