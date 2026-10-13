// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// limitsServer answers the reads `runner set` and `runner pool create|update` make and records every
// write's body by method and path.
type limitsServer struct {
	*httptest.Server
	mu     sync.Mutex
	writes map[string]string
}

func newLimitsServer(t *testing.T, desk sdk.RunnerView, remote sdk.RemoteRunnerView, pool sdk.RunnerPool) *limitsServer {
	t.Helper()
	ls := &limitsServer{writes: map[string]string{}}
	answer := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		if r.Method != http.MethodGet {
			ls.mu.Lock()
			ls.writes[key] = string(body)
			ls.mu.Unlock()
		}
		switch {
		case key == "GET /api/v1/me/runners":
			answer(w, []sdk.RunnerView{desk})
		case key == "GET /api/v1/me/runners/"+desk.ID.String():
			answer(w, desk)
		case key == "PUT /api/v1/me/runners/"+desk.ID.String():
			var req types.RunnerUpdateRequest
			_ = json.Unmarshal(body, &req)
			v := desk
			v.Limits = req.Limits
			answer(w, v)
		case key == "GET /api/v1/remote-runners":
			answer(w, []sdk.RemoteRunnerView{remote})
		case key == "GET /api/v1/remote-runners/"+remote.ID, key == "PUT /api/v1/remote-runners/"+remote.ID:
			answer(w, remote)
		case key == "PUT /api/v1/remote-runners/"+remote.ID+"/people-policy":
			answer(w, remote.RemoteRunner)
		case key == "GET /api/v1/runner-pools":
			answer(w, sdk.RunnerPoolList{Pools: []sdk.RunnerPoolCatalogueEntry{{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: pool.ID, Name: pool.Name}}}})
		case strings.HasSuffix(key, "/runner-pools/"+pool.ID.String()), r.Method == http.MethodPost:
			answer(w, pool)
		default:
			http.Error(w, "unexpected "+key, http.StatusTeapot)
		}
	}))
	t.Cleanup(ls.Close)
	return ls
}

func (ls *limitsServer) wrote() map[string]string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	out := ls.writes
	ls.writes = map[string]string{}
	return out
}

// Every `wardyn runner set` line of the approved mock parses and reaches its handler: a limits change
// sends the whole set with only the named amounts changed, in GB and CPUs as the console says them; a
// remote-hosted runner's goes to the administrators' route at the revision read; the flag refusals the
// mock shows are made before any request.
func TestRunnerSetMockLines(t *testing.T) {
	desk := sdk.RunnerView{ID: uuid.New(), Name: "desk-2", State: types.RunnerClaimed, Kind: types.RunnerKindSelfHosted,
		Limits: &sdk.RunnerLimits{RunsAtOnce: 2, Total: sdk.RunnerAmounts{CPUMillis: 12000, MemoryMiB: 49152, DiskMiB: 819200},
			PerRun: sdk.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 16384, DiskMiB: 102400}}}
	remote := sdk.RemoteRunnerView{RemoteRunner: types.RemoteRunner{ID: "sandbox-cluster", Name: "sandbox-cluster", Revision: 3}}
	srv := newLimitsServer(t, desk, remote, sdk.RunnerPool{})
	run := func(args ...string) (string, error) {
		return execCmdCapture(t, append([]string{"--url", srv.URL, "--token", "t"}, args...)...)
	}

	out, err := run("runner", "set", "desk-2", "--max-runs", "4", "--total-memory", "32", "--per-run-memory", "8")
	if err != nil || strings.TrimSpace(out) != "limits for desk-2 updated: runs at once 4; memory 32 GB in total, 8 GB per run" {
		t.Fatalf("set limits = %q, %v", out, err)
	}
	var sent types.RunnerUpdateRequest
	if err := json.Unmarshal([]byte(srv.wrote()["PUT /api/v1/me/runners/"+desk.ID.String()]), &sent); err != nil || sent.Limits == nil {
		t.Fatalf("set limits sent %+v, %v", sent, err)
	}
	want := *desk.Limits
	want.RunsAtOnce, want.Total.MemoryMiB, want.PerRun.MemoryMiB = 4, 32*1024, 8*1024
	if *sent.Limits != want {
		t.Fatalf("set limits sent %+v, want %+v", *sent.Limits, want)
	}

	if out, err := run("runner", "set", "sandbox-cluster", "--available-to-people", "on"); err != nil || !strings.Contains(out, "sandbox-cluster is now available to people's pools") {
		t.Fatalf("available to people = %q, %v", out, err)
	}
	if _, ok := srv.wrote()["PUT /api/v1/remote-runners/sandbox-cluster/people-policy"]; !ok {
		t.Fatal("available to people wrote no people policy")
	}

	// A remote-hosted runner with no limits yet takes the whole set, and then gets it at the revision read.
	if _, err := run("runner", "set", "sandbox-cluster", "--total-cpu", "24"); err == nil || !strings.Contains(err.Error(), "no limits yet") {
		t.Fatalf("part of a first limit set = %v, want every flag asked for", err)
	}
	if _, err := run("runner", "set", "sandbox-cluster", "--max-runs", "6", "--total-cpu", "24", "--total-memory", "96", "--total-disk", "1000",
		"--per-run-cpu", "4", "--per-run-memory", "16", "--per-run-disk", "160"); err != nil {
		t.Fatal(err)
	}
	var remoteSent sdk.UpdateRemoteRunnerRequest
	if err := json.Unmarshal([]byte(srv.wrote()["PUT /api/v1/remote-runners/sandbox-cluster"]), &remoteSent); err != nil || remoteSent.Revision != 3 ||
		remoteSent.Limits == nil || remoteSent.Limits.RunsAtOnce != 6 || remoteSent.Limits.Total.CPUMillis != 24000 || remoteSent.Limits.PerRun.DiskMiB != 160*1024 {
		t.Fatalf("remote limits sent %+v, %v", remoteSent, err)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"runner", "set", "desk-2", "--per-run-memory", "64", "--total-memory", "32"}, "per-run memory (64) can't be more than total memory (32)"},
		{[]string{"runner", "set", "desk-2"}, "name something to change: --max-runs, a --total-* or --per-run-* flag"},
		{[]string{"runner", "set", "desk-2", "--max-runs", "0"}, "--max-runs is a whole number, 1 or more"},
	} {
		if _, err := run(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v = %v, want %q", tc.args, err, tc.want)
		}
		if w := srv.wrote(); len(w) != 0 {
			t.Errorf("%v wrote %v before refusing", tc.args, w)
		}
	}
}

// Every `wardyn runner pool create|update` limit line of the approved mock parses and reaches its
// handler. A flag left out is the pool's runners' to set: a run type with no amounts, no barrier list.
func TestRunnerPoolLimitFlagsMockLines(t *testing.T) {
	pool := sdk.RunnerPool{ID: uuid.New(), Name: "Laptop", Scope: sdk.RunnerPoolUser, Revision: 4}
	srv := newLimitsServer(t, sdk.RunnerView{ID: uuid.New()}, sdk.RemoteRunnerView{}, pool)
	run := func(args ...string) error {
		_, err := execCmdCapture(t, append([]string{"--url", srv.URL, "--token", "t"}, args...)...)
		return err
	}
	limitsOf := func(t *testing.T, w map[string]string, key string) sdk.RunnerPoolLimits {
		t.Helper()
		var body struct {
			Limits *sdk.RunnerPoolLimits `json:"limits"`
		}
		if err := json.Unmarshal([]byte(w[key]), &body); err != nil || body.Limits == nil {
			t.Fatalf("%s sent %q", key, w[key])
		}
		return *body.Limits
	}

	if err := run("runner", "pool", "create", "--name", "Laptop", "--run-type", "background", "--run-type", "interactive", "--barrier", "fence", "--barrier", "wall"); err != nil {
		t.Fatal(err)
	}
	l := limitsOf(t, srv.wrote(), "POST /api/v1/me/runner-pools")
	if l.Background == nil || l.Interactive == nil || l.Background.CPUMillis != nil || len(l.Barriers) != 2 || l.Barriers[1] != types.CC2 {
		t.Fatalf("run types and barriers = %+v", l)
	}

	if err := run("runner", "pool", "create", "--name", "Home server", "--run-type", "background", "--background-memory", "2:8", "--background-disk", "20:50", "--max-runs", "2"); err != nil {
		t.Fatal(err)
	}
	l = limitsOf(t, srv.wrote(), "POST /api/v1/me/runner-pools")
	if l.Interactive != nil || *l.Background.MemoryMiB != (types.RunnerPoolAmount{Default: 2048, Cap: 8192}) ||
		*l.Background.DiskMiB != (types.RunnerPoolAmount{Default: 20480, Cap: 51200}) || *l.MaxConcurrentRuns != 2 || len(l.Barriers) != 0 {
		t.Fatalf("narrowed limits = %+v", l)
	}

	if err := run("runner", "pool", "create", "--name", "General", "--scope", "organization", "--run-type", "background", "--barrier", "wall"); err != nil {
		t.Fatal(err)
	}
	if l := limitsOf(t, srv.wrote(), "POST /api/v1/runner-pools"); l.Background == nil || len(l.Barriers) != 1 || l.Barriers[0] != types.CC2 {
		t.Fatalf("organization pool limits = %+v", l)
	}

	for _, args := range [][]string{
		{"runner", "pool", "update", "Laptop", "--max-runs", "6", "--barrier", "fence", "--barrier", "wall", "--barrier", "vault"},
		{"runner", "pool", "update", "Laptop", "--max-runs", "6"},
		{"runner", "pool", "update", "Laptop", "--background-memory", "2:64"},
	} {
		if err := run(args...); err != nil {
			t.Fatalf("%v = %v", args, err)
		}
		if w := srv.wrote(); w["PUT /api/v1/me/runner-pools/"+pool.ID.String()] == "" {
			t.Fatalf("%v wrote %v", args, w)
		}
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"runner", "pool", "create", "--name", "Tiny", "--run-type", "background", "--background-memory", "8:4"}, "--background-memory: the default (8) can't be more than the maximum (4)"},
		{[]string{"runner", "pool", "create", "--name", "Tiny", "--limits-file", "t.json", "--barrier", "wall"}, "--limits-file replaces the limit flags; use one or the other"},
		{[]string{"runner", "pool", "update", "Laptop"}, "name something to change: --name, --switch, --max-runs or a limit flag"},
	} {
		if err := run(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v = %v, want %q", tc.args, err, tc.want)
		}
		if w := srv.wrote(); len(w) != 0 {
			t.Errorf("%v wrote %v before refusing", tc.args, w)
		}
	}
}
