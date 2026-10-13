// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CreateRunStoresTheEndAndTheLifetimeCap creates runs through the real door
// and reads them back from Postgres: the end and the cap are counted from the stored
// created_at, an untouched Interactive run ends after 24 hours, and an older
// client's run keeps today's no end.
func TestPG_CreateRunStoresTheEndAndTheLifetimeCap(t *testing.T) {
	srv, pool := pgHarness(t)
	srv.cfg.AgentImages = composeAgentImages
	srv.cfg.RunMaxAge = 48 * time.Hour
	create := func(t *testing.T, body string) types.AgentRun {
		t.Helper()
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", w.Code, w.Body.String())
		}
		var created types.AgentRun
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		stored, err := store.NewPG(pool).GetRun(context.Background(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	within := func(got *time.Time, from time.Time, d time.Duration) bool {
		return got != nil && got.Sub(from.Add(d)).Abs() < time.Millisecond
	}

	t.Run("an ask past the deployment's age is cut to it", func(t *testing.T) {
		run := create(t, endBody("background", `"ends_after_sec":604800`))
		if !within(run.EndsAt, run.CreatedAt, 48*time.Hour) || !within(run.LifetimeCapAt, run.CreatedAt, 48*time.Hour) || run.LifetimeCapSource != types.LimitSourceDeployment {
			t.Errorf("ends %v, cap %v (%q), created %v; want both at created_at + 48h from the deployment", run.EndsAt, run.LifetimeCapAt, run.LifetimeCapSource, run.CreatedAt)
		}
	})
	t.Run("an untouched Interactive run ends after 24 hours", func(t *testing.T) {
		run := create(t, endBody("interactive", ""))
		if !within(run.EndsAt, run.CreatedAt, 24*time.Hour) || !within(run.LifetimeCapAt, run.CreatedAt, 48*time.Hour) {
			t.Errorf("ends %v, cap %v, created %v; want 24h and a 48h cap", run.EndsAt, run.LifetimeCapAt, run.CreatedAt)
		}
	})
	t.Run("an older client's run under no age cap keeps today's no end", func(t *testing.T) {
		srv.cfg.RunMaxAge = 0
		run := create(t, `{"agent":"claude-code","repo":"acme/widgets","task":"t"}`)
		if run.EndsAt != nil || run.LifetimeCapAt != nil || run.LifetimeCapSource != "" {
			t.Errorf("ends %v, cap %v (%q); want none", run.EndsAt, run.LifetimeCapAt, run.LifetimeCapSource)
		}
	})
}
