// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A read that fails while the local-eligibility step asks again, for a waiting run's own runners, records
// nothing: the launch behaves as one with nothing recorded, the step still passes, and it answers nothing.
func TestStepLocalEligibility_FailedSecondAskRecordsNothing(t *testing.T) {
	srv := &Server{cfg: Config{Store: &scmTestStore{err: errors.New("site config unavailable")}}}
	req := createRunRequest{Agent: "claude-code", Task: "t"}
	w := httptest.NewRecorder()
	f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), req: &req, queued: true,
		place: runPlacement{pooled: true, placement: types.PlacementRemote}}
	if ok := srv.stepLocalEligibility(f, &foldRecorder{}); !ok || f.place.noLocal || w.Body.Len() != 0 {
		t.Fatalf("the step = %v, no_local %v, answered %q; want it passed with nothing recorded and nothing written", ok, f.place.noLocal, w.Body)
	}
}
