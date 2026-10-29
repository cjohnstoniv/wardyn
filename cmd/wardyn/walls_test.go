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
