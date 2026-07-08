// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
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
