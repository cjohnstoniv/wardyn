// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApprovedHostResolvingPrivateIsDenied: an APPROVED egress_domain host, whoever decided it (a person or an
// approval rule), that resolves into private address space is still refused by the address guard, which runs after
// the approval releases the host.
func TestApprovedHostResolvingPrivateIsDenied(t *testing.T) {
	const host = "registry.example.test"
	var raises, gets atomic.Int32
	cp := approvalCPStub(apState(types.ApprovalApproved), &raises, &gets)
	defer cp.Close()
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	ap.hosts[approvalHostKey(host)] = &hostApproval{state: apApproved, approvalID: uuid.New(), scope: types.ScopeRun}
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"unrelated.test"}, FirstUseApproval: types.FirstUseWaitForReview}),
		Approval: ap,
		Sink:     sink,
		Resolver: fakeResolver{m: map[string][]net.IP{host: ips("10.9.8.7")}},
		Dial:     redirectDial("127.0.0.1:1"),
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, connectReq(t, host+":443"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("approved host resolving to 10.9.8.7: status = %d, want 403", rec.Code)
	}
	if d := <-sink.ch; d.Decision != egress.Deny || d.RuleSource != "builtin:private-ip" {
		t.Fatalf("decision = %s %s, want deny builtin:private-ip", d.Decision, d.RuleSource)
	}
}
