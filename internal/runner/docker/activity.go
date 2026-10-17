// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// BatchSample is false: every ref is a daemon read of its own, about a second
// long, so the caller bounds how many it asks for (runner.ActivitySampler).
func (d *Driver) BatchSample() bool { return false }

// SampleCPU is runner.ActivitySampler: the AGENT container's CPU use in percent
// of one core, from the daemon's two samples a second apart. The proxy sidecar
// is another container and is never read. A container that is gone, paused or
// unreadable has no reading, which is not the same as a quiet one.
func (d *Driver) SampleCPU(ctx context.Context, refs []string) (map[string]float64, error) {
	out := make(map[string]float64, len(refs))
	for _, ref := range refs {
		if pct, ok := d.sampleOne(ctx, ref); ok {
			out[ref] = pct
		}
	}
	return out, nil
}

func (d *Driver) sampleOne(ctx context.Context, ref string) (float64, bool) {
	res, err := d.cli.ContainerStats(ctx, ref, client.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		return 0, false
	}
	defer res.Body.Close()
	var st container.StatsResponse
	if json.NewDecoder(res.Body).Decode(&st) != nil {
		return 0, false
	}
	window := st.Read.Sub(st.PreRead)
	if st.PreRead.IsZero() || window <= 0 || st.CPUStats.CPUUsage.TotalUsage < st.PreCPUStats.CPUUsage.TotalUsage {
		return 0, false
	}
	used := st.CPUStats.CPUUsage.TotalUsage - st.PreCPUStats.CPUUsage.TotalUsage // nanoseconds of CPU time
	return float64(used) / float64(window.Nanoseconds()) * 100, true
}
