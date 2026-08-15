// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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

func TestDockerCorrelator_FullAndShortID(t *testing.T) {
	run := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	full := "0123456789abcdef0123456789abcdef" // 32 chars
	lister := &fakeLister{containers: []managedContainer{{ID: full, RunID: run}}}
	c := newDockerCorrelator(lister, time.Minute)
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
	c := newDockerCorrelator(lister, time.Hour)
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
	c2 := newDockerCorrelator(lister, 0)
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
