// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

func poolLimits() *client.RunnerPoolLimits {
	return &client.RunnerPoolLimits{
		Background: &client.RunnerPoolBackgroundLimits{
			CPUMillis: client.RunnerPoolAmount{Default: 1000, Cap: 2000},
			MemoryMiB: client.RunnerPoolAmount{Default: 1024, Cap: 2048},
			Lifetime:  client.RunnerPoolDuration{Unlimited: true},
		},
		Barriers: []types.ConfinementClass{types.CC2},
	}
}

func TestPoolRequestsValidateTheirLimits(t *testing.T) {
	name := "Build farm"
	bad := poolLimits()
	bad.Barriers = nil
	for _, tc := range []struct {
		name string
		err  error
		ok   bool
	}{
		{"create without limits", client.CreateRunnerPoolRequest{Name: name, HostingType: client.RunnerPoolRemoteProvided}.Validate(), true},
		{"create with limits", client.CreateRunnerPoolRequest{Name: name, HostingType: client.RunnerPoolSelfHosted, Limits: poolLimits()}.Validate(), true},
		{"create with bad limits", client.CreateRunnerPoolRequest{Name: name, HostingType: client.RunnerPoolSelfHosted, Limits: bad}.Validate(), false},
		{"create with a bad hosting type", client.CreateRunnerPoolRequest{Name: name, HostingType: "cloud"}.Validate(), false},
		{"create with a bad name", client.CreateRunnerPoolRequest{Name: " x", HostingType: client.RunnerPoolSelfHosted}.Validate(), false},
		{"update only the limits", client.UpdateRunnerPoolRequest{Revision: 2, Limits: poolLimits()}.Validate(), true},
		{"update with bad limits", client.UpdateRunnerPoolRequest{Revision: 2, Limits: bad}.Validate(), false},
		{"update with a good name and bad limits", client.UpdateRunnerPoolRequest{Revision: 2, Name: &name, Limits: bad}.Validate(), false},
	} {
		if (tc.err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, tc.err, tc.ok)
		}
	}
}

func TestPoolChoiceCarriesLimitsAndProvenance(t *testing.T) {
	raw, err := json.Marshal(client.RunnerPoolChoice{Name: "P", Barriers: []types.ConfinementClass{types.CC1}, Limits: []client.EffectiveRunLimits{{
		RunType:   client.RunnerPoolRunBackground,
		CPUMillis: client.EffectiveLimit{Default: client.LimitAmount{Value: 2000}, Cap: client.LimitAmount{Value: 4000}, DefaultSource: client.LimitSourcePool, CapSource: client.LimitSourceGovernance},
		Lifetime:  client.EffectiveLimit{Default: client.LimitAmount{Unlimited: true}, Cap: client.LimitAmount{Unlimited: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"barriers":["CC1"]`, `"run_type":"background"`, `"cap_source":"governance"`, `"lifetime_sec":{"default":{"value":0,"unlimited":true}`, `"lifetime_span_sec"`, `"lifetime_lease_sec"`, `"no_end_allowed":false`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("wire %s lacks %s", raw, want)
		}
	}
	if strings.Contains(string(raw), "idle_sec") {
		t.Errorf("a Background entry carries no idle: %s", raw)
	}
	plain, _ := json.Marshal(client.RunnerPoolChoice{Name: "P"})
	if strings.Contains(string(plain), "limits") || strings.Contains(string(plain), "barriers") {
		t.Errorf("a pool with no limits sends none: %s", plain)
	}
}
