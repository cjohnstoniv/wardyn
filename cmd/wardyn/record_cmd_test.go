// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// profileBody is a synthesized profile with one observed domain, one anomaly
// and one warning: every section printProfile renders.
const profileBody = `{
  "proposed": {"inline_policy": {"allowed_domains": ["pypi.org", "api.anthropic.com"], "min_confinement_class": "CC2", "allow_all_egress": false}},
  "overall_risk": "low",
  "observations": {"domains": [{"host": "pypi.org", "methods": ["GET"]}], "anomalies": ["connected to 203.0.113.9 directly"]},
  "warnings": ["one grant was dropped"]
}`

func TestRecordCommands(t *testing.T) {
	runID, wsID, policyID := uuid.New(), uuid.New(), uuid.New()
	var policyBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/runs/" + runID.String() + "/profile/synthesize":
			fmt.Fprint(w, profileBody)
		case "POST /api/v1/policies":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &policyBody)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%q,"name":"learned"}`, policyID)
		case "POST /api/v1/workspaces/" + wsID.String() + "/record":
			fmt.Fprintf(w, `{"record_run_id":"rec-1","task_key":"build & test","mode":"open","detail":"attach to drive it","warnings":["egress is open"]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no such run","reason":"run_not_found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	run := func(args ...string) (string, error) {
		return execCmdCapture(t, append(args, "--url", srv.URL, "--token", "tok")...)
	}

	t.Run("synthesize prints every section of the profile", func(t *testing.T) {
		out, err := run("record", "synthesize", runID.String())
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"overall risk: low", "min confinement: CC2", "allowed domains (2): [pypi.org api.anthropic.com]",
			"eligible grants: 0", "observed domains:", "- pypi.org [GET]", "ANOMALIES (1):", "! connected to 203.0.113.9 directly",
			"warning: one grant was dropped",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if out, err := run("record", "synthesize", runID.String(), "--json"); err != nil || !strings.Contains(out, `"overall_risk": "low"`) {
			t.Errorf("--json = %q, %v", out, err)
		}
		if _, err := run("record", "synthesize", "not-a-uuid"); err == nil {
			t.Error("a malformed run id was accepted")
		}
		if _, err := run("record", "synthesize", uuid.NewString()); err == nil || !strings.Contains(err.Error(), "no such run") {
			t.Errorf("unknown run: err = %v, want the server's message", err)
		}
	})

	t.Run("save stores the synthesized policy under the given name and says how to launch it", func(t *testing.T) {
		out, err := run("record", "save", runID.String(), "--name", "learned")
		if err != nil {
			t.Fatal(err)
		}
		if policyBody["name"] != "learned" {
			t.Errorf("policy request = %v, want the name sent", policyBody)
		}
		spec, _ := policyBody["spec"].(map[string]any)
		if d, _ := spec["allowed_domains"].([]any); len(d) != 2 {
			t.Errorf("saved spec %v, want the synthesized allowed domains", spec)
		}
		for _, want := range []string{`saved sandbox profile as policy "learned" (id ` + policyID.String() + `)`, "--policy " + policyID.String()} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if out, err := run("record", "save", runID.String(), "--name", "learned", "--json"); err != nil || !strings.Contains(out, policyID.String()) {
			t.Errorf("--json = %q, %v", out, err)
		}
		if _, err := run("record", "save", runID.String()); err == nil || !strings.Contains(err.Error(), `required flag(s) "name" not set`) {
			t.Errorf("without --name: err = %v, want cobra's required-flag message", err)
		}
		if _, err := run("record", "save", "not-a-uuid", "--name", "x"); err == nil {
			t.Error("a malformed run id was accepted")
		}
	})

	t.Run("task launches a recording run and prints what to do next", func(t *testing.T) {
		out, err := run("record", "task", wsID.String(), "build & test")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"record run rec-1 launched (task build & test, mode open)", "attach to drive it", "warning: egress is open",
			"`wardyn record synthesize rec-1`",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if out, err := run("record", "task", wsID.String(), "k", "--json"); err != nil || !strings.Contains(out, `"record_run_id": "rec-1"`) {
			t.Errorf("--json = %q, %v", out, err)
		}
		if _, err := run("record", "task", "not-a-uuid", "k"); err == nil {
			t.Error("a malformed workspace id was accepted")
		}
	})
}

func TestOrDashShowsEmptyAndNullAsADash(t *testing.T) {
	for in, want := range map[string]string{"": "-", "null": "-", "low": "low"} {
		if got := orDash(in); got != want {
			t.Errorf("orDash(%q) = %q, want %q", in, got, want)
		}
	}
}
