// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/authz"
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

// The "launch it remote" sentence follows a sent 4xx refusal and a denial decision: a server error keeps
// its body, a denial that hides its cause or answers with another reason keeps its sentence, and no refusal
// at all is a no-op.
func TestRunRefusal_AddRemoteInstead_A4xxAndADenial(t *testing.T) {
	hint := remoteInsteadMsg("Mixed")
	for name, tc := range map[string]struct {
		status int
		want   bool
	}{"a 4xx": {http.StatusUnprocessableEntity, true}, "a 5xx": {http.StatusInternalServerError, false}} {
		t.Run(name, func(t *testing.T) {
			ref := runError(tc.status, "placement_credential", "refused")
			ref.addRemoteInstead("Mixed")
			if got := strings.Contains(ref.body.Error, hint); got != tc.want {
				t.Errorf("body %q has the sentence = %v, want %v", ref.body.Error, got, tc.want)
			}
		})
	}
	denied := runDenied(authz.Deny(authz.ReasonGovernanceUnit, "runs.placement", "Your profile keeps this run off your computer."))
	denied.addRemoteInstead("Mixed")
	if want := "Your profile keeps this run off your computer. " + hint; denied.decision.Sentence != want {
		t.Errorf("the denial says %q, want %q", denied.decision.Sentence, want)
	}
	hidden := authz.Deny(authz.ReasonGovernanceUnit, "runs.placement", "kept")
	hidden.WireReason = authz.ReasonGovernanceUnit
	masked := runDenied(hidden)
	masked.addRemoteInstead("Mixed")
	if masked.decision.Sentence != "kept" {
		t.Errorf("a denial answering with a wire reason says %q, want it unchanged", masked.decision.Sentence)
	}
	var none *runRefusal
	none.addRemoteInstead("Mixed")
}
