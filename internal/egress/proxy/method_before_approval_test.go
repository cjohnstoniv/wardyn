// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestEvaluate_MethodRefusalRaisesNoApproval pins F032: the method restriction
// is decided BEFORE the first-use approval flow, so a request whose method can
// never pass raises no ApprovalRequest and (under wait_for_review) takes no
// hold slot. The sandbox picks the method, so the old ordering let it pick how
// many approval rows and hold slots it could create: N POSTs to N unknown hosts
// under a GET-only policy filled the operator's queue with decisions the very
// next step refuses unconditionally.
func TestEvaluate_MethodRefusalRaisesNoApproval(t *testing.T) {
	for _, mode := range []types.FirstUseMode{
		types.FirstUseDenyWithReview, types.FirstUseWaitForReview,
	} {
		t.Run(string(mode), func(t *testing.T) {
			var raised atomic.Int32
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/internal/approvals") {
					raised.Add(1)
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
					return
				}
				http.Error(w, "unexpected", http.StatusTeapot)
			}))
			defer cp.Close()

			ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
			p, _ := newTestProxy(t, types.RunPolicySpec{
				AllowedDomains:   []string{"known.test"},
				AllowedMethods:   []string{http.MethodGet},
				FirstUseApproval: mode,
			}, "127.0.0.1:1", ap, nil)

			dec, target, log := p.evaluate(context.Background(), "unknown.test", 443, http.MethodPost, "/x")

			if dec != egress.Deny {
				t.Fatalf("decision = %q (rule %q), want deny: the method can never pass, so nothing may be pending",
					dec, log.RuleSource)
			}
			if log.RuleSource != "policy:method" {
				t.Fatalf("rule_source = %q, want policy:method", log.RuleSource)
			}
			if target != "" {
				t.Fatalf("dial target must be empty on deny, got %q", target)
			}
			if n := raised.Load(); n != 0 {
				t.Fatalf("approvals raised = %d, want 0: a human must not be asked to decide egress "+
					"for a request the method restriction refuses unconditionally", n)
			}
		})
	}
}

// TestEvaluate_PolicyDenyStillBeatsMethod is the ordering guard that comes with
// the fix: hoisting the method check must not restate a policy-denied host as
// policy:method. A denied host is denied for BEING denied.
func TestEvaluate_PolicyDenyStillBeatsMethod(t *testing.T) {
	p, _ := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains: []string{"known.test"},
		DeniedDomains:  []string{"blocked.test"},
		AllowedMethods: []string{http.MethodGet},
	}, "127.0.0.1:1", nil, nil)

	_, _, log := p.evaluate(context.Background(), "blocked.test", 443, http.MethodPost, "/x")
	if log.RuleSource != "policy:denied" {
		t.Fatalf("rule_source = %q, want policy:denied", log.RuleSource)
	}
}
