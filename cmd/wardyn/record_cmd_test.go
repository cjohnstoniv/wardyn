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

// TestRecordSaveAsTemplate: a member cannot write a policy, so --template saves the
// synthesized profile as a person-scope template, and the 403 without it names the flag.
func TestRecordSaveAsTemplate(t *testing.T) {
	runID, tplID := uuid.New(), uuid.New()
	var tplBody map[string]any
	var policyPosts int
	profile := profileBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/runs/" + runID.String() + "/profile/synthesize":
			fmt.Fprint(w, profile)
		case "POST /api/v1/policies":
			policyPosts++
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"operator only","reason":"operator_required"}`)
		case "POST /api/v1/templates":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &tplBody)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%q,"name":"learned","scope":"person"}`, tplID)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	run := func(args ...string) (string, error) {
		return execCmdCapture(t, append(args, "--url", srv.URL, "--token", "tok")...)
	}

	out, err := run("record", "save", runID.String(), "--name", "learned", "--template")
	if err != nil {
		t.Fatal(err)
	}
	if want := `saved sandbox profile as your template "learned" (id ` + tplID.String() + `)`; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if policyPosts != 0 {
		t.Errorf("--template wrote %d policies, want none", policyPosts)
	}
	if tplBody["scope"] != "person" || tplBody["name"] != "learned" || tplBody["group_id"] != nil {
		t.Errorf("template request = %v, want person scope, the name, no group", tplBody)
	}
	doc, _ := tplBody["document"].(map[string]any)
	intent, _ := doc["intent"].(map[string]any)
	pol, _ := intent["inline_policy"].(map[string]any)
	if d, _ := pol["allowed_domains"].([]any); len(d) != 2 {
		t.Errorf("inline_policy %v, want the synthesized allowed domains", pol)
	}
	if pol["min_confinement_class"] != "CC2" || pol["allow_all_egress"] != false {
		t.Errorf("inline_policy %v, want the confinement floor and allow_all_egress false", pol)
	}
	// A recording with no grants decides "no grant": the key is written empty, or the source policy's grants survive.
	if g, has := pol["eligible_grants"].([]any); !has || len(g) != 0 {
		t.Errorf("inline_policy eligible_grants = %v (present %v), want an explicit empty list", pol["eligible_grants"], has)
	}
	for _, zero := range []string{"workspace_mounts", "resources", "llm_inspection", "denied_domains", "allowed_methods"} {
		if _, has := pol[zero]; has {
			t.Errorf("inline_policy carries the key %q a recording never decides: it must inherit", zero)
		}
	}
	if doc["kind"] != "RunTemplate" || doc["coverage"] != "partial" {
		t.Errorf("document = %v, want a partial RunTemplate", doc)
	}

	// A recorded grant is kept as it is, and the keys a recording never decides stay out even when the proposal fills them.
	profile = `{"proposed":{"inline_policy":{"allowed_domains":[],"min_confinement_class":"CC2","auto_stop_after_sec":3600,` +
		`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.example.com","header":"x-api-key"},"ttl_seconds":600}],` +
		`"denied_domains":["bad.example.com"],"resources":{"cpu_millis":2000},"workspace_mounts":[{"host_path":"/srv"}]}}}`
	if _, err := run("record", "save", runID.String(), "--name", "learned", "--template"); err != nil {
		t.Fatal(err)
	}
	doc, _ = tplBody["document"].(map[string]any)
	intent, _ = doc["intent"].(map[string]any)
	pol, _ = intent["inline_policy"].(map[string]any)
	grants, _ := pol["eligible_grants"].([]any)
	if len(grants) != 1 {
		t.Fatalf("eligible_grants = %v, want the one recorded grant", pol["eligible_grants"])
	}
	if g, _ := grants[0].(map[string]any); g["kind"] != "api_key" || g["ttl_seconds"] != float64(600) {
		t.Errorf("grant = %v, want it unchanged", grants[0])
	}
	if d, has := pol["allowed_domains"].([]any); !has || len(d) != 0 || pol["auto_stop_after_sec"] != float64(3600) {
		t.Errorf("inline_policy = %v, want allowed_domains [] and auto_stop_after_sec 3600 kept", pol)
	}
	for _, key := range []string{"denied_domains", "resources", "workspace_mounts"} {
		if _, has := pol[key]; has {
			t.Errorf("inline_policy carries %q, which a recording never decides", key)
		}
	}

	_, err = run("record", "save", runID.String(), "--name", "learned")
	if err == nil || !strings.Contains(err.Error(), "Members can save it as their own template with --template.") {
		t.Errorf("a 403 without --template: err = %v, want the hint naming the flag", err)
	}
}

func TestOrDashShowsEmptyAndNullAsADash(t *testing.T) {
	for in, want := range map[string]string{"": "-", "null": "-", "low": "low"} {
		if got := orDash(in); got != want {
			t.Errorf("orDash(%q) = %q, want %q", in, got, want)
		}
	}
}
