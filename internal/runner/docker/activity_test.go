// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// statsOver is a daemon reading of ns nanoseconds of CPU time across a window.
func statsOver(ns uint64, window time.Duration) container.StatsResponse {
	read := time.Now()
	return container.StatsResponse{
		Read:        read,
		PreRead:     read.Add(-window),
		PreCPUStats: container.CPUStats{CPUUsage: container.CPUUsage{TotalUsage: 5_000_000_000}},
		CPUStats:    container.CPUStats{CPUUsage: container.CPUUsage{TotalUsage: 5_000_000_000 + ns}},
	}
}

// TestSampleCPU_ReadsTheAgentAsAPercentOfOneCore: the reading is the rate between
// the daemon's two samples, a run with no reading is absent (never zero), and
// each read asks the daemon for its previous sample so the figure is a rate.
func TestSampleCPU_ReadsTheAgentAsAPercentOfOneCore(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if d.BatchSample() {
		t.Error("BatchSample = true; every Docker ref is a read of its own")
	}

	f.stats = map[string]container.StatsResponse{sb.Ref: statsOver(400_000_000, time.Second)} // 0.4 core
	got, err := d.SampleCPU(ctx, []string{sb.Ref, "no-such-ref"})
	if err != nil {
		t.Fatalf("SampleCPU: %v", err)
	}
	if pct, ok := got[sb.Ref]; !ok || math.Abs(pct-40) > 0.01 {
		t.Errorf("reading = %v (%v), want 40", pct, ok)
	}
	if _, ok := got["no-such-ref"]; ok || len(got) != 1 {
		t.Errorf("readings = %v; a ref the daemon does not have must be absent", got)
	}
	for _, o := range f.statsCalls {
		if o.Stream || !o.IncludePreviousSample {
			t.Errorf("stats options = %+v, want one non-streaming read with the previous sample", o)
		}
	}
}

// TestSampleCPU_NoReadingIsNotQuiet: a read the daemon could not make, or one
// that cannot be turned into a rate, leaves the ref out rather than reading 0.
func TestSampleCPU_NoReadingIsNotQuiet(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	noPrev := statsOver(1, time.Second)
	noPrev.PreRead = time.Time{}
	reset := statsOver(0, time.Second)
	reset.PreCPUStats.CPUUsage.TotalUsage = reset.CPUStats.CPUUsage.TotalUsage + 1
	for name, st := range map[string]*container.StatsResponse{
		"daemon error":       nil,
		"no previous sample": &noPrev,
		"counter went back":  &reset,
		"zero-length window": ptr(statsOver(1, 0)),
		"negative-length":    ptr(statsOver(1, -time.Second)),
	} {
		f.stats = map[string]container.StatsResponse{}
		if st != nil {
			f.stats[sb.Ref] = *st
		}
		got, err := d.SampleCPU(ctx, []string{sb.Ref})
		if err != nil || len(got) != 0 {
			t.Errorf("%s: readings = %v, err = %v; want none", name, got, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
