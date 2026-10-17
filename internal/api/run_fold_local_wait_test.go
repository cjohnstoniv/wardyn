// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The "launch it remote" sentence follows a sent 4xx refusal only: a status-0 denial and a server error keep
// their body, and no refusal at all is a no-op.
func TestRunRefusal_AddRemoteInstead_OnlyASent4xx(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		want   bool
	}{"status 0 (a denial decision)": {0, false}, "a 4xx": {http.StatusUnprocessableEntity, true}, "a 5xx": {http.StatusInternalServerError, false}} {
		t.Run(name, func(t *testing.T) {
			ref := runError(tc.status, "placement_credential", "refused")
			ref.addRemoteInstead("Mixed")
			if got := strings.Contains(ref.body.Error, remoteInsteadMsg("Mixed")); got != tc.want {
				t.Errorf("body %q has the sentence = %v, want %v", ref.body.Error, got, tc.want)
			}
		})
	}
	var none *runRefusal
	none.addRemoteInstead("Mixed")
}
