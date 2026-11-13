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
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestWindowOpensAndSaysSo(t *testing.T) {
	id := uuid.New()
	srv := newCmdServer(t, http.StatusOK, map[string]any{"grant_id": uuid.New(), "runner_id": uuid.New(), "runner": "studio", "trigger": "cli"})
	out, err := execCmdCapture(t, "window", id.String(), "--url", srv.URL, "--token", "t")
	if err != nil {
		t.Fatal(err)
	}
	req := srv.last()
	if req.method != http.MethodPost || req.path != "/api/v1/runs/"+id.String()+"/desktop/window" {
		t.Fatalf("request = %s %s", req.method, req.path)
	}
	if !strings.Contains(out, "opening on studio") || !strings.Contains(out, "hands the screen back") {
		t.Fatalf("output = %q", out)
	}
}

func TestWindowRefusalIsTheServersSentence(t *testing.T) {
	const sentence = "Only a macOS VM run has a window."
	srv := newCmdServer(t, http.StatusConflict, map[string]string{"error": sentence})
	_, err := execCmdCapture(t, "window", uuid.NewString(), "--url", srv.URL, "--token", "t")
	if err == nil || err.Error() != sentence {
		t.Fatalf("err = %v, want exactly %q", err, sentence)
	}
}

func TestWindowStatusShowsStateAndLastClose(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, map[string]any{"mac_window": map[string]any{
		"state": "paused", "message": "This run is paused. You can open its window once it is running again.",
		"last_close": map[string]any{"reason": "host_locked", "at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}}})
	out, err := execCmdCapture(t, "window", uuid.NewString(), "--status", "--url", srv.URL, "--token", "t")
	if err != nil {
		t.Fatal(err)
	}
	if srv.last().method != http.MethodGet {
		t.Fatalf("--status sent %s, want GET", srv.last().method)
	}
	for _, want := range []string{"window: paused", "This run is paused.", "host_locked"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestRunWindowFlagAsksAtStart(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
	if err := execCmd(t, "run", "--agent", "claude-code", "--machine-kind", "macos_vm", "--window", "--url", srv.URL, "--token", "t"); err != nil {
		t.Fatal(err)
	}
	var body sdk.CreateRunRequest
	if err := json.Unmarshal(srv.last().body, &body); err != nil || body.DesktopWindow != "at_start" {
		t.Fatalf("body = %s, want desktop_window=at_start", srv.last().body)
	}
	if err := execCmd(t, "run", "--agent", "claude-code", "--url", srv.URL, "--token", "t"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(srv.last().body), "desktop_window") {
		t.Fatalf("desktop_window sent without --window: %s", srv.last().body)
	}
}

// The refusal is shown as the server's sentence, and the exit code still follows its status class.
func TestWindowRefusalKeepsItsExitClass(t *testing.T) {
	const sentence = "Only a macOS VM run has a window."
	for status, code := range map[int]int{http.StatusConflict: 3, http.StatusForbidden: 2, http.StatusServiceUnavailable: 4} {
		srv := newCmdServer(t, status, map[string]string{"error": sentence})
		_, err := execCmdCapture(t, "window", uuid.NewString(), "--url", srv.URL, "--token", "t")
		if err == nil || err.Error() != sentence || exitCodeFor(err) != code {
			t.Errorf("%d: err = %v (exit %d), want exactly %q with exit %d", status, err, exitCodeFor(err), sentence, code)
		}
	}
}
