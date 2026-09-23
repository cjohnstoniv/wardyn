// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestResolveWaitDoesNotRetryTheHostCap: past maxApprovalHosts, Resolve fails
// closed with no entry, no raise and no row, and reports it as a DISTINCT
// state (apCapped), not as {apPending, uuid.Nil} — the SAME shape a raise
// still in flight has. Otherwise ResolveWait's concurrent-raise retry loop
// cannot tell them apart and sleeps its whole budget (concurrentRaiseRetries *
// holdPollInterval = 5s) for every new host, holding a goroutine and a socket
// with no holdSem slot to bound it.
//
// That would turn the fail-closed cap into a way for the sandbox to pin
// unbounded goroutines for 5 s each in a 256 MiB sidecar: the cap is reached
// by NAMING hosts, which the agent controls. evaluate's default arm maps
// anything that is not approved/denied to Pending, so the wire answer is the
// same either way.
//
// Runs at the REAL holdPollInterval so "well under the retry budget" means the
// shipped 5 s, not a shrunken one.
func TestResolveWaitDoesNotRetryTheHostCap(t *testing.T) {
	// No CP stub: a capped Resolve must not make a network call either. A URL
	// nothing listens on makes that a failure rather than a silent pass.
	ap := newApprovalClient("http://127.0.0.1:1", newTokenSource("tok"), uuid.New(), nil)
	ap.configureHold(types.FirstUseWaitForReview, 30*time.Second, 16)

	// Saturate the per-run host table directly: the cap is a count, and 4096
	// real raises would test the CP stub rather than the cap.
	for i := 0; len(ap.hosts) < maxApprovalHosts; i++ {
		ap.hosts[fmt.Sprintf("h%d.test", i)] = &hostApproval{state: apApproved}
	}

	start := time.Now()
	res := ap.ResolveWait(context.Background(), "new.test")
	elapsed := time.Since(start)

	budget := concurrentRaiseRetries * holdPollInterval
	if elapsed >= holdPollInterval {
		t.Fatalf("ResolveWait over the host cap took %v (retry budget %v); the capped verdict is "+
			"indistinguishable from a raise in flight, so every new host parks a goroutine", elapsed, budget)
	}
	if res.State == apApproved || res.State == apDenied {
		t.Fatalf("capped ResolveWait = %v, want a fail-closed non-terminal state", res.State)
	}
}

// TestCappedHostIsAuditedAsTheHostCap: apCapped and apPending are DIFFERENT FACTS,
// and the operator reading decision rows must be able to tell them apart. "An
// approval is waiting on you" and "the run's host table is full, nothing was
// raised and nothing ever will be" have different fixes, and one slog line in the
// sidecar is not a signal an operator sees. The wire verdict is the same (Pending,
// fail closed); only the reason label distinguishes them.
func TestCappedHostIsAuditedAsTheHostCap(t *testing.T) {
	buf := &bytes.Buffer{}
	ap := newApprovalClient("http://127.0.0.1:1", newTokenSource("tok"), uuid.New(), nil)
	for i := 0; len(ap.hosts) < maxApprovalHosts; i++ {
		ap.hosts[fmt.Sprintf("h%d.test", i)] = &hostApproval{state: apApproved}
	}
	p := newProxy(Options{
		RunID: uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{
			FirstUseApproval: types.FirstUseDenyWithReview,
		}),
		Approval: ap,
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		Resolver: publicResolver{},
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://new.test/"))

	d := lastDecision(t, buf)
	if d.Decision != egress.Pending {
		t.Fatalf("decision = %q, want pending (fail closed)", d.Decision)
	}
	if d.RuleSource != ruleSourceApprovalHostCap {
		t.Errorf("rule_source = %q, want %q — a capped host is indistinguishable from a raise "+
			"awaiting a human", d.RuleSource, ruleSourceApprovalHostCap)
	}
}
