// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CreateRunStoresItsShape creates runs through the real door and reads
// them back from Postgres: a run keeps the shape it names, a run that names none
// keeps the one shape it fits, and a run that fits several, or an older
// client's, keeps none.
func TestPG_CreateRunStoresItsShape(t *testing.T) {
	srv, pool := pgHarness(t)
	srv.cfg.AgentImages = composeAgentImages
	const (
		bg    = `"experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},"tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
		inter = `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	)
	for _, tc := range []struct {
		name, body string
		want       types.RunShape
	}{
		{"a named shape", `{"title":"t","shape":"environment",` + inter + `}`, types.RunShapeEnvironment},
		{"a background agent task names none", `{"title":"t",` + bg + `}`, types.RunShapeAgentTask},
		{"an interactive run with a harness fits two", `{"title":"t",` + inter + `}`, ""},
		{"an older client's run", `{"agent":"claude-code","repo":"acme/widgets","task":"t"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, tc.body)
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
			if created.Shape != tc.want || stored.Shape != tc.want {
				t.Errorf("shape = %q answered, %q stored; want %q", created.Shape, stored.Shape, tc.want)
			}
		})
	}
}
