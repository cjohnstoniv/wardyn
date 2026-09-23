// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMemoedPrivateHostSpendsNoApproval is V1-D5, and it is the finding's argument one
// step further down the pipeline.
//
// B6's memo sat BELOW the first-use approval flow. For an `unknown`-verdict host
// that resolves privately, each of the agent CLI's ten retries therefore
// re-entered Resolve/ResolveWait before reaching the memo: a spent scope=once
// grant was consumed, or a fresh egress_domain question was POSTed to a human, or
// (under wait_for_review) the connection was PARKED until the hold deadline — and
// the request was then refused straight out of the memo with a NIL decision log.
// A human decision spent, and a denial with no decision row at all, where
// pre-0.7.2 there was at least an egress.deny.
//
// A private-IP target can never be approved into reachability, so the memo
// belongs above the raise. The drive: the control plane approves once, the host
// resolves into RFC1918, and nine more identical attempts must touch the control
// plane zero further times while still getting the identical 403.
func TestMemoedPrivateHostSpendsNoApproval(t *testing.T) {
	for _, mode := range []types.FirstUseMode{
		types.FirstUseDenyWithReview, types.FirstUseWaitForReview,
	} {
		t.Run(string(mode), func(t *testing.T) {
			const host = "svc.priv.internal"
			var raises, gets atomic.Int32
			cp := approvalCPStub(apState(types.ApprovalPending), &raises, &gets)
			defer cp.Close()

			ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
			ap.configureHold(mode, 2*time.Second, 4)
			// A human has ALREADY answered this host, scope=once — the exact grant
			// the field report watched get spent on a request that was then denied
			// from the memo with no decision row at all.
			ap.hosts[approvalHostKey(host)] = &hostApproval{
				state: apApproved, approvalID: uuid.New(), scope: types.ScopeOnce,
			}
			buf := &bytes.Buffer{}
			sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
			p := newProxy(Options{
				RunID: uuid.New(),
				Policy: CompilePolicy(types.RunPolicySpec{
					AllowedDomains:   []string{"unrelated.test"},
					FirstUseApproval: mode,
				}),
				Approval: ap,
				Sink:     sink,
				Resolver: fakeResolver{m: map[string][]net.IP{host: ips("10.9.8.7")}},
				Dial:     redirectDial("127.0.0.1:1"),
			})

			// Attempt 1 SPENDS the once-grant, falls through to IP vetting, is
			// refused by the address guard, and opens the memo streak.
			first := httptest.NewRecorder()
			p.ServeHTTP(first, connectReq(t, host+":443"))
			if first.Code != http.StatusForbidden {
				t.Fatalf("first attempt: status = %d, want 403", first.Code)
			}
			if !p.privateIPMemoed(host, 443) {
				t.Fatal("the first refusal did not open a memo streak — this test is asserting nothing")
			}

			// Attempts 2..10: the CLI's retry loop. Each must be answered from the
			// memo, with the same 403, and must not touch the control plane at all.
			for i := range 9 {
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, connectReq(t, host+":443"))
				if rec.Code != http.StatusForbidden {
					t.Fatalf("retry %d: status = %d, want 403", i+2, rec.Code)
				}
				if body := rec.Body.String(); !strings.Contains(body, egressInternalHostsRemedy) {
					t.Fatalf("retry %d: body = %q, want the byte-identical private-IP refusal", i+2, body)
				}
			}
			// The once-grant is spent, so every retry that reaches the approval flow
			// finds apNone and RAISES A NEW APPROVAL — a fresh question POSTed to a
			// human for a host that can never be reached, answered from the memo a
			// line later with a nil log. Zero is the only correct count.
			if n := raises.Load(); n != 0 {
				t.Errorf("the retries raised %d approval(s) — a private-IP target can never be approved "+
					"into reachability, so every one of those is a human asked a question whose answer "+
					"the next line discards", n)
			}
			if n := gets.Load(); n != 0 {
				t.Errorf("the retries polled the control plane %d time(s) for a verdict the address guard "+
					"has already made final", n)
			}

			// The repeat count is attributed, not lost: closing the streak appends ONE
			// summary row carrying all nine suppressed attempts.
			p.flushPrivateIPMemo()
			summary := lastDecision(t, buf)
			if summary.Repeat != 9 {
				t.Errorf("summary repeat = %d, want 9 — the nine suppressed attempts must still be "+
					"attributed to a row, or the trail under-reports the refusal", summary.Repeat)
			}
			if summary.RuleSource != "builtin:private-ip" {
				t.Errorf("summary rule_source = %q, want builtin:private-ip", summary.RuleSource)
			}
		})
	}
}
