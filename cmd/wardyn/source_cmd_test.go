// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sourceAPI answers the /sources family the way the server does and records
// what the CLI sent, so each subcommand's request and its output are both
// pinned.
type sourceAPI struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string // "METHOD path?query"
	body []byte   // last request body
}

func newSourceAPI(t *testing.T, srcs []types.Source, scanID uuid.UUID) *sourceAPI {
	t.Helper()
	a := &sourceAPI{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen = append(a.seen, r.Method+" "+r.URL.RequestURI())
		a.body = nil
		if r.Body != nil {
			var raw json.RawMessage
			if json.NewDecoder(r.Body).Decode(&raw) == nil {
				a.body = raw
			}
		}
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sources":
			_ = json.NewEncoder(w).Encode(map[string]any{"sources": srcs})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sources":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(srcs[0])
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scan"):
			if strings.Contains(r.URL.Path, scanID.String()) {
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprintf(w, `{"scan_run_id":%q}`, scanID)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no such source","reason":"source_not_found"}`)
		case r.Method == http.MethodDelete && r.URL.Query().Get("force") != "":
			_ = json.NewEncoder(w).Encode(map[string]any{"detached_from": []string{"ws-one", "ws-two"}})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"source is attached to workspace ws-one","reason":"source_in_use"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *sourceAPI) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[len(a.seen)-1]
}

func TestSourceCommands(t *testing.T) {
	repo := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: "acme/widgets", Ref: "main", Status: types.WorkspaceScanned,
		Requirements: map[string]types.WorkspaceRequirement{"secret:A": {}, "egress:pypi.org": {}}}
	dir := types.Source{ID: uuid.New(), Kind: types.SourceLocalDir, Locator: "/srv/work", Status: types.WorkspacePendingScan}
	scanID := uuid.New()
	api := newSourceAPI(t, []types.Source{repo, dir}, scanID)
	base := []string{"--url", api.URL, "--token", "tok"}
	run := func(args ...string) (string, error) {
		return execCmdCapture(t, append(args, base...)...)
	}

	t.Run("list prints one row per source with its ref and contract size", func(t *testing.T) {
		out, err := run("source", "list")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"ID", "CONTRACT", repo.ID.String(), "acme/widgets", "main", "2 reqs", dir.ID.String(), "/srv/work"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		var dirRow string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, dir.ID.String()) {
				dirRow = line
			}
		}
		if f := strings.Fields(dirRow); len(f) != 6 || f[3] != "-" || f[5] != "-" {
			t.Errorf("dir row %q: want an empty ref and an empty contract shown as '-'", dirRow)
		}
	})

	t.Run("list --json emits the contract rows", func(t *testing.T) {
		out, err := run("source", "list", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var got []types.Source
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 2 || len(got[0].Requirements) != 2 {
			t.Errorf("--json output = %q (err %v), want both sources with the first one's two contract rows", out, err)
		}
	})

	t.Run("create posts the flags and reports the source", func(t *testing.T) {
		out, err := run("source", "create", "--kind", "repo", "--locator", "acme/widgets", "--ref", "main", "--name", "widgets")
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("source %s (repo acme/widgets, status scanned)", repo.ID); !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
		var sent map[string]any
		if err := json.Unmarshal(api.body, &sent); err != nil || sent["kind"] != "repo" || sent["locator"] != "acme/widgets" || sent["ref"] != "main" || sent["name"] != "widgets" {
			t.Errorf("request body = %s (err %v), want the flags as sent", api.body, err)
		}
		if out, err := run("source", "create", "--kind", "repo", "--locator", "acme/widgets", "--json"); err != nil || !strings.Contains(out, `"locator": "acme/widgets"`) {
			t.Errorf("create --json = %q, %v, want the source as JSON", out, err)
		}
	})

	t.Run("scan prints the raw reply and refuses a malformed id before any request", func(t *testing.T) {
		out, err := run("source", "scan", scanID.String())
		if err != nil || !strings.Contains(out, scanID.String()) || api.last() != "POST /api/v1/sources/"+scanID.String()+"/scan" {
			t.Errorf("scan = %q, %v (last request %q), want the scan_run_id", out, err, api.last())
		}
		before := len(api.seen)
		if _, err := run("source", "scan", "not-a-uuid"); err == nil {
			t.Error("a malformed id was accepted")
		}
		if len(api.seen) != before {
			t.Error("a malformed id reached the server")
		}
		if _, err := run("source", "scan", uuid.NewString()); err == nil || !strings.Contains(err.Error(), "no such source") {
			t.Errorf("unknown source: err = %v, want the server's message", err)
		}
	})

	t.Run("delete surfaces the in-use refusal, and --force names what it detached", func(t *testing.T) {
		id := repo.ID.String()
		if _, err := run("source", "delete", id); err == nil || !strings.Contains(err.Error(), "attached to workspace ws-one") {
			t.Errorf("delete of an attached source: err = %v, want the 409 message", err)
		}
		out, err := run("source", "rm", id, "--force")
		if err != nil {
			t.Fatal(err)
		}
		if api.last() != "DELETE /api/v1/sources/"+id+"?force=1" {
			t.Errorf("last request = %q, want the forced delete", api.last())
		}
		for _, want := range []string{"source " + id + " deleted", "detached from: ws-one, ws-two"} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q lacks %q", out, want)
			}
		}
		if _, err := run("source", "delete", "not-a-uuid"); err == nil {
			t.Error("delete accepted a malformed id")
		}
	})
}
