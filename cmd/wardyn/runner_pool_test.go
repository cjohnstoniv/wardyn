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
	remote, self := uuid.New(), uuid.New()
	srv := newCmdServer(t, http.StatusOK, sdk.RunnerPoolDefaults{})
	base := []string{"--url", srv.URL, "--token", "t"}
	if err := execCmd(t, append([]string{"runner", "pool", "default", "set", "--hosting", "self_hosted", "--remote-pool", remote.String(), "--self-hosted-pool", self.String()}, base...)...); err != nil {
		t.Fatal(err)
	}
	req := srv.last()
	var got sdk.RunnerPoolDefaults
	if err := json.Unmarshal(req.body, &got); err != nil || req.method != http.MethodPut || req.path != "/api/v1/me/runner-pool-defaults" ||
		got.PreferredHosting != sdk.RunnerPoolSelfHosted || *got.RemoteProvided != remote || *got.SelfHosted != self {
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
		{"set", "--hosting", "cloud"},
		{"set", "--remote-pool", "nope"},
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
		RunnerPoolChoice: sdk.RunnerPoolChoice{Name: "Home server", HostingType: sdk.RunnerPoolSelfHosted, Availability: sdk.RunnerPoolUnavailable, Reason: "runner_pool_no_online_runner"},
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

// create makes the caller's own Self-Hosted pool unless --remote asks an admin route for a Remote
// Provided one; update and delete read the pool first and take the route its hosting type owns.
func TestRunnerPoolManageCommandsTakeTheOwnersRoute(t *testing.T) {
	id := uuid.New()
	for _, c := range []struct {
		hosting sdk.RunnerPoolHosting
		write   string
	}{{sdk.RunnerPoolSelfHosted, "/api/v1/me/runner-pools/"}, {sdk.RunnerPoolRemoteProvided, "/api/v1/runner-pools/"}} {
		srv := newCmdServer(t, http.StatusOK, sdk.RunnerPool{ID: id, Name: "Laptop", HostingType: c.hosting, Revision: 3})
		base := []string{"--url", srv.URL, "--token", "t"}
		if err := execCmd(t, append([]string{"runner", "pool", "update", id.String(), "--switch", "off"}, base...)...); err != nil {
			t.Fatal(err)
		}
		get, put := srv.reqs[len(srv.reqs)-2], srv.last()
		var body sdk.UpdateRunnerPoolRequest
		if err := json.Unmarshal(put.body, &body); err != nil || get.method != http.MethodGet || put.method != http.MethodPut ||
			put.path != c.write+id.String() || body.Revision != 3 || body.State == nil || *body.State != sdk.RunnerPoolSwitchDisabled {
			t.Errorf("%s update sent %s %s then %s %s %s", c.hosting, get.method, get.path, put.method, put.path, put.body)
		}
		if err := execCmd(t, append([]string{"runner", "pool", "delete", id.String()}, base...)...); err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Errorf("delete without --yes = %v", err)
		}
		if err := execCmd(t, append([]string{"runner", "pool", "delete", id.String(), "--yes"}, base...)...); err != nil {
			t.Fatal(err)
		}
		if r := srv.last(); r.method != http.MethodDelete || r.path != c.write+id.String() {
			t.Errorf("%s delete sent %s %s", c.hosting, r.method, r.path)
		}
	}
	srv := newCmdServer(t, http.StatusCreated, sdk.RunnerPool{ID: id, Name: "Laptop", HostingType: sdk.RunnerPoolSelfHosted})
	base := []string{"--url", srv.URL, "--token", "t"}
	for args, want := range map[string]string{"--name Laptop": "/api/v1/me/runner-pools", "--name Provided --remote": "/api/v1/runner-pools"} {
		if err := execCmd(t, append(append([]string{"runner", "pool", "create"}, strings.Fields(args)...), base...)...); err != nil {
			t.Fatal(err)
		}
		if r := srv.last(); r.method != http.MethodPost || r.path != want {
			t.Errorf("create %s sent %s %s", args, r.method, r.path)
		}
	}
	for _, r := range srv.reqs {
		var mine sdk.CreateMyRunnerPoolRequest
		if r.path == "/api/v1/me/runner-pools" && (json.Unmarshal(r.body, &mine) != nil || mine.Name != "Laptop" || strings.Contains(string(r.body), "hosting_type")) {
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

func TestRunnerPoolOrgDefaultSetKeepsThePoolsAndSetsTheBarrier(t *testing.T) {
	remote := uuid.New()
	srv := newCmdServer(t, http.StatusOK, sdk.RunnerPoolDefaults{RemoteProvided: &remote})
	base := []string{"--url", srv.URL, "--token", "t"}
	if err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", "--barrier", "CC2"}, base...)...); err != nil {
		t.Fatal(err)
	}
	req := srv.last()
	var got sdk.RunnerPoolDefaults
	if err := json.Unmarshal(req.body, &got); err != nil || req.method != http.MethodPut || req.path != "/api/v1/runner-pool-defaults" ||
		got.Barrier != sdk.CC2 || got.RemoteProvided == nil || *got.RemoteProvided != remote {
		t.Fatalf("set sent %s %s %s", req.method, req.path, req.body)
	}
	if err := execCmd(t, append([]string{"runner", "pool", "org-default", "set", "--barrier", "none"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if body := srv.last().body; strings.Contains(string(body), "barrier") {
		t.Errorf("none sent %s, want no barrier", body)
	}
	n := len(srv.reqs)
	for _, bad := range [][]string{{"set"}, {"set", "--barrier", "CC9"}} {
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
