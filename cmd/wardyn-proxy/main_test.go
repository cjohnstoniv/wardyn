// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestLogBranchNSPosture_WarnsOnceWhenDisabled is cmd/wardyn-proxy's first
// test (audit row 64): opting out of git-broker push branch-namespace
// confinement (WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false) must produce
// exactly one boot WARN naming the run. Before this, the OFF state logged
// nothing at all, so the only way to tell a confined proxy from an
// opted-out one was `docker inspect` on the sidecar.
func TestLogBranchNSPosture_WarnsOnceWhenDisabled(t *testing.T) {
	t.Setenv("WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS", "false")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	runID := uuid.New()
	logBranchNSPosture(runID)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want exactly one log line, got %d: %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v: %q", err, lines[0])
	}
	if got := rec["level"]; got != "WARN" {
		t.Errorf("level = %v, want WARN", got)
	}
	if got, _ := rec["msg"].(string); !strings.Contains(got, "WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false") {
		t.Errorf("msg = %q, want it to name the opt-out env var", got)
	}
	if got := rec["run_id"]; got != runID.String() {
		t.Errorf("run_id = %v, want %s", got, runID)
	}
}

// TestLogBranchNSPosture_SilentWhenEnabled asserts the default (enforced)
// posture — the common case, unset or an explicit "on" value — logs nothing:
// the WARN exists to flag the opt-out, not to narrate the safe default on
// every boot.
func TestLogBranchNSPosture_SilentWhenEnabled(t *testing.T) {
	for _, v := range []string{"", "true", "1"} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv("WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS", v)
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			logBranchNSPosture(uuid.New())

			if buf.Len() != 0 {
				t.Errorf("enforced posture must log nothing, got %q", buf.String())
			}
		})
	}
}

// TestLogBranchNSPosture_StatesPATOptIn is the mirror of the two above for the
// git_pat lane's switch: that one is default OFF, so the state worth a boot line
// is the state an operator turned ON. Info, not Warn — nothing is weakened — but
// stated, because a confinement nobody announced looks like a forge bug to
// whoever gets the 403.
func TestLogBranchNSPosture_StatesPATOptIn(t *testing.T) {
	t.Setenv("WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS", "") // the App lane stays silent
	t.Setenv("WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS", "on")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	runID := uuid.New()
	logBranchNSPosture(runID)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want exactly one log line, got %d: %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v: %q", err, lines[0])
	}
	if got := rec["level"]; got != "INFO" {
		t.Errorf("level = %v, want INFO", got)
	}
	if got, _ := rec["msg"].(string); !strings.Contains(got, "WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS") {
		t.Errorf("msg = %q, want it to name the opt-in env var", got)
	}
	if got := rec["run_id"]; got != runID.String() {
		t.Errorf("run_id = %v, want %s", got, runID)
	}
}

// TestLogBranchNSPosture_SilentWhenPATSwitchUnset: the default deployment gets
// no new boot noise — the whole point of an opt-in.
func TestLogBranchNSPosture_SilentWhenPATSwitchUnset(t *testing.T) {
	for _, v := range []string{"", "false", "off"} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv("WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS", "")
			t.Setenv("WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS", v)
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			logBranchNSPosture(uuid.New())

			if buf.Len() != 0 {
				t.Errorf("an un-opted-in git_pat lane must log nothing, got %q", buf.String())
			}
		})
	}
}
