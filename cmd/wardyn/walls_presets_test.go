// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// presetsServer answers /api/v1/wall-presets from a fixed list and records every other request.
type presetsServer struct {
	list string
	reqs []string
	body map[string]any
}

func newPresetsServer(t *testing.T, ps *presetsServer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/wall-presets":
			_, _ = io.WriteString(w, ps.list)
			return
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/wall-presets/"):
			_, _ = io.WriteString(w, `{"id":"read-only","scope":"builtin","name":"Read-only","version":1,"spec":{"push_rules":{"deny_all":true}}}`)
			return
		}
		ps.reqs = append(ps.reqs, r.Method+" "+r.URL.Path)
		ps.body = nil
		_ = json.Unmarshal(raw, &ps.body)
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/runs"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"`+uuid.NewString()+`","state":"PENDING","confinement_class":"CC2"}`)
		case r.Method == http.MethodPut && ps.body["expected_version"] == float64(99):
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"stale","reason":"wall_preset_version_conflict"}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"new","scope":"person","name":"Docs","version":2,"spec":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	idMine = "11111111-1111-1111-1111-111111111111"
	idOrg  = "22222222-2222-2222-2222-222222222222"
)

var presetsList = `{"presets":[
 {"id":"open","scope":"builtin","name":"Open","version":1,"spec":{}},
 {"id":"read-only","scope":"builtin","name":"Read-only","version":1,"spec":{"push_rules":{"deny_all":true},"github_capabilities":["code_read"]}},
 {"id":"` + idOrg + `","scope":"org","name":"Docs","version":3,"description":"docs only","spec":{"allowed_domains":["docs.example"]}},
 {"id":"` + idMine + `","scope":"person","name":"docs","version":1,"spec":{"auto_stop_after_sec":600}}],
 "publish":{"org":true,"groups":["g1"],"group_labels":{"g1":"Platform"},"groups_unverified":false}}`

func TestWallPresetsList(t *testing.T) {
	srv := newPresetsServer(t, &presetsServer{list: presetsList})
	out, err := execCmdCapture(t, "walls", "presets", "--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "NARROWS", "built in", "organisation", "yours",
		"github_capabilities, push_rules.deny_all", "allowed_domains", "nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
}

func TestWallPresetShowAndAmbiguousName(t *testing.T) {
	srv := newPresetsServer(t, &presetsServer{list: presetsList})
	out, err := execCmdCapture(t, "walls", "presets", "show", "READ-ONLY", "--url", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "deny_all: true") {
		t.Fatalf("show read-only = %q, %v", out, err)
	}
	_, err = execCmdCapture(t, "walls", "presets", "show", "docs", "--url", srv.URL, "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "more than one of your audiences (organisation, yours)") ||
		!strings.Contains(err.Error(), "--mine, --group or --org") {
		t.Fatalf("show docs err = %v, want the audiences and the flags", err)
	}
	if _, err = execCmdCapture(t, "walls", "presets", "show", "docs", "--org", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("show docs --org: %v", err)
	}
	if _, err = execCmdCapture(t, "walls", "presets", "show", "nope", "--url", srv.URL, "--token", "tok"); err == nil {
		t.Fatal("show of a missing preset succeeded")
	}
}

func writePresetFile(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWallPresetSet(t *testing.T) {
	ps := &presetsServer{list: presetsList}
	srv := newPresetsServer(t, ps)
	f := writePresetFile(t, "denied_domains: [evil.example]\n")
	run := func(args ...string) error {
		_, err := execCmdCapture(t, append([]string{"walls", "presets", "set"}, append(args, "--url", srv.URL, "--token", "tok")...)...)
		return err
	}
	// A new name in your own audience is a create.
	if err := run("Fresh", "-f", f); err != nil || ps.reqs[len(ps.reqs)-1] != "POST /api/v1/wall-presets" || ps.body["scope"] != "person" {
		t.Fatalf("create: %v %v %v", err, ps.reqs, ps.body)
	}
	// The same name in an audience that has it replaces it, sending the version read and keeping the description.
	if err := run("docs", "--org", "-f", f); err != nil || ps.reqs[len(ps.reqs)-1] != "PUT /api/v1/wall-presets/"+idOrg ||
		ps.body["expected_version"] != float64(3) || ps.body["description"] != "docs only" {
		t.Fatalf("replace: %v %v %v", err, ps.reqs, ps.body)
	}
	// A group is named by its label and sent by its id.
	if err := run("Team", "--group", "platform", "-f", f); err != nil || ps.body["scope"] != "group" || ps.body["group_id"] != "g1" {
		t.Fatalf("group: %v %v", err, ps.body)
	}
	// A built-in's name is refused before any request, and so is a file naming no limit.
	n := len(ps.reqs)
	if err := run("Sealed", "-f", f); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("built-in name: %v", err)
	}
	if err := run("Empty", "-f", writePresetFile(t, "{}\n")); err == nil {
		t.Fatal("an empty file was accepted")
	}
	if len(ps.reqs) != n {
		t.Fatalf("requests for refused sets: %v", ps.reqs[n:])
	}
	// A deny_all file is read like any other knob.
	if err := run("Quiet", "-f", writePresetFile(t, "push_rules:\n  deny_all: true\n")); err != nil {
		t.Fatalf("deny_all file: %v", err)
	}
}

func TestWallPresetSetVersionConflict(t *testing.T) {
	ps := &presetsServer{list: strings.Replace(presetsList, `"version":1,"spec":{"auto_stop`, `"version":99,"spec":{"auto_stop`, 1)}
	srv := newPresetsServer(t, ps)
	_, err := execCmdCapture(t, "walls", "presets", "set", "docs", "-f", writePresetFile(t, "auto_stop_after_sec: 60\n"), "--url", srv.URL, "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("conflict err = %v", err)
	}
}

func TestWallPresetDelete(t *testing.T) {
	ps := &presetsServer{list: presetsList}
	srv := newPresetsServer(t, ps)
	if _, err := execCmdCapture(t, "walls", "presets", "delete", "docs", "--url", srv.URL, "--token", "tok"); err == nil || len(ps.reqs) != 0 {
		t.Fatalf("ambiguous delete: %v %v", err, ps.reqs)
	}
	out, err := execCmdCapture(t, "walls", "presets", "delete", "docs", "--mine", "--url", srv.URL, "--token", "tok")
	if err != nil || ps.reqs[0] != "DELETE /api/v1/wall-presets/"+idMine || !strings.Contains(out, "keep it") {
		t.Fatalf("delete --mine: %q %v %v", out, err, ps.reqs)
	}
}

func TestRunWallPreset(t *testing.T) {
	ps := &presetsServer{list: presetsList}
	srv := newPresetsServer(t, ps)
	out, err := execCmdCapture(t, "run", "--agent", "claude-code", "--wall-preset", "Read-Only", "--url", srv.URL, "--token", "tok")
	if err != nil || ps.body["wall_preset"] != "read-only" || !strings.Contains(out, "Wall preset: Read-only (built in, version 1).") {
		t.Fatalf("builtin: %q %v %v", out, err, ps.body)
	}
	if _, err = execCmdCapture(t, "run", "--agent", "claude-code", "--wall-preset", idOrg, "--url", srv.URL, "--token", "tok"); err != nil || ps.body["wall_preset"] != idOrg {
		t.Fatalf("by id: %v %v", err, ps.body)
	}
	n := len(ps.reqs)
	_, err = execCmdCapture(t, "run", "--agent", "claude-code", "--wall-preset", "docs", "--url", srv.URL, "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "more than one of your audiences") || len(ps.reqs) != n {
		t.Fatalf("ambiguous name: %v, requests %v", err, ps.reqs[n:])
	}
}

// --wall-preset is the one flag besides the run's name that goes with --file.
func TestRunFileAcceptsWallPreset(t *testing.T) {
	if !runFileFlags["wall-preset"] {
		t.Fatal("--wall-preset is refused beside --file")
	}
	if types.IsBuiltinWallPresetName("docs") {
		t.Fatal("test fixture name collides with a built-in")
	}
}
