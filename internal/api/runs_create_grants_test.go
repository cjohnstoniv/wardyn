// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPersistRunGrantsLeavesApprovalGatedKeyOffTheProxy: an approval-gated
// api_key grant is never on the proxy's injection list, so the proxy never
// resolves it and no run is handed its key. The same grant without approval
// is injected.
func TestPersistRunGrantsLeavesApprovalGatedKeyOffTheProxy(t *testing.T) {
	for _, gated := range []bool{false, true} {
		srv, _, _ := govEscapeFixture(t, &capStore{})
		g := types.GrantSpec{Kind: types.GrantAPIKey, RequiresApproval: gated,
			Scope: mustJSON(map[string]any{"host": "api.example.test", "header": "x-api-key", "secret_name": govCorpSecret})}
		gw, ok := srv.persistRunGrants(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil),
			uuid.New(), time.Now().UTC(), types.RunPolicySpec{EligibleGrants: []types.GrantSpec{g}}, types.PlacementRemote, localJudgement{})
		if !ok {
			t.Fatalf("requires_approval=%v: persistRunGrants failed", gated)
		}
		want := 1
		if gated {
			want = 0
		}
		if len(gw.injections) != want {
			t.Errorf("requires_approval=%v: injections = %+v, want %d", gated, gw.injections, want)
		}
	}
}
