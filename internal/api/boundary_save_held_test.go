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

// TestGetBoundaryReportsWhetherTheCallersSaveIsHeld (#3389): the boundary read carries save_held_for_approval
// only when this caller's save would be held for a second human, so the editor words its submit to match. The
// admin token saves directly past the switch (govBypass), so it never reads true. Local mode refuses the write
// outright (#3568), so no caller there has a held save either.
func TestGetBoundaryReportsWhetherTheCallersSaveIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    string
		admin  bool // the admin token, not a signed-in person
		local  bool // local mode, where the write is refused rather than held
		wantOn bool
	}{
		{"switch on, signed-in security admin", "true", false, false, true},
		{"switch on, admin token", "true", true, false, false},
		{"switch off, signed-in security admin", "false", false, false, false},
		{"switch off, admin token", "false", true, false, false},
		{"switch on, local mode", "true", false, true, false},
		{"switch on, local mode, admin token", "true", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envGovernanceSecondHuman, tc.env)
			srv := &Server{cfg: Config{Store: hardBoundary(types.BoundarySpec{}), LocalMode: tc.local}}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/governance/boundary", nil)
			if !tc.admin {
				req = req.WithContext(withHumanIdentity(req.Context(), "sub-alice", "alice@corp.example", "admin", "standard", nil, false))
			}
			w := httptest.NewRecorder()
			srv.handleGetBoundary(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			got, present := raw["save_held_for_approval"]
			if present != tc.wantOn || (present && string(got) != "true") {
				t.Errorf("save_held_for_approval = %s (present %v), want present=%v", got, present, tc.wantOn)
			}
		})
	}
}
