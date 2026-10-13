// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestRunPoolFlagIsSentAsRunnerPoolID(t *testing.T) {
	pool := uuid.New()
	for _, args := range [][]string{
		{"run", "--agent", "claude-code", "--pool", pool.String()},
		{"run", "--agent", "claude-code", "--pool", pool.String(), "--dry-run"},
	} {
		srv := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": "x", "reason": "request_field_unavailable"})
		if err := execCmd(t, append(args, "--url", srv.URL, "--token", "t")...); err == nil {
			t.Fatalf("%v: the server's refusal was swallowed", args)
		}
		var body sdk.CreateRunRequest
		if err := json.Unmarshal(srv.last().body, &body); err != nil || body.RunnerPoolID != pool.String() {
			t.Fatalf("%v: body = %s, want runner_pool_id %s", args, srv.last().body, pool)
		}
	}
	srv := newCmdServer(t, http.StatusOK, map[string]string{})
	if err := execCmd(t, "run", "--agent", "claude-code", "--pool", "build-farm", "--url", srv.URL, "--token", "t"); err == nil || !strings.Contains(err.Error(), "--pool") {
		t.Fatalf("a malformed --pool = %v, want an error naming the flag, before any request", err)
	}
	if len(srv.reqs) != 0 {
		t.Fatal("a malformed --pool reached the server")
	}
}

func TestRunnerPoolDefaultCommands(t *testing.T) {
	pool := uuid.New()
	srv := newCmdServer(t, http.StatusOK, sdk.RunnerPoolDefaults{})
	base := []string{"--url", srv.URL, "--token", "t"}
	if err := execCmd(t, append([]string{"runner", "pool", "default", "set", pool.String()}, base...)...); err != nil {
		t.Fatal(err)
	}
	req := srv.last()
	if req.method != http.MethodPut || req.path != "/api/v1/me/runner-pool-defaults" || string(req.body) != `{"pool_id":"`+pool.String()+`"}` {
		t.Fatalf("set sent %s %s %s", req.method, req.path, req.body)
	}
	for args, want := range map[string][2]string{
		"get":   {http.MethodGet, "/api/v1/me/runner-pool-defaults"},
		"clear": {http.MethodDelete, "/api/v1/me/runner-pool-defaults"},
	} {
		if err := execCmd(t, append([]string{"runner", "pool", "default", args}, base...)...); err != nil {
			t.Fatal(err)
		}
		if r := srv.last(); r.method != want[0] || r.path != want[1] {
			t.Errorf("%s sent %s %s", args, r.method, r.path)
		}
	}
	n := len(srv.reqs)
	for _, bad := range [][]string{
		{"set"},
		{"set", "--hosting", "self_hosted", pool.String()},
	} {
		if err := execCmd(t, append(append([]string{"runner", "pool", "default"}, bad...), base...)...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(srv.reqs) != n {
		t.Fatal("an invalid default reached the server")
	}
}

func TestRunnerPoolListSurfacesTheServersRefusal(t *testing.T) {
	srv := newCmdServer(t, http.StatusNotImplemented, map[string]string{"error": "This server does not manage runner pools yet.", "reason": "runner_pools_unavailable"})
	err := execCmd(t, "runner", "pool", "list", "--url", srv.URL, "--token", "t")
	if err == nil || !strings.Contains(err.Error(), "runner pools") {
		t.Fatalf("err = %v", err)
	}
}

// The list prints each pool's availability reason and each barrier's reason, the default marked.
func TestRunnerPoolListPrintsTheReasons(t *testing.T) {
	list := sdk.RunnerPoolList{Pools: []sdk.RunnerPoolCatalogueEntry{{
		RunnerPoolChoice: sdk.RunnerPoolChoice{Name: "Home server", Scope: sdk.RunnerPoolUser, Availability: sdk.RunnerPoolUnavailable, Reason: "runner_pool_no_online_runner"},
		BarrierOptions: []sdk.BarrierOption{
			{Class: "CC1", ReasonCode: "below_floor", Floor: "CC2"},
			{Class: "CC2", Available: true, Default: true},
			{Class: "CC3", ReasonCode: "not_offered", ReasonDetail: "kvm_missing"},
		},
	}}}
	var out strings.Builder
	if err := printRunnerPools(&out, list); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"unavailable (runner_pool_no_online_runner)", "CC1 (below_floor) CC2* CC3 (not_offered: kvm_missing)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// create makes the caller's own user pool unless --org asks an admin route for an organization pool;
// update and delete read the pool first and take the route its scope owns.
func TestRunnerPoolManageCommandsTakeTheOwnersRoute(t *testing.T) {
	id := uuid.New()
	for _, c := range []struct {
		scope sdk.RunnerPoolScope
		write string
	}{{sdk.RunnerPoolUser, "/api/v1/me/runner-pools/"}, {sdk.RunnerPoolOrganization, "/api/v1/runner-pools/"}} {
		srv := newCmdServer(t, http.StatusOK, sdk.RunnerPool{ID: id, Name: "Laptop", Scope: c.scope, Revision: 3})
		base := []string{"--url", srv.URL, "--token", "t"}
		if err := execCmd(t, append([]string{"runner", "pool", "update", id.String(), "--switch", "off"}, base...)...); err != nil {
			t.Fatal(err)
		}
		get, put := srv.reqs[len(srv.reqs)-2], srv.last()
		var body sdk.UpdateRunnerPoolRequest
		if err := json.Unmarshal(put.body, &body); err != nil || get.method != http.MethodGet || put.method != http.MethodPut ||
			put.path != c.write+id.String() || body.Revision != 3 || body.State == nil || *body.State != sdk.RunnerPoolSwitchDisabled {
			t.Errorf("%s update sent %s %s then %s %s %s", c.scope, get.method, get.path, put.method, put.path, put.body)
		}
		if err := execCmd(t, append([]string{"runner", "pool", "delete", id.String()}, base...)...); err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Errorf("delete without --yes = %v", err)
		}
		if err := execCmd(t, append([]string{"runner", "pool", "delete", id.String(), "--yes"}, base...)...); err != nil {
			t.Fatal(err)
		}
		if r := srv.last(); r.method != http.MethodDelete || r.path != c.write+id.String() {
			t.Errorf("%s delete sent %s %s", c.scope, r.method, r.path)
		}
	}
	srv := newCmdServer(t, http.StatusCreated, sdk.RunnerPool{ID: id, Name: "Laptop", Scope: sdk.RunnerPoolUser})
	base := []string{"--url", srv.URL, "--token", "t"}
	for args, want := range map[string]string{"--name Laptop": "/api/v1/me/runner-pools", "--name Builds --scope organization": "/api/v1/runner-pools"} {
		if err := execCmd(t, append(append([]string{"runner", "pool", "create"}, strings.Fields(args)...), base...)...); err != nil {
			t.Fatal(err)
		}
		if r := srv.last(); r.method != http.MethodPost || r.path != want {
			t.Errorf("create %s sent %s %s", args, r.method, r.path)
		}
	}
	for _, r := range srv.reqs {
		var mine sdk.CreateMyRunnerPoolRequest
		if r.path == "/api/v1/me/runner-pools" && (json.Unmarshal(r.body, &mine) != nil || mine.Name != "Laptop" || strings.Contains(string(r.body), "scope")) {
			t.Errorf("own create body = %s", r.body)
		}
	}
	n := len(srv.reqs)
	for _, bad := range [][]string{{"create"}, {"create", "--name", "x", "--scope", "shared"}, {"create", "--name", "x", "--org"}, {"update", id.String()}, {"update", id.String(), "--switch", "maybe"}} {
		if err := execCmd(t, append(append([]string{"runner", "pool"}, bad...), base...)...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(srv.reqs) != n {
		t.Error("an invalid command reached the server")
	}
}

func TestRunnerRemoteCommands(t *testing.T) {
	view := sdk.RemoteRunnerView{RemoteRunner: sdk.RemoteRunner{ID: "docker", Name: "Docker host", Revision: 4, PeoplePools: sdk.PeoplePoolsOff}, Configured: true}
	srv := newCmdServer(t, http.StatusOK, view)
	base := []string{"--url", srv.URL, "--token", "t"}
	for _, c := range []struct {
		args         []string
		method, path string
		body         string
	}{
		{[]string{"show", "docker"}, http.MethodGet, "/api/v1/remote-runners/docker", ""},
		{[]string{"set", "docker", "--name", "Build host"}, http.MethodPut, "/api/v1/remote-runners/docker", `{"revision":4,"name":"Build host"}`},
		{[]string{"people", "set", "docker", "--everyone"}, http.MethodPut, "/api/v1/remote-runners/docker/people-policy", `{"people_pools":"everyone"}`},
		{[]string{"people", "set", "docker", "--group", "eng", "--user", "alice"}, http.MethodPut, "/api/v1/remote-runners/docker/people-policy",
			`{"people_pools":"listed","subjects":[{"subject_type":"user","subject":"alice"},{"subject_type":"group","subject":"eng"}]}`},
		{[]string{"people", "clear", "docker"}, http.MethodDelete, "/api/v1/remote-runners/docker/people-policy", ""},
	} {
		if err := execCmd(t, append(append([]string{"runner", "remote"}, c.args...), base...)...); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if r := srv.last(); r.method != c.method || r.path != c.path || (c.body != "" && string(r.body) != c.body) {
			t.Errorf("%v sent %s %s %s", c.args, r.method, r.path, r.body)
		}
	}
	list := newCmdServer(t, http.StatusOK, []sdk.RemoteRunnerView{view})
	for flag, path := range map[string]string{"": "/api/v1/remote-runners", "--mine": "/api/v1/me/remote-runners"} {
		args := []string{"runner", "remote", "list", "--url", list.URL, "--token", "t"}
		if flag != "" {
			args = append(args, flag)
		}
		if err := execCmd(t, args...); err != nil {
			t.Fatal(err)
		}
		if r := list.last(); r.method != http.MethodGet || r.path != path {
			t.Errorf("list %s sent %s %s", flag, r.method, r.path)
		}
	}
	n := len(srv.reqs)
	for _, bad := range [][]string{{"people", "set", "docker"}, {"people", "set", "docker", "--everyone", "--user", "a"}, {"set", "docker"}} {
		if err := execCmd(t, append(append([]string{"runner", "remote"}, bad...), base...)...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(srv.reqs) != n {
		t.Error("an invalid command reached the server")
	}
}

// poolRoutes is a test server that answers each "METHOD /path" it knows with its body (and 404 for any
// other), recording every request.
type poolRoutes struct {
	*cmdServer
}

func newPoolRoutes(t *testing.T, routes map[string]any) poolRoutes {
	t.Helper()
	cs := &cmdServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.reqs = append(cs.reqs, recordedReq{method: r.Method, path: r.URL.Path, body: body})
		cs.mu.Unlock()
		resp, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			resp, ok = routes[r.Method+" *"]
		}
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found","reason":"runner_pool_not_found"}`))
			return
		}
		if resp == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(cs.Close)
	return poolRoutes{cs}
}

func (p poolRoutes) sent(method, path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.reqs {
		if r.method == method && r.path == path {
			return true
		}
	}
	return false
}

// add and remove take a pool by id or name and runners by id or name: your own self-hosted runner goes
// through your own route, and a remote-hosted runner through the organization's route for an
// organization pool and through your own for your user pool. A name nothing answers to goes on as a
// remote-hosted runner's id, and the server's sentence is printed.
func TestRunnerPoolAddAndRemoveResolveRunnersByName(t *testing.T) {
	laptop, mine := uuid.New(), uuid.New()
	for _, c := range []struct {
		scope  sdk.RunnerPoolScope
		prefix string
		remote string
	}{{sdk.RunnerPoolUser, "/api/v1/me/runner-pools/", "GET /api/v1/me/remote-runners"}, {sdk.RunnerPoolOrganization, "/api/v1/runner-pools/", "GET /api/v1/remote-runners"}} {
		pool := sdk.RunnerPool{ID: laptop, Name: "Laptop", Scope: c.scope, Revision: 1}
		srv := newPoolRoutes(t, map[string]any{
			"GET /api/v1/runner-pools":                    sdk.RunnerPoolList{Pools: []sdk.RunnerPoolCatalogueEntry{{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: laptop, Name: "Laptop", Scope: c.scope}}}},
			"GET /api/v1/runner-pools/" + laptop.String(): pool,
			"GET /api/v1/me/runners":                      []sdk.RunnerView{{ID: mine, Name: "laptop-7"}},
			c.remote:                                      []sdk.RemoteRunnerView{{RemoteRunner: sdk.RemoteRunner{ID: "docker", Name: "shared-docker"}}},
			"PUT *":                                       sdk.RunnerPoolMember{PoolID: laptop},
			"DELETE *":                                    nil,
		})
		base := []string{"--url", srv.URL, "--token", "t"}
		out, err := execCmdCapture(t, append([]string{"runner", "pool", "add", "Laptop", "laptop-7", "shared-docker"}, base...)...)
		if err != nil {
			t.Fatalf("%s add: %v", c.scope, err)
		}
		if !srv.sent(http.MethodPut, "/api/v1/me/runner-pools/"+laptop.String()+"/runners/"+mine.String()) ||
			!srv.sent(http.MethodPut, c.prefix+laptop.String()+"/remote-runners/docker") || !strings.Contains(out, "added to Laptop") {
			t.Errorf("%s add sent %+v and printed %q", c.scope, srv.reqs, out)
		}
		if _, err := execCmdCapture(t, append([]string{"runner", "pool", "remove", laptop.String(), "shared-docker"}, base...)...); err != nil ||
			!srv.sent(http.MethodDelete, c.prefix+laptop.String()+"/remote-runners/docker") {
			t.Errorf("%s remove: %v", c.scope, err)
		}
	}
	if err := execCmd(t, "runner", "pool", "add", "Laptop", "--url", "http://127.0.0.1:1", "--token", "t"); err == nil {
		t.Error("add with no runner was accepted")
	}
	sentence := "eu-cluster is not available to people's pools. Ask your admin to make it available."
	srv := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": sentence, "reason": "runner_pool_member_mismatch"})
	if err := execCmd(t, "runner", "pool", "add", laptop.String(), "eu-cluster", "--url", srv.URL, "--token", "t"); err == nil || !strings.Contains(err.Error(), sentence) {
		t.Errorf("a refused add = %v, want the server's sentence", err)
	}
}

// create --scope organization --runner makes the organization's pool and then puts each runner in it.
func TestRunnerPoolCreateWithRunners(t *testing.T) {
	id := uuid.New()
	srv := newPoolRoutes(t, map[string]any{
		"POST /api/v1/runner-pools":                                       sdk.RunnerPool{ID: id, Name: "General", Scope: sdk.RunnerPoolOrganization, Revision: 1},
		"GET /api/v1/remote-runners":                                      []sdk.RemoteRunnerView{{RemoteRunner: sdk.RemoteRunner{ID: "k8s", Name: "eu-cluster"}}},
		"PUT /api/v1/runner-pools/" + id.String() + "/remote-runners/k8s": sdk.RunnerPoolMember{PoolID: id},
	})
	out, err := execCmdCapture(t, "runner", "pool", "create", "--name", "General", "--scope", "organization", "--runner", "eu-cluster", "--url", srv.URL, "--token", "t")
	if err != nil || !srv.sent(http.MethodPut, "/api/v1/runner-pools/"+id.String()+"/remote-runners/k8s") || !strings.Contains(out, "eu-cluster added to General") {
		t.Fatalf("create --runner = %v, sent %+v, printed %q", err, srv.reqs, out)
	}
}

// org-default set --pool takes an organization pool by name and keeps the barrier; a user pool is
// refused before any write, in the canon's words; none and clear take the default pool away.
func TestRunnerPoolOrgDefaultSetByPool(t *testing.T) {
	org, user, keep := uuid.New(), uuid.New(), uuid.New()
	srv := newPoolRoutes(t, map[string]any{
		"GET /api/v1/runner-pool-defaults": sdk.RunnerPoolDefaults{PoolID: &keep, Barrier: sdk.CC2},
		"PUT /api/v1/runner-pool-defaults": sdk.RunnerPoolDefaults{},
		"GET /api/v1/runner-pools": sdk.RunnerPoolList{Pools: []sdk.RunnerPoolCatalogueEntry{
			{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: org, Name: "CI jobs", Scope: sdk.RunnerPoolOrganization}},
			{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: user, Name: "Laptop", Scope: sdk.RunnerPoolUser}}}},
		"GET /api/v1/runner-pools/" + org.String():  sdk.RunnerPool{ID: org, Name: "CI jobs", Scope: sdk.RunnerPoolOrganization},
		"GET /api/v1/runner-pools/" + user.String(): sdk.RunnerPool{ID: user, Name: "Laptop", Scope: sdk.RunnerPoolUser},
	})
	base := []string{"--url", srv.URL, "--token", "t"}
	if err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", "--pool", "CI jobs"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if body := string(srv.last().body); body != `{"pool_id":"`+org.String()+`","barrier":"CC2"}` {
		t.Errorf("set --pool sent %s", body)
	}
	if err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", "--barrier", "CC1"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if body := string(srv.last().body); body != `{"pool_id":"`+keep.String()+`","barrier":"CC1"}` {
		t.Errorf("set --barrier sent %s", body)
	}
	for _, args := range [][]string{{"set", "--pool", "none"}, {"clear"}} {
		if err := execCmd(t, append(append([]string{"runner", "pool", "org-default"}, args...), base...)...); err != nil {
			t.Fatal(err)
		}
		if body := string(srv.last().body); body != `{"barrier":"CC2"}` {
			t.Errorf("%v sent %s, want the barrier kept and no pool", args, body)
		}
	}
	n := len(srv.reqs)
	err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", "--pool", "Laptop"}, base...)...)
	if err == nil || !strings.Contains(err.Error(), "there is no organization default for user pools; people choose their own") {
		t.Errorf("a user pool = %v", err)
	}
	for _, r := range srv.reqs[n:] {
		if r.method == http.MethodPut {
			t.Error("a user pool as the organization default reached a write")
		}
	}
	for _, bad := range [][]string{{"set"}, {"set", "--barrier", "CC9"}, {"set", "CI jobs"}} {
		if err := execCmd(t, append(append([]string{"runner", "pool", "org-default"}, bad...), base...)...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

// runner set --available-to-people turns a remote-hosted runner's people policy on (everyone) or off; a
// held write exits 0 saying so; --max-runs keeps working on your own runner.
func TestRunnerSetAvailableToPeople(t *testing.T) {
	srv := newPoolRoutes(t, map[string]any{
		"GET /api/v1/me/runners":                          []sdk.RunnerView{},
		"GET /api/v1/remote-runners":                      []sdk.RemoteRunnerView{{RemoteRunner: sdk.RemoteRunner{ID: "k8s", Name: "sandbox-cluster"}}},
		"PUT /api/v1/remote-runners/k8s/people-policy":    sdk.RemoteRunner{ID: "k8s"},
		"DELETE /api/v1/remote-runners/k8s/people-policy": nil,
	})
	base := []string{"--url", srv.URL, "--token", "t"}
	out, err := execCmdCapture(t, append([]string{"runner", "set", "sandbox-cluster", "--available-to-people", "on"}, base...)...)
	if err != nil || string(srv.last().body) != `{"people_pools":"everyone"}` || !strings.Contains(out, "sandbox-cluster is now available to people's pools") {
		t.Fatalf("on = %v, sent %s, printed %q", err, srv.last().body, out)
	}
	if _, err := execCmdCapture(t, append([]string{"runner", "set", "k8s", "--available-to-people", "off"}, base...)...); err != nil ||
		!srv.sent(http.MethodDelete, "/api/v1/remote-runners/k8s/people-policy") {
		t.Fatalf("off = %v", err)
	}
	n := len(srv.reqs)
	for _, bad := range [][]string{{"k8s"}, {"k8s", "--available-to-people", "maybe"}, {"k8s", "--available-to-people", "on", "--max-runs", "2"}} {
		if err := execCmd(t, append(append([]string{"runner", "set"}, bad...), base...)...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(srv.reqs) != n {
		t.Error("an invalid runner set reached the server")
	}
	held := newCmdServer(t, http.StatusAccepted, map[string]any{"pending_change": map[string]any{"id": uuid.NewString(), "state": "pending"}})
	out, err = execCmdCapture(t, "runner", "remote", "people", "set", "k8s", "--everyone", "--url", held.URL, "--token", "t")
	if err != nil || !strings.Contains(out, "pending approval: 1 change(s) stored") {
		t.Errorf("a held people policy = %v, printed %q, want exit 0 saying it waits", err, out)
	}
}
