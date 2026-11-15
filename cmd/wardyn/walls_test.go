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
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsStore is a /api/v1/me/walls that keeps what was put, as the server does.
type wallsStore struct {
	spec    json.RawMessage
	methods []string
}

func newWallsServer(t *testing.T) (*httptest.Server, *wallsStore) {
	t.Helper()
	st := &wallsStore{spec: json.RawMessage(`{}`)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/walls" {
			http.NotFound(w, r)
			return
		}
		st.methods = append(st.methods, r.Method)
		switch r.Method {
		case http.MethodPut:
			var in struct {
				Spec json.RawMessage `json:"spec"`
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			st.spec = in.Spec
		case http.MethodDelete:
			st.spec = json.RawMessage(`{}`)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		out := map[string]any{"spec": st.spec}
		if string(st.spec) != "{}" {
			out["updated_at"] = time.Date(2020, 10, 29, 12, 0, 0, 0, time.UTC)
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

const wallsYAML = `# only these sites
allowed_domains:
  - pypi.org
  - "1.10"
  - "2020-01-02"
denied_domains: [evil.example]
first_use_approval: deny_with_review
min_confinement_class: CC2
desktop:
  copy_out: blocked
`

// A walls file survives set -> show -> set unchanged, and a host that YAML would read as a number or a
// date stays the text the file wrote.
func TestWallsYAMLRoundTrip(t *testing.T) {
	srv, st := newWallsServer(t)
	file := filepath.Join(t.TempDir(), "walls.yaml")
	if err := os.WriteFile(file, []byte(wallsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execCmdCapture(t, "walls", "set", "--file", file, "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	var got types.PersonalWallsSpec
	if err := json.Unmarshal(st.spec, &got); err != nil {
		t.Fatal(err)
	}
	if got.AllowedDomains == nil || strings.Join(*got.AllowedDomains, ",") != "pypi.org,1.10,2020-01-02" {
		t.Fatalf("allowed_domains = %v, want the three hosts as written", got.AllowedDomains)
	}
	if got.Desktop == nil || got.Desktop.CopyOut != types.TransferBlocked || got.FirstUseApproval == nil || *got.FirstUseApproval != types.FirstUseDenyWithReview {
		t.Fatalf("stored spec = %s", st.spec)
	}
	first := string(st.spec)

	shown, err := execCmdCapture(t, "walls", "--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(shown, "updated_at") || !strings.Contains(shown, "evil.example") {
		t.Fatalf("walls printed:\n%s", shown)
	}
	// What `walls` prints is what `walls set` reads.
	out := filepath.Join(t.TempDir(), "shown.yaml")
	if err := os.WriteFile(out, []byte(shown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execCmdCapture(t, "walls", "set", "-f", out, "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	if string(st.spec) != first {
		t.Fatalf("spec after the round trip = %s, want %s", st.spec, first)
	}
}

const narrowingYAML = `azure_devops_capabilities: [code_read, pr]
github_capabilities: [code_read]
tool_rules:
  - {tool: Bash, effect: deny}
  - {tool: "*", effect: hold}
push_rules:
  deny_paths: [secrets/**, .github/workflows/**]
recording: screen_actions
auto_stop_after_sec: 600
`

// The capability lists, tool rules, push deny paths, recording floor and auto-stop cap are read from a
// file as the spec's own typed fields, and what `walls` prints for them is what `walls set` reads.
func TestWallsYAMLRoundTripNarrowingKnobs(t *testing.T) {
	srv, st := newWallsServer(t)
	file := filepath.Join(t.TempDir(), "walls.yaml")
	if err := os.WriteFile(file, []byte(narrowingYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execCmdCapture(t, "walls", "set", "--file", file, "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	var got types.PersonalWallsSpec
	if err := json.Unmarshal(st.spec, &got); err != nil {
		t.Fatal(err)
	}
	if got.AzureDevOpsCapabilities == nil || len(*got.AzureDevOpsCapabilities) != 2 || got.GitHubCapabilities == nil || len(*got.GitHubCapabilities) != 1 ||
		got.ToolRules == nil || len(*got.ToolRules) != 2 || (*got.ToolRules)[0].Effect != types.ToolDeny ||
		got.AddedDenyPaths() == nil || len(*got.AddedDenyPaths()) != 2 ||
		got.Recording == nil || *got.Recording != types.RecordingScreenActions || got.AutoStopAfterSec == nil || *got.AutoStopAfterSec != 600 {
		t.Fatalf("stored spec = %s", st.spec)
	}
	first := string(st.spec)

	shown, err := execCmdCapture(t, "walls", "--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"azure_devops_capabilities:", "github_capabilities:", "tool_rules:", "deny_paths:", "recording: screen_actions", "auto_stop_after_sec: 600"} {
		if !strings.Contains(shown, want) {
			t.Errorf("walls printed:\n%s\nwant %q", shown, want)
		}
	}
	out := filepath.Join(t.TempDir(), "shown.yaml")
	if err := os.WriteFile(out, []byte(shown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execCmdCapture(t, "walls", "set", "-f", out, "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	if string(st.spec) != first {
		t.Fatalf("spec after the round trip = %s, want %s", st.spec, first)
	}
}

// A file that names a knob walls do not have is refused before any request, and a file that names one
// of the new knobs alone is a limit, not an empty document.
func TestWallsSetNarrowingKnobsFile(t *testing.T) {
	srv, st := newWallsServer(t)
	for name, body := range map[string]string{
		"a push rule that is not a deny path": "push_rules:\n  max_file_size_mib: 5\n",
		"a misspelt knob":                     "auto_stop: 600\n",
	} {
		file := filepath.Join(t.TempDir(), "walls.yaml")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := execCmdCapture(t, "walls", "set", "-f", file, "--url", srv.URL, "--token", "tok"); err == nil {
			t.Errorf("%s: set succeeded, want a refusal", name)
		}
	}
	if len(st.methods) != 0 {
		t.Errorf("requests sent for a refused file: %v", st.methods)
	}
	file := filepath.Join(t.TempDir(), "walls.yaml")
	if err := os.WriteFile(file, []byte("auto_stop_after_sec: 600\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execCmdCapture(t, "walls", "set", "-f", file, "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("a file naming only auto_stop_after_sec: %v", err)
	}
}

// A site YAML would read as a number or a date is refused rather than written back in another spelling.
func TestWallsSetRefusesAHostThatIsNotText(t *testing.T) {
	srv, st := newWallsServer(t)
	for _, host := range []string{"1.10", "2020-01-02", "true", "42"} {
		err := execCmdStdin(t, "allowed_domains:\n  - "+host+"\n", "walls", "set", "-f", "-", "--url", srv.URL, "--token", "tok")
		if err == nil || !strings.Contains(err.Error(), "quotes") {
			t.Errorf("%s: err = %v, want the put-it-in-quotes refusal", host, err)
		}
	}
	if len(st.methods) != 0 {
		t.Fatalf("requests sent: %v", st.methods)
	}
}

// One YAML reader checks and decodes the hosts, so the YAML 1.1 booleans (on, yes, off, no, y, n in any
// case) are sent as the text they were written as, never as true or false; a quoted host, whatever it
// would read as unquoted, is sent as written too.
func TestWallsSetSendsAHostAsWritten(t *testing.T) {
	srv, st := newWallsServer(t)
	for host, want := range map[string]string{
		"yes": "yes", "no": "no", "on": "on", "off": "off", "Yes": "Yes", "ON": "ON", "y": "y", "n": "n",
		`"on"`: "on", `'yes'`: "yes", `"true"`: "true", `"1.10"`: "1.10",
	} {
		if err := execCmdStdin(t, "allowed_domains: ["+host+"]\n", "walls", "set", "-f", "-", "--url", srv.URL, "--token", "tok"); err != nil {
			t.Errorf("%s: %v", host, err)
			continue
		}
		if got := string(st.spec); got != `{"allowed_domains":["`+want+`"]}` {
			t.Errorf("%s: stored spec = %s, want the host %q", host, got, want)
		}
	}
}

func TestWallsSetReadsStdin(t *testing.T) {
	srv, st := newWallsServer(t)
	if err := execCmdStdin(t, "denied_domains: [evil.example]\n", "walls", "set", "-f", "-", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	if string(st.spec) != `{"denied_domains":["evil.example"]}` {
		t.Fatalf("stored spec = %s", st.spec)
	}
}

// A file the spec cannot hold is refused before anything is sent: a misspelt key would otherwise be no
// limit at all, and an empty file would silently clear the walls.
func TestWallsSetRefusesAFileItCannotHold(t *testing.T) {
	srv, st := newWallsServer(t)
	for name, doc := range map[string]string{
		"unknown key":   "denied_domain: [evil.example]\n",
		"empty":         "",
		"names nothing": "desktop: {}\n",
		"not a mapping": "- pypi.org\n",
	} {
		t.Run(name, func(t *testing.T) {
			err := execCmdStdin(t, doc, "walls", "set", "-f", "-", "--url", srv.URL, "--token", "tok")
			if err == nil {
				t.Fatal("set succeeded, want a refusal")
			}
		})
	}
	if len(st.methods) != 0 {
		t.Fatalf("requests sent for refused files: %v", st.methods)
	}
}

// The whole walls document is replaced, so a second YAML document, or text after the first that does
// not parse, is refused before any request: reading only the first would drop the limits after it.
func TestWallsSetRefusesMoreThanOneDocument(t *testing.T) {
	srv, st := newWallsServer(t)
	st.spec = json.RawMessage(`{"allowed_domains":[]}`)
	for name, doc := range map[string]string{
		"second document":          "auto_stop_after_sec: 600\n---\nallowed_domains: []\n",
		"empty second document":    "auto_stop_after_sec: 600\n---\n",
		"text that does not parse": "auto_stop_after_sec: 600\n---\n: : [\n",
	} {
		t.Run(name, func(t *testing.T) {
			err := execCmdStdin(t, doc, "walls", "set", "-f", "-", "--url", srv.URL, "--token", "tok")
			if err == nil || !strings.Contains(err.Error(), "the walls file does not parse") {
				t.Fatalf("err = %v, want the walls file refused", err)
			}
		})
	}
	// The same reader serves a wall preset.
	err := execCmdStdin(t, "denied_domains: [evil.example]\n---\n{}\n", "walls", "presets", "set", "mine", "-f", "-", "--url", srv.URL, "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "exactly one document") {
		t.Errorf("walls presets set = %v, want the one-document refusal", err)
	}
	if len(st.methods) != 0 || string(st.spec) != `{"allowed_domains":[]}` {
		t.Fatalf("requests %v, stored %s: want none sent and the stored walls kept", st.methods, st.spec)
	}
	// A document closed with the end marker is one document.
	if err := execCmdStdin(t, "auto_stop_after_sec: 600\n...\n", "walls", "set", "-f", "-", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("one document with its end marker: %v", err)
	}
	if string(st.spec) != `{"auto_stop_after_sec":600}` {
		t.Errorf("stored spec = %s", st.spec)
	}
}

func TestWallsClear(t *testing.T) {
	srv, st := newWallsServer(t)
	st.spec = json.RawMessage(`{"denied_domains":["evil.example"]}`)
	out, err := execCmdCapture(t, "walls", "clear", "--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.methods) != 1 || st.methods[0] != http.MethodDelete || string(st.spec) != "{}" {
		t.Fatalf("requests = %v, spec = %s, want one DELETE", st.methods, st.spec)
	}
	if !strings.Contains(out, "removed your walls") {
		t.Fatalf("stdout = %q", out)
	}
	shown, err := execCmdCapture(t, "walls", "--url", srv.URL, "--token", "tok")
	if err != nil || shown != "{}\n" {
		t.Fatalf("walls after clear = %q, %v, want {}", shown, err)
	}
}

func TestWallsJSON(t *testing.T) {
	srv, st := newWallsServer(t)
	st.spec = json.RawMessage(`{"allow_all_egress":false}`)
	out, err := execCmdCapture(t, "walls", "--json", "--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	var w types.PersonalWalls
	if err := json.Unmarshal([]byte(out), &w); err != nil || w.Spec.AllowAllEgress == nil || *w.Spec.AllowAllEgress {
		t.Fatalf("--json = %s (%v)", out, err)
	}
}
