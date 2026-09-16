// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"testing"

	"github.com/google/uuid"
)

// TestWardynLabels_ReservedKeysWinOverCallerSupplied is the docker half of
// B9-F1: extra is caller-supplied (it reaches here from policy/dispatch), and
// it was applied LAST — so an entry named wardyn.run-id silently overwrote the
// run's own id on every object stamped with it. Every teardown path on this
// substrate selects on that label, so a run that mislabelled itself could never
// be torn down by id again: its agent container, its proxy sidecar holding the
// run's credentials, and its per-run network would all answer to someone else's
// selector (or to nothing at all).
//
// The k8s driver has stamped the three reserved keys last since its M3 finding;
// this is the same rule on the substrate that did not have it.
func TestWardynLabels_ReservedKeysWinOverCallerSupplied(t *testing.T) {
	runID := uuid.New()
	other := uuid.New().String()

	l := wardynLabels(runID, componentAgent, map[string]string{
		labelRun:       other,
		labelComponent: componentProxy,
		labelManaged:   "false",
		"team":         "platform",
	})

	if l[labelRun] != runID.String() {
		t.Errorf("%s = %q, want %q — a caller-supplied label must never rename the run", labelRun, l[labelRun], runID)
	}
	if l[labelComponent] != componentAgent {
		t.Errorf("%s = %q, want %q", labelComponent, l[labelComponent], componentAgent)
	}
	if l[labelManaged] != "true" {
		t.Errorf("%s = %q, want \"true\"", labelManaged, l[labelManaged])
	}
	if l["team"] != "platform" {
		t.Errorf("a non-reserved caller label was dropped: %v", l)
	}
}
