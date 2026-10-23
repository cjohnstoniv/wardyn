// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGetBoundaryReportsWhetherASecondHumanIsRequired (#3389): the boundary read carries
// second_human_required from WARDYN_GOVERNANCE_SECOND_HUMAN, so the editor words its submit to match.
func TestGetBoundaryReportsWhetherASecondHumanIsRequired(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{{"true", true}, {"false", false}} {
		t.Run("switch="+tc.env, func(t *testing.T) {
			t.Setenv(envGovernanceSecondHuman, tc.env)
			srv := &Server{cfg: Config{Store: hardBoundary(types.BoundarySpec{})}}
			w := httptest.NewRecorder()
			srv.handleGetBoundary(w, httptest.NewRequest(http.MethodGet, "/api/v1/governance/boundary", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			got, present := raw["second_human_required"]
			if present != tc.want || (present && string(got) != "true") {
				t.Errorf("second_human_required = %s (present %v), want present=%v true", got, present, tc.want)
			}
		})
	}
}
