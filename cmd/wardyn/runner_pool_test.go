// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
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
		{"set", "nope"},
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
	for args, want := range map[string]string{"--name Laptop": "/api/v1/me/runner-pools", "--name Builds --org": "/api/v1/runner-pools"} {
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
	for _, bad := range [][]string{{"create"}, {"update", "not-an-id", "--name", "x"}, {"update", id.String()}, {"update", id.String(), "--switch", "maybe"}} {
		if err := execCmd(t, append(append([]string{"runner", "pool"}, bad...), base...)...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(srv.reqs) != n {
		t.Error("an invalid command reached the server")
	}
}

func TestRunnerPoolOrgDefaultSetKeepsThePoolAndSetsTheBarrier(t *testing.T) {
	remote := uuid.New()
	srv := newCmdServer(t, http.StatusOK, sdk.RunnerPoolDefaults{PoolID: &remote})
	base := []string{"--url", srv.URL, "--token", "t"}
	if err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", "--barrier", "CC2"}, base...)...); err != nil {
		t.Fatal(err)
	}
	req := srv.last()
	var got sdk.RunnerPoolDefaults
	if err := json.Unmarshal(req.body, &got); err != nil || req.method != http.MethodPut || req.path != "/api/v1/runner-pool-defaults" ||
		got.Barrier != sdk.CC2 || got.PoolID == nil || *got.PoolID != remote {
		t.Fatalf("set sent %s %s %s", req.method, req.path, req.body)
	}
	if err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", "--barrier", "none"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if body := srv.last().body; strings.Contains(string(body), "barrier") {
		t.Errorf("none sent %s, want no barrier", body)
	}
	other := uuid.New()
	if err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", other.String()}, base...)...); err != nil {
		t.Fatal(err)
	}
	if body := string(srv.last().body); body != `{"pool_id":"`+other.String()+`","barrier":"CC1"}` && !strings.Contains(body, `"pool_id":"`+other.String()+`"`) {
		t.Errorf("a pool sent %s", body)
	}
	if err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", "none"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if body := string(srv.last().body); strings.Contains(body, "pool_id") {
		t.Errorf("none sent %s, want no default pool", body)
	}
	n := len(srv.reqs)
	for _, bad := range [][]string{{"set"}, {"set", "--barrier", "CC9"}, {"set", "not-a-pool"}} {
		if err := execCmd(t, append(append([]string{"runner", "pool", "org-default"}, bad...), base...)...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	for _, r := range srv.reqs[n:] {
		if r.method == http.MethodPut {
			t.Fatal("an invalid barrier reached the server")
		}
	}
}

// add and remove send one of your runners to your own pool's member route and print the server's
// sentence when it refuses.
func TestRunnerPoolAddAndRemoveOwnRunner(t *testing.T) {
	pool, runnerID := uuid.New(), uuid.New()
	path := "/api/v1/me/runner-pools/" + pool.String() + "/runners/" + runnerID.String()
	srv := newCmdServer(t, http.StatusCreated, sdk.RunnerPoolMember{PoolID: pool, RunnerID: &runnerID})
	base := []string{"--url", srv.URL, "--token", "t"}
	for verb, method := range map[string]string{"add": http.MethodPut, "remove": http.MethodDelete} {
		if err := execCmd(t, append([]string{"runner", "pool", verb, pool.String(), runnerID.String()}, base...)...); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if r := srv.last(); r.method != method || r.path != path {
			t.Errorf("%s sent %s %s, want %s %s", verb, r.method, r.path, method, path)
		}
	}
	n := len(srv.reqs)
	for _, bad := range [][]string{{"add", pool.String()}, {"add", "not-a-pool", runnerID.String()}, {"remove", pool.String(), "not-a-runner"}} {
		if err := execCmd(t, append(append([]string{"runner", "pool"}, bad...), base...)...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(srv.reqs) != n {
		t.Error("an invalid command reached the server")
	}
	sentence := runnerID.String() + " is not one of your runners in Laptop."
	refusing := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": sentence, "reason": "runner_pool_member_mismatch"})
	err := execCmd(t, "runner", "pool", "add", pool.String(), runnerID.String(), "--url", refusing.URL, "--token", "t")
	if err == nil || !strings.Contains(err.Error(), sentence) {
		t.Errorf("a refused add = %v, want the server's sentence", err)
	}
}

// add and remove --remote read the pool and take the organisation's route for an organization pool and
// the caller's own for one of their user pools.
func TestRunnerPoolRemoteMemberTakesThePoolsRoute(t *testing.T) {
	id := uuid.New()
	for scope, prefix := range map[sdk.RunnerPoolScope]string{sdk.RunnerPoolOrganization: "/api/v1/runner-pools/", sdk.RunnerPoolUser: "/api/v1/me/runner-pools/"} {
		srv := newCmdServer(t, http.StatusOK, sdk.RunnerPool{ID: id, Name: "P", Scope: scope, Revision: 1})
		base := []string{"--url", srv.URL, "--token", "t"}
		for verb, method := range map[string]string{"add": http.MethodPut, "remove": http.MethodDelete} {
			if err := execCmd(t, append([]string{"runner", "pool", verb, id.String(), "--remote", "docker"}, base...)...); err != nil {
				t.Fatalf("%s %s: %v", scope, verb, err)
			}
			if r := srv.last(); r.method != method || r.path != prefix+id.String()+"/remote-runners/docker" {
				t.Errorf("%s %s sent %s %s", scope, verb, r.method, r.path)
			}
		}
		n := len(srv.reqs)
		for _, bad := range [][]string{{"add", id.String()}, {"add", id.String(), uuid.NewString(), "--remote", "docker"}} {
			if err := execCmd(t, append(append([]string{"runner", "pool"}, bad...), base...)...); err == nil {
				t.Errorf("%v was accepted", bad)
			}
		}
		if len(srv.reqs) != n {
			t.Error("an invalid command reached the server")
		}
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
