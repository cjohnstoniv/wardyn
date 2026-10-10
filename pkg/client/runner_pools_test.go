// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestCreateRunRequest_RunnerPoolIDIsOmittedUntilChosen(t *testing.T) {
	b, _ := json.Marshal(client.CreateRunRequest{Agent: "claude-code"})
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if _, present := raw["runner_pool_id"]; present {
		t.Fatal("a request naming no pool posts runner_pool_id: an unset pool must inherit the default, not name an empty one")
	}
	b, _ = json.Marshal(client.CreateRunRequest{Agent: "claude-code", RunnerPoolID: "p"})
	if err := json.Unmarshal(b, &raw); err != nil || raw["runner_pool_id"] != "p" {
		t.Fatalf("runner_pool_id = %v", raw["runner_pool_id"])
	}
}

func TestUpdateRunnerPoolRequestNeverDeletes(t *testing.T) {
	sw := func(v client.RunnerPoolSwitch) *client.RunnerPoolSwitch { return &v }
	name, bad := "Build farm", " x"
	for _, tc := range []struct {
		name string
		r    client.UpdateRunnerPoolRequest
		ok   bool
	}{
		{"rename", client.UpdateRunnerPoolRequest{Revision: 2, Name: &name}, true},
		{"disable", client.UpdateRunnerPoolRequest{Revision: 2, State: sw("disabled")}, true},
		{"enable", client.UpdateRunnerPoolRequest{Revision: 2, State: sw("active")}, true},
		{"delete is the DELETE route", client.UpdateRunnerPoolRequest{Revision: 2, State: sw("deleted")}, false},
		{"unknown state", client.UpdateRunnerPoolRequest{Revision: 2, State: sw("paused")}, false},
		{"no change", client.UpdateRunnerPoolRequest{Revision: 2}, false},
		{"no revision", client.UpdateRunnerPoolRequest{Name: &name}, false},
		{"bad name", client.UpdateRunnerPoolRequest{Revision: 2, Name: &bad}, false},
	} {
		if err := tc.r.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestPreflightResultDecodesThePoolFacts(t *testing.T) {
	var res client.PreflightResult
	body := `{"setup_items":[],"provenance":[],"resources":[],"local_placement":[],"allowed_images":[],
	  "runner_pool":{"id":"11111111-1111-1111-1111-111111111111","name":"Build farm","hosting_type":"remote_provided","revision":4,"selection":"organisation_default"},
	  "runner_pools":[{"id":"11111111-1111-1111-1111-111111111111","name":"Build farm","hosting_type":"remote_provided","availability":"unavailable","reason":"runner_pool_unavailable"}]}`
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	if res.RunnerPool == nil || res.RunnerPool.Selection != client.RunnerPoolSelectedOrg || res.RunnerPool.HostingType != client.RunnerPoolRemoteProvided || res.RunnerPool.Revision != 4 {
		t.Fatalf("runner_pool = %+v", res.RunnerPool)
	}
	if len(res.RunnerPools) != 1 || res.RunnerPools[0].Availability != client.RunnerPoolUnavailable || res.RunnerPools[0].Reason != runnerpool.ReasonUnavailable {
		t.Fatalf("runner_pools = %+v", res.RunnerPools)
	}
}

// TestRunnerPoolMethodsHitTheirRoutes pins every SDK method to the method and
// path the server mounts it on (internal/api's mountRunnerPoolRoutes), and
// that a server's refusal reaches the caller as its reason.
func TestRunnerPoolMethodsHitTheirRoutes(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"x","reason":"runner_pools_unavailable"}`))
	}))
	defer srv.Close()
	c := client.New(srv.URL, "t")
	ctx := context.Background()
	pool, runner := uuid.New(), uuid.New()
	name := "n"
	for _, tc := range []struct {
		method, path string
		call         func() error
	}{
		{"GET", "/api/v1/runner-pools", func() error { _, err := c.ListRunnerPools(ctx); return err }},
		{"GET", "/api/v1/runner-pools/" + pool.String(), func() error { _, err := c.GetRunnerPool(ctx, pool); return err }},
		{"POST", "/api/v1/runner-pools", func() error {
			_, err := c.CreateRunnerPool(ctx, client.CreateRunnerPoolRequest{Name: name, HostingType: client.RunnerPoolSelfHosted})
			return err
		}},
		{"PUT", "/api/v1/runner-pools/" + pool.String(), func() error {
			_, err := c.UpdateRunnerPool(ctx, pool, client.UpdateRunnerPoolRequest{Revision: 1, Name: &name})
			return err
		}},
		{"DELETE", "/api/v1/runner-pools/" + pool.String(), func() error { return c.DeleteRunnerPool(ctx, pool) }},
		{"PUT", "/api/v1/me/runner-pools/" + pool.String() + "/runners/" + runner.String(), func() error { _, err := c.AddMyRunnerToPool(ctx, pool, runner); return err }},
		{"DELETE", "/api/v1/me/runner-pools/" + pool.String() + "/runners/" + runner.String(), func() error { return c.RemoveMyRunnerFromPool(ctx, pool, runner) }},
		{"GET", "/api/v1/runner-pool-defaults", func() error { _, err := c.GetOrgRunnerPoolDefaults(ctx); return err }},
		{"PUT", "/api/v1/runner-pool-defaults", func() error { _, err := c.SetOrgRunnerPoolDefaults(ctx, client.RunnerPoolDefaults{}); return err }},
		{"GET", "/api/v1/me/runner-pool-defaults", func() error { _, err := c.GetMyRunnerPoolDefaults(ctx); return err }},
		{"PUT", "/api/v1/me/runner-pool-defaults", func() error { _, err := c.SetMyRunnerPoolDefaults(ctx, client.RunnerPoolDefaults{}); return err }},
		{"DELETE", "/api/v1/me/runner-pool-defaults", func() error { return c.ClearMyRunnerPoolDefaults(ctx) }},
	} {
		err := tc.call()
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Reason != string(runnerpool.ReasonPoolsUnavailable) {
			t.Errorf("%s %s: err = %v, want the server's reason", tc.method, tc.path, err)
		}
		if gotMethod != tc.method || gotPath != tc.path {
			t.Errorf("sent %s %s, want %s %s", gotMethod, gotPath, tc.method, tc.path)
		}
	}
}
