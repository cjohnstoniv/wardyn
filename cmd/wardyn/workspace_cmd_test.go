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
	"sync"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workspaceAPI answers the /workspaces family and the library-source lookup
// --attach resolves through, and keeps the last create body.
type workspaceAPI struct {
	*httptest.Server
	mu       sync.Mutex
	created  sdk.WorkspaceRequest
	creates  int
	lastPath string
	list     []types.Workspace
	truncate bool
}

func newWorkspaceAPI(t *testing.T, lib types.Source, ws types.Workspace) *workspaceAPI {
	t.Helper()
	a := &workspaceAPI{list: []types.Workspace{ws}}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.lastPath = r.Method + " " + r.URL.RequestURI()
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workspaces":
			body, _ := io.ReadAll(r.Body)
			a.mu.Lock()
			a.creates++
			_ = json.Unmarshal(body, &a.created)
			a.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(ws)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/workspaces":
			if a.truncate {
				w.Header().Set("X-Wardyn-Truncated", "true")
			}
			_ = json.NewEncoder(w).Encode(a.list)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/workspaces/"+ws.ID.String():
			_ = json.NewEncoder(w).Encode(ws)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/workspaces/"+ws.ID.String():
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workspaces/"+ws.ID.String()+"/scan":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"scan_run_ids":["r1"]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sources/"+lib.ID.String():
			_ = json.NewEncoder(w).Encode(lib)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not found","reason":"not_found"}`)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

func TestWorkspaceCommands(t *testing.T) {
	libDir := types.Source{ID: uuid.New(), Kind: types.SourceLocalDir, Locator: "/srv/shared"}
	libRepo := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: "acme/lib", Ref: "v2"}
	ws := types.Workspace{
		ID: uuid.New(), Name: "payments", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/payments"}},
	}
	api := newWorkspaceAPI(t, libDir, ws)
	base := []string{"--url", api.URL, "--token", "tok"}
	run := func(args ...string) (string, string, error) {
		root := rootCmd()
		root.SetArgs(append(args, base...))
		var out, errOut strings.Builder
		root.SetOut(&out)
		root.SetErr(&errOut)
		err := root.Execute()
		return out.String(), errOut.String(), err
	}

	t.Run("create from --add and --attach resolves the library source and names the workspace after the first source", func(t *testing.T) {
		out, _, err := run("workspace", "create", "--add", "dir:/work/api@/work/api", "--attach", libDir.ID.String()+"@/work/shared:rw")
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("created workspace %s (%q, local_dir /srv/payments, status scanned)", ws.ID, "payments"); !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
		got := api.created
		if got.Name != "/work/api" || len(got.Sources) != 2 {
			t.Fatalf("sent %+v, want the name defaulted to the first --add path and two sources", got)
		}
		if s := got.Sources[1]; s.Type != types.WorkspaceSourceTypeLocalDir || s.Path != "/srv/shared" || s.Target != "/work/shared" || !s.Writable {
			t.Errorf("attached source sent as %+v, want the library dir resolved, at its target, read-write", s)
		}
	})

	t.Run("create attaching a library repo carries its ref", func(t *testing.T) {
		lib := libRepo
		api2 := newWorkspaceAPI(t, lib, ws)
		root := rootCmd()
		root.SetArgs([]string{"workspace", "create", "--name", "w", "--attach", lib.ID.String(), "--json", "--url", api2.URL, "--token", "tok"})
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&strings.Builder{})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if s := api2.created.Sources; len(s) != 1 || s[0].Type != types.WorkspaceSourceTypeRepo || s[0].Source != "acme/lib" || s[0].Ref != "v2" {
			t.Errorf("sent sources %+v, want the library repo at its ref", s)
		}
		if !strings.Contains(out.String(), `"name": "payments"`) {
			t.Errorf("--json output %q is not the created workspace", out.String())
		}
	})

	t.Run("create refuses a bad --add and an unresolvable --attach before creating anything", func(t *testing.T) {
		before := api.creates
		if _, _, err := run("workspace", "create", "--add", "volume:/x"); err == nil || !strings.Contains(err.Error(), "type must be dir, repo, or ephemeral") {
			t.Errorf("bad --add: err = %v", err)
		}
		missing := uuid.NewString()
		if _, _, err := run("workspace", "create", "--attach", missing); err == nil || !strings.Contains(err.Error(), "resolve --attach "+missing) {
			t.Errorf("unresolvable --attach: err = %v, want it to name the source id", err)
		}
		if api.creates != before {
			t.Errorf("a refused create reached the server (%d create call(s) beyond the %d before)", api.creates-before, before)
		}
	})

	t.Run("list prints a row per workspace and warns when the server truncated", func(t *testing.T) {
		api.truncate = true
		out, errOut, err := run("workspace", "list", "--limit", "1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "COMPOSITION") || !strings.Contains(out, ws.ID.String()) || !strings.Contains(out, "local_dir /srv/payments") {
			t.Errorf("list output:\n%s", out)
		}
		if !strings.Contains(errOut, "workspace list truncated at 1 row(s)") || !strings.Contains(errOut, "--offset=1") {
			t.Errorf("stderr %q lacks the truncation warning pointing at --offset=1", errOut)
		}
		if !strings.Contains(api.lastPath, "limit=1") {
			t.Errorf("request %q did not carry --limit", api.lastPath)
		}
		api.truncate = false
		if out, _, err := run("workspace", "list", "--json"); err != nil || !strings.Contains(out, `"name": "payments"`) {
			t.Errorf("list --json = %q, %v", out, err)
		}
	})

	t.Run("get, delete and scan address the workspace by id", func(t *testing.T) {
		id := ws.ID.String()
		if out, _, err := run("workspace", "get", id); err != nil || !strings.Contains(out, "payments") {
			t.Errorf("get = %q, %v, want the one-line table", out, err)
		}
		if out, _, err := run("workspace", "get", id, "--json"); err != nil || !strings.Contains(out, `"status": "scanned"`) {
			t.Errorf("get --json = %q, %v", out, err)
		}
		if out, _, err := run("workspace", "scan", id); err != nil || !strings.Contains(out, "scan_run_ids") {
			t.Errorf("scan = %q, %v, want the raw reply", out, err)
		}
		if out, _, err := run("workspace", "rm", id); err != nil || !strings.Contains(out, "workspace "+id+" deleted") || api.lastPath != "DELETE /api/v1/workspaces/"+id {
			t.Errorf("rm = %q, %v (last request %q)", out, err, api.lastPath)
		}
		for _, verb := range []string{"get", "delete", "scan"} {
			if _, _, err := run("workspace", verb, "not-a-uuid"); err == nil {
				t.Errorf("%s accepted a malformed id", verb)
			}
		}
		if _, _, err := run("workspace", "get", uuid.NewString()); err == nil {
			t.Error("get of an unknown workspace succeeded")
		}
	})
}
