// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunCorrelationIDFlagIsSentAsTheRequestField: --correlation-id is the request's correlation_id, and
// no flag sends no key.
func TestRunCorrelationIDFlagIsSentAsTheRequestField(t *testing.T) {
	sent := func(args ...string) map[string]any {
		t.Helper()
		srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
		if err := execCmd(t, append([]string{"run", "--agent", "claude-code", "--url", srv.URL, "--token", "t"}, args...)...); err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(srv.last().body, &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if raw := sent("--correlation-id", "build-42/a:b"); raw["correlation_id"] != "build-42/a:b" {
		t.Errorf("--correlation-id sent %v", raw)
	}
	if raw := sent(); raw["correlation_id"] != nil {
		t.Errorf("no flag sent %v; want no correlation_id", raw)
	}
}

// TestRunFileTakesTheCorrelationIDFromTheFlag: a document cannot carry a correlation_id (a template field
// the door excludes), so the flag stands beside --file and reaches the create request.
func TestRunFileTakesTheCorrelationIDFromTheFlag(t *testing.T) {
	rig := runFileRig(t)
	file := writeTemplateFile(t, "run.yaml", runDocYAML)
	if _, err := rig.run(t, "run", "--file", file, "--correlation-id", "ci-7"); err != nil {
		t.Fatal(err)
	}
	if sent := rig.sent(runsPath); len(sent) != 1 || !strings.Contains(sent[0], `"correlation_id":"ci-7"`) {
		t.Errorf("create requests = %q; want correlation_id ci-7", sent)
	}
}
