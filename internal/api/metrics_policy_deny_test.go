// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestEgressDeniesTotalCountsPolicyDeniesOnly: wardyn_egress_denies_total
// counts policy denials only. Its exposition declares it "Egress decisions
// ingested with decision=deny, by reason (proxy decision ingest)" (DRAFT, M2
// canon pending) — the only egress counter Wardyn exposes — and two large
// classes of Deny are not policy denials at all: builtin:dial-failed (a failed
// upstream dial on a request policy ALLOWED, emitted from four proxy sites)
// and the synthetic egress.decisions.dropped:<n> summary (an audit-fidelity
// alert about lost decision records). Counting them would page an operator
// alerting on the series for a flaky upstream or a wedged control plane, and
// hide the true deny rate. Counting every Deny, the series reads 4 instead of
// 2.
//
// The audit half is asserted in the same test on purpose: scoping the COUNTER
// must not stop recording the egress.deny rows those decisions still
// legitimately produce.
func TestEgressDeniesTotalCountsPolicyDeniesOnly(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())

	for _, rs := range []string{
		"policy",                      // a real policy deny — counts
		"builtin:private-ip",          // a builtin GUARD deny — still a denial, counts
		"builtin:dial-failed",         // the upstream dial lost it — must NOT count
		"egress.decisions.dropped:42", // audit-fidelity summary — must NOT count
	} {
		body := fmt.Sprintf(`{"request":{"host":"api.example.com","port":443,"method":"CONNECT"},`+
			`"decision":"deny","rule_source":%q}`, rs)
		if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", tok, body); w.Code != http.StatusAccepted {
			t.Fatalf("POST decision %s = %d, want 202; body=%s", rs, w.Code, w.Body.String())
		}
	}

	w := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	if want := "wardyn_egress_denies_total 2"; !strings.Contains(w.Body.String(), want) {
		var got string
		for _, l := range strings.Split(w.Body.String(), "\n") {
			if strings.HasPrefix(l, "wardyn_egress_denies_total ") {
				got = l
			}
		}
		t.Errorf("policy-deny series = %q, want %q (dial-failed and decisions-dropped are not policy denials)", got, want)
	}

	var denies int
	for _, ev := range h.audit.events {
		if ev.Action == "egress.deny" {
			denies++
		}
	}
	if denies != 4 {
		t.Errorf("egress.deny audit rows = %d, want 4 — scoping the counter must not drop audit rows", denies)
	}
}
