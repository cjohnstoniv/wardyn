// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestWorkloadListAndGet: the table says each workload's state, generation and why its last
// launch was refused; --owner and the page flags reach the query; get prints the stored workload.
func TestWorkloadListAndGet(t *testing.T) {
	runID := uuid.New()
	rows := []sdk.Workload{
		{Name: "web", Owner: "person:me", Source: sdk.WorkloadSourceAPI, State: "RUNNING", Generation: 3, CurrentGeneration: 3, RunID: &runID},
		{Name: "worker", Owner: "service:ci", Source: sdk.WorkloadSourceK8s, Generation: 4, CurrentGeneration: 3,
			LastRefusal: &sdk.WorkloadRefusal{Generation: 4, Reason: "egress_ceiling", At: time.Now()}},
	}
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workloads":
			query = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(rows)
		case "/api/v1/workloads/web":
			_ = json.NewEncoder(w).Encode(rows[0])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rig := &routeRig{Server: srv}

	out, err := rig.run(t, "workload", "list", "--owner", "service:ci", "--limit", "5", "--offset", "10")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "GENERATION", "web", "person:me", "RUNNING", "3/3", runID.String(), "worker", "k8s", "3/4", "egress_ceiling"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"owner=service%3Aci", "limit=5", "offset=10"} {
		if !strings.Contains(query, want) {
			t.Errorf("query = %q, want %s", query, want)
		}
	}

	out, err = rig.run(t, "workload", "list", "--json")
	var got []sdk.Workload
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || len(got) != 2 {
		t.Errorf("list --json: err = %v, output = %s", err, out)
	}

	out, err = rig.run(t, "workload", "get", "web")
	var one sdk.Workload
	if err != nil || json.Unmarshal([]byte(out), &one) != nil || one.Name != "web" || one.Generation != 3 {
		t.Errorf("get: err = %v, output = %s", err, out)
	}
}

// TestWorkloadRestartAndDelete: restart answers like an apply, a refusal is exit 1, and the owner
// flag names whose workload; delete says what it ended.
func TestWorkloadRestartAndDelete(t *testing.T) {
	runID := uuid.New()
	var restartOwner, deleteOwner string
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/workloads/web/restart":
			restartOwner = r.URL.Query().Get("owner")
			launch := sdk.WorkloadLaunch{State: sdk.WorkloadLaunched, RunID: &runID}
			if refuse {
				launch = sdk.WorkloadLaunch{State: sdk.WorkloadRefused, Reason: "pool_full", Message: "the pool is at its run limit"}
			}
			_ = json.NewEncoder(w).Encode(sdk.WorkloadApplyResult{Outcome: sdk.WorkloadUnchanged, Launch: launch})
		case "DELETE /api/v1/workloads/web":
			deleteOwner = r.URL.Query().Get("owner")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rig := &routeRig{Server: srv}

	out, err := rig.run(t, "workload", "restart", "web", "--owner", "service:ci")
	if err != nil || out != "workload/web restarted run "+runID.String()+"\n" || restartOwner != "service:ci" {
		t.Errorf("restart: err = %v, output = %q, owner = %q", err, out, restartOwner)
	}
	refuse = true
	out, err = rig.run(t, "workload", "restart", "web")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 || out != "workload/web refused pool_full: the pool is at its run limit\n" {
		t.Errorf("refused restart: err = %v, output = %q", err, out)
	}

	out, err = rig.run(t, "workload", "delete", "web", "--owner", "person:gina")
	if err != nil || out != "workload/web deleted\n" || deleteOwner != "person:gina" {
		t.Errorf("delete: err = %v, output = %q, owner = %q", err, out, deleteOwner)
	}
}

// TestWorkloadBindingCommands: create sends the three fields and needs all of them, list prints the
// table, delete takes an id and refuses text that is not one before any request.
func TestWorkloadBindingCommands(t *testing.T) {
	binding := sdk.WorkloadBinding{ID: uuid.New(), PoolID: uuid.New(), Namespace: "team-a", Service: "deployer", Sponsor: "person:gina", CreatedAt: time.Now()}
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"POST /api/v1/workload-bindings":                          replyJSON(http.StatusCreated, binding),
		"GET /api/v1/workload-bindings":                           replyJSON(http.StatusOK, []sdk.WorkloadBinding{binding}),
		"DELETE /api/v1/workload-bindings/" + binding.ID.String(): replyJSON(http.StatusNoContent, nil),
	})

	out, err := rig.run(t, "workload", "binding", "create", "--pool", "prod-eu", "--namespace", "team-a", "--service", "deployer")
	if err != nil || !strings.Contains(out, "team-a") || !strings.Contains(out, binding.ID.String()) {
		t.Errorf("create: err = %v, output = %q", err, out)
	}
	var req sdk.CreateWorkloadBindingRequest
	if got := rig.sent("POST /api/v1/workload-bindings"); len(got) != 1 || json.Unmarshal([]byte(got[0]), &req) != nil ||
		req != (sdk.CreateWorkloadBindingRequest{Pool: "prod-eu", Namespace: "team-a", Service: "deployer"}) {
		t.Errorf("create body = %q", got)
	}
	if _, err := rig.run(t, "workload", "binding", "create", "--pool", "prod-eu"); err == nil || !strings.Contains(err.Error(), "required flag") {
		t.Errorf("create without --namespace and --service = %v", err)
	}

	out, err = rig.run(t, "workload", "binding", "list")
	if err != nil || !strings.Contains(out, "person:gina") || !strings.Contains(out, "deployer") {
		t.Errorf("list: err = %v, output = %q", err, out)
	}

	out, err = rig.run(t, "workload", "binding", "delete", binding.ID.String())
	if err != nil || out != "workload binding "+binding.ID.String()+" deleted\n" {
		t.Errorf("delete: err = %v, output = %q", err, out)
	}
	if _, err := rig.run(t, "workload", "binding", "delete", "not-an-id"); err == nil || !strings.Contains(err.Error(), "invalid workload binding id") {
		t.Errorf("delete of a non-id = %v", err)
	}
	if n := len(rig.sent("DELETE /api/v1/workload-bindings/not-an-id")); n != 0 {
		t.Errorf("a non-id was sent (%d)", n)
	}
}
