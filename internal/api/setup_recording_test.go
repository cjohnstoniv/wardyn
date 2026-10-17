// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// recordingRowFor reads the recording row from GET /setup/status for a server given the recording
// component /healthz would report, the WARDYN_RECORDING_STORE value and whether HA is on.
func recordingRowFor(t *testing.T, comp *ComponentInfo, configured string, ha bool) (SetupCheck, bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := Config{AdminToken: adminToken, RecordingSelection: configured, HA: ha}
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

// A store that came up is an info row naming it and its source; a store that did not (fs with no
// directory, or off) is a non-blocking warn that names the cause, the remedies and the runner consequence.
func TestSetupStatusRecordingRow(t *testing.T) {
	c, ok := recordingRowFor(t, &ComponentInfo{Selected: "pg", Source: "configured"}, "pg", false)
	if !ok || c.Status != "info" || c.Blocking || c.Fix != "" ||
		c.Detail != "Session recording is on: the pg store is selected (configured)." {
		t.Errorf("pg row = %+v, %v; want info, on, naming pg and its source", c, ok)
	}

	disabled := &ComponentInfo{Selected: "none", Source: "disabled"}
	c, ok = recordingRowFor(t, disabled, "fs", false)
	if !ok || c.Status != "warn" || c.Blocking || c.Cause != "no_dir" ||
		!strings.Contains(c.Detail, "WARDYN_RECORDING_STORE is fs and the recording directory (-recording-dir) is empty") ||
		strings.Contains(c.Detail, "Helm chart") || !strings.Contains(c.Detail, "every run is marked not_recorded: org_off") ||
		!strings.Contains(c.Fix, "-recording-dir (WARDYN_RECORDING_DIR)") || !strings.Contains(c.Fix, "env.WARDYN_RECORDING_STORE=pg") {
		t.Errorf("fs with no dir row = %+v, %v; want a non-blocking warn naming the directory flag and the dir or pg remedy", c, ok)
	}

	c, ok = recordingRowFor(t, disabled, "off", false)
	if !ok || c.Status != "warn" || c.Blocking || c.Cause != "store_off" ||
		!strings.Contains(c.Detail, "WARDYN_RECORDING_STORE is off (the Helm chart's default without persistence, or set by hand)") ||
		!strings.Contains(c.Fix, "env.WARDYN_RECORDING_STORE=pg") || !strings.Contains(c.Fix, "persistence.enabled=true") {
		t.Errorf("off row = %+v, %v; want a non-blocking warn naming the chart default and both remedies", c, ok)
	}

	// Under high availability the chart refuses persistence and wardynd refuses fs: only pg is offered.
	c, ok = recordingRowFor(t, disabled, "off", true)
	if !ok || !strings.Contains(c.Fix, "env.WARDYN_RECORDING_STORE=pg") || strings.Contains(c.Fix, "persistence") {
		t.Errorf("off row under HA = %+v, %v; want the pg remedy only", c, ok)
	}

	if c, ok := recordingRowFor(t, nil, "", false); ok {
		t.Errorf("a server with no recording component showed the row: %+v", c)
	}
}

// The row shows to an operator and not to a member: its fix names deployment settings.
func TestSetupStatusRecordingRowIsOperatorOnly(t *testing.T) {
	srv, _, _ := newRunnerRegistrationServer(t)
	srv.cfg.Components = map[string]ComponentInfo{"recording": {Selected: "none", Source: "disabled"}}
	srv.cfg.RecordingSelection = "off"
	rowFor := func(cookie *http.Cookie) bool {
		t.Helper()
		w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /setup/status = %d %s", w.Code, w.Body.String())
		}
		var st SetupStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		for _, c := range st.Checks {
			if c.ID == "recording" {
				return true
			}
		}
		return false
	}
	if !rowFor(ssoSession(t, "admin", "admin@corp.example", oidc.RoleAdmin)) {
		t.Error("an admin did not see the recording row")
	}
	if rowFor(ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)) {
		t.Error("a member saw the recording row")
	}
}
