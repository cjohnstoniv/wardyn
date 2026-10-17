// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

// recordingRowFor reads the recording row from GET /setup/status for a server given the recording
// component /healthz would report and the WARDYN_RECORDING_STORE value.
func recordingRowFor(t *testing.T, comp *ComponentInfo, configured string) (SetupCheck, bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := Config{AdminToken: adminToken, RecordingSelection: configured}
	if comp != nil {
		cfg.Components = map[string]ComponentInfo{"recording": *comp}
	}
	code, st := decodeSetup(t, New(cfg), adminToken)
	if code != 200 {
		t.Fatalf("GET /setup/status = %d", code)
	}
	for _, c := range st.Checks {
		if c.ID == "recording" {
			return c, true
		}
	}
	return SetupCheck{}, false
}

// A store that came up is an info row naming it; a store that did not (fs with no directory, or off) is a
// non-blocking warn naming both remedies and the runner consequence.
func TestSetupStatusRecordingRow(t *testing.T) {
	c, ok := recordingRowFor(t, &ComponentInfo{Selected: "pg", Source: "configured"}, "pg")
	if !ok || c.Status != "info" || c.Blocking || !strings.Contains(c.Detail, "on") || !strings.Contains(c.Detail, "pg") {
		t.Errorf("pg row = %+v, %v; want info, on, naming pg", c, ok)
	}

	disabled := &ComponentInfo{Selected: "none", Source: "disabled"}
	c, ok = recordingRowFor(t, disabled, "fs")
	if !ok || c.Status != "warn" || c.Blocking || c.Cause != "no_dir" ||
		!strings.Contains(c.Detail, "WARDYN_RECORDING_STORE is fs and WARDYN_RECORDING_DIR is empty") ||
		!strings.Contains(c.Detail, "not_recorded: org_off") ||
		!strings.Contains(c.Fix, "env.WARDYN_RECORDING_STORE=pg") || !strings.Contains(c.Fix, "persistence.enabled=true") {
		t.Errorf("fs with no dir row = %+v, %v; want a non-blocking warn with the chart remedies", c, ok)
	}

	c, ok = recordingRowFor(t, disabled, "off")
	if !ok || c.Status != "warn" || c.Cause != "store_off" || !strings.Contains(c.Detail, "WARDYN_RECORDING_STORE is off") ||
		!strings.Contains(c.Fix, "env.WARDYN_RECORDING_STORE=pg") {
		t.Errorf("off row = %+v, %v; want a warn naming the off store and the remedy", c, ok)
	}

	if c, ok := recordingRowFor(t, nil, ""); ok {
		t.Errorf("a server with no recording component showed the row: %+v", c)
	}
}
