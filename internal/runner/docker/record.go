// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strconv"

	"github.com/moby/moby/client"
)

// labelRecord stamps the agent container with its run's recording choice at create
// (SandboxSpec.RecordWithin), so every later exec, attach and revive follows the run's own choice
// and never the driver-wide Config.Record alone: one driver runs recorded and unrecorded runs side
// by side, and a runnerd restart keeps each run's choice.
const labelRecord = "wardyn.record"

// recordsLabels is whether the run whose agent carries labels is recorded: its own choice within
// Config.Record. An agent created before the label existed follows Config.Record, as it did then.
func (d *Driver) recordsLabels(labels map[string]string) bool {
	v, ok := labels[labelRecord]
	if !ok {
		return d.cfg.Record
	}
	rec, _ := strconv.ParseBool(v)
	return d.cfg.Record && rec
}

// recordsAgent is recordsLabels for the agent container ref; Config.Record when it cannot be read.
func (d *Driver) recordsAgent(ctx context.Context, ref string) bool {
	if !d.cfg.Record {
		return false
	}
	insp, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil || insp.Container.Config == nil {
		return d.cfg.Record
	}
	return d.recordsLabels(insp.Container.Config.Labels)
}

// withRecordLabel stamps labelRecord on an agent's labels, after any caller-supplied entry.
func withRecordLabel(labels map[string]string, record bool) map[string]string {
	labels[labelRecord] = strconv.FormatBool(record)
	return labels
}
