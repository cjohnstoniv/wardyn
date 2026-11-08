// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoVotePath is a reviewer vote, classified pr_vote.
const adoVotePath = "/acme/proj/_apis/git/repositories/r1/pullrequests/7/reviewers/a1b2?api-version=7.1"

// A run dispatched before the pull-request split holds pr. Its grant, and every resolve answer
// naming pr, still admits a vote: pr reads as pr_create, pr_vote and pr_complete.
func TestADOHoldResolveNamingPRAdmitsVote(t *testing.T) {
	t.Run("a grant naming pr forwards a vote without asking", func(t *testing.T) {
		cp := newCapControlPlane(t)
		h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalApproved)})
		h.p.adoGrants[adoHost] = ADOGrant{Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapPR}}
		h.do(t, http.MethodPut, adoVotePath, `{"vote":10}`, nil)
		if asks, _ := cp.snapshot(); len(asks) != 0 {
			t.Errorf("a vote under a pr grant was asked for: %v", asks)
		}
		if n := len(h.fake.Requests()); n != 1 {
			t.Errorf("upstream saw %d requests, want the vote forwarded", n)
		}
	})
	for _, tc := range []struct {
		name   string
		answer func(first bool) (int, any)
	}{
		{"a first ask answered with pr", func(bool) (int, any) {
			return http.StatusOK, types.ResolvedInjection{Header: "Authorization", Value: "Bearer " + adoToken, Capabilities: []string{"code_read", "pr"}}
		}},
		{"a held ask approved for the run, answered with pr", func(first bool) (int, any) {
			if first {
				return http.StatusLocked, map[string]string{"state": capabilityPendingState, "approval_id": "6f1d1b6e-8a0c-4a52-9a3e-2b0c4f1e7d11"}
			}
			return http.StatusOK, types.ResolvedInjection{Header: "Authorization", Value: "Bearer " + adoToken, Capabilities: []string{"code_read", "pr"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := &capControlPlane{}
			cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cp.mu.Lock()
				defer cp.mu.Unlock()
				q := r.URL.Query()
				cp.asks = append(cp.asks, q)
				if q.Get("capability") != string(adoscope.CapPRVote) {
					t.Errorf("ask names capability %q, want pr_vote", q.Get("capability"))
				}
				status, body := tc.answer(q.Get("approval") == "")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(body)
			}))
			t.Cleanup(cp.srv.Close)
			h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalApproved)})
			rec := h.do(t, http.MethodPut, adoVotePath, `{"vote":10}`, nil)
			if n := len(h.fake.Requests()); n != 1 {
				t.Fatalf("upstream saw %d requests (status %d body %s), want the vote forwarded", n, rec.Code, rec.Body.String())
			}
			if !h.p.inject.reauth.holds(adoscope.CapPRVote) {
				t.Error("the run was not widened to pr_vote")
			}
		})
	}
}
