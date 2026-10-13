// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An interactive run that asks for a startup is not idle: the line names what starts.
func TestRunCmd_InteractiveLineNamesTheStartup(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     string
		wantIdle bool
	}{
		{"harness startup", []string{"--experience", "interactive", "--tool", "claude-code", "--startup", "harness:claude-code"}, "starts claude-code", false},
		{"command startup", []string{"--experience", "interactive", "--tool", "claude-code", "--startup", "command:npm run dev"}, "starts npm run dev", false},
		{"none startup", []string{"--experience", "interactive", "--tool", "claude-code", "--startup", "none"}, "sandbox is idle", true},
		{"no startup", []string{"--experience", "interactive", "--tool", "claude-code"}, "sandbox is idle", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCmdServer(t, http.StatusCreated, types.AgentRun{ID: uuid.New(), State: types.RunPending, ConfinementClass: types.CC1})
			out, err := execCmdCapture(t, append([]string{"run", "--url", srv.URL, "--token", "tok"}, tc.args...)...)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output missing %q:\n%s", tc.want, out)
			}
			if !tc.wantIdle && strings.Contains(out, "sandbox is idle") {
				t.Errorf("a run with a startup printed \"sandbox is idle\":\n%s", out)
			}
		})
	}
}
