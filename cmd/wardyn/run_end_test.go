// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestParseEndsIn(t *testing.T) {
	for in, want := range map[string]int{
		"90m": 5400, "36h": 129600, "2d": 172800, "1d12h": 129600, "0.5d": 43200, "30s": 30, "1h30m": 5400,
		"1.5d": 129600, "2d12h": 216000,
	} {
		if got, err := parseEndsIn(in); err != nil || got != want {
			t.Errorf("parseEndsIn(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"abc", "0s", "-5m", "500ms", "d", "2dd", "9999999999d", ".5d", "1..5d"} {
		if got, err := parseEndsIn(in); err == nil {
			t.Errorf("parseEndsIn(%q) = %d, want an error", in, got)
		}
	}
}

func TestRunEndFlagsAreSentAsTheRequestFields(t *testing.T) {
	sent := func(t *testing.T, args ...string) (map[string]any, error) {
		t.Helper()
		srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
		err := execCmd(t, append([]string{"run", "--agent", "claude-code", "--url", srv.URL, "--token", "t"}, args...)...)
		if len(srv.reqs) == 0 {
			return nil, err
		}
		var raw map[string]any
		if jerr := json.Unmarshal(srv.last().body, &raw); jerr != nil {
			t.Fatal(jerr)
		}
		return raw, err
	}

	if raw, err := sent(t, "--ends-in", "2d"); err != nil || raw["ends_after_sec"] != float64(172800) || raw["no_end"] != nil {
		t.Errorf("--ends-in 2d sent %v (%v); want ends_after_sec 172800", raw, err)
	}
	if raw, err := sent(t, "--no-end"); err != nil || raw["no_end"] != true || raw["ends_after_sec"] != nil {
		t.Errorf("--no-end sent %v (%v); want no_end true", raw, err)
	}
	if raw, err := sent(t); err != nil || raw["no_end"] != nil || raw["ends_after_sec"] != nil {
		t.Errorf("neither flag sent %v (%v); want neither key", raw, err)
	}
	// Both flags, or a bad duration, are a usage error before any request.
	for _, args := range [][]string{{"--ends-in", "2d", "--no-end"}, {"--ends-in", "soon"}, {"--ends-in", "0s"}} {
		raw, err := sent(t, args...)
		if err == nil || raw != nil {
			t.Errorf("%v sent %v (%v); want a usage error and no request", args, raw, err)
		}
	}
}

func TestRunCreatePrintsTheAppliedEnd(t *testing.T) {
	end := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name string
		run  types.AgentRun
		want string
	}{
		{"an end", types.AgentRun{ID: uuid.New(), State: types.RunPending, EndsAt: &end}, "  ends: " + end.Format(time.RFC3339) + "\n"},
		{"no end", types.AgentRun{ID: uuid.New(), State: types.RunPending}, "  ends: never (no end)\n"},
	} {
		srv := newCmdServer(t, http.StatusCreated, map[string]any{"id": tc.run.ID, "state": tc.run.State, "ends_at": tc.run.EndsAt})
		out, err := execCmdCapture(t, "run", "--agent", "claude-code", "--url", srv.URL, "--token", "t")
		if err != nil || !strings.Contains(out, tc.want) {
			t.Errorf("%s: output %q (%v); want it to contain %q", tc.name, out, err, tc.want)
		}
	}
}
