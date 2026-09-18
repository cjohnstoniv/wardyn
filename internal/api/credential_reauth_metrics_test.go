// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// EVERY LABEL AT ITS OWN TRANSITION (general S1, security NIT-3). The first
// shape bumped expired/cancelled at a later RESOLVE that happened to meet a
// terminal row — which counts retries rather than outcomes (the measured SDK
// cadence is ~30 s, so one row scored dozens) and never fires at all once the
// sidecar has given up — and had no `timeout` label at all, with a comment
// pointing at a series that could not answer for it.

func metricsText(t *testing.T, s *Server) string {
	t.Helper()
	var b strings.Builder
	s.metrics.write(&b)
	return b.String()
}

func reauthCount(t *testing.T, s *Server, outcome string) string {
	t.Helper()
	for _, line := range strings.Split(metricsText(t, s), "\n") {
		if strings.HasPrefix(line, `wardyn_credential_reauth_total{outcome="`+outcome+`"}`) {
			return strings.TrimSpace(strings.SplitN(line, "}", 2)[1])
		}
	}
	t.Fatalf("no wardyn_credential_reauth_total series for %q", outcome)
	return ""
}

// The CLOSED label set carries all five the plan named — `timeout` included,
// which is the one an operator tunes the knob against.
func TestCredentialReauthMetrics_EveryPlannedLabelIsExposed(t *testing.T) {
	s := New(Config{})
	text := metricsText(t, s)
	for _, outcome := range []string{"requested", "resolved", "expired", "cancelled", "timeout"} {
		if !strings.Contains(text, `wardyn_credential_reauth_total{outcome="`+outcome+`"}`) {
			t.Errorf("the %q series is missing; an outcome nobody can graph is an outcome nobody sees", outcome)
		}
	}
}

// A HOLD EXPIRY IS NOT A POLICY DENIAL. Policy allowed the host and allowed the
// request; what ran out was a person's time. Counting it on
// wardyn_egress_denies_total — the series whose HELP promises "denial by
// policy" and which operators alert on — pages security for somebody at lunch.
func TestCredentialReauthMetrics_TimeoutIsNotAPolicyDenial(t *testing.T) {
	if isPolicyDeny(ruleSourceCredentialReauthTimeout) {
		t.Error("credential:reauth-timeout counts as a policy denial; a person not signing in would page security")
	}
	// The exclusion is narrow: every genuine denial still counts.
	for _, src := range []string{"policy:denied", "policy:default-deny", "approval:denied", "builtin:private-ip", "policy:tool-deny"} {
		if !isPolicyDeny(src) {
			t.Errorf("%q stopped counting as a policy denial — the exclusion is too wide", src)
		}
	}
}

// …and it IS counted, on its own series, where the daemon learns of it: the
// expiry happens in the sidecar and the approval row deliberately stays
// PENDING, so this decision row is the only signal that reaches the daemon.
//
// DRIVEN THROUGH THE REAL INGEST (round-2 F2). The first shape called the
// recorder directly and then re-implemented the ingest predicate in the test,
// so deleting the wiring in handlePostDecision left it green — a pin that
// cannot see the thing it pins.
func TestCredentialReauthMetrics_TimeoutCountedAtTheDecisionIngest(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)

	before := reauthCount(t, srv, "timeout")
	beforeDenies := metricValue(t, srv, "wardyn_egress_denies_total")

	post := func(ruleSource string) {
		t.Helper()
		body, err := json.Marshal(egress.DecisionLog{
			Request:    egress.Request{Host: "portal.sso.eu-west-2.amazonaws.com", Port: 443, Method: http.MethodGet},
			Decision:   egress.Deny,
			RuleSource: ruleSource,
		})
		if err != nil {
			t.Fatal(err)
		}
		if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, string(body)); w.Code >= 300 {
			t.Fatalf("post decision %s: code = %d; body=%s", ruleSource, w.Code, w.Body.String())
		}
	}

	post(ruleSourceCredentialReauthTimeout)

	if after := reauthCount(t, srv, "timeout"); after == before {
		t.Fatalf("a credential:reauth-timeout decision did not move the timeout label (%s -> %s) — "+
			"the expiry happens in the SIDECAR, so this ingest is the only signal that reaches the daemon", before, after)
	}
	// …and it did NOT move the policy-denial series, which is the one operators
	// page on: a person who went to lunch is not a policy denial.
	if got := metricValue(t, srv, "wardyn_egress_denies_total"); got != beforeDenies {
		t.Errorf("wardyn_egress_denies_total moved %s -> %s on a hold expiry", beforeDenies, got)
	}

	// The control: an ORDINARY policy deny moves the denial series and not the
	// re-auth one, so the routing above is a real discrimination.
	reauthBefore := reauthCount(t, srv, "timeout")
	post("policy:denied")
	if got := metricValue(t, srv, "wardyn_egress_denies_total"); got == beforeDenies {
		t.Error("an ordinary policy deny did not move wardyn_egress_denies_total — the exclusion is too wide")
	}
	if got := reauthCount(t, srv, "timeout"); got != reauthBefore {
		t.Errorf("an ordinary policy deny moved the re-auth timeout label (%s -> %s)", reauthBefore, got)
	}
}

// metricValue reads a single unlabelled counter out of the scrape.
func metricValue(t *testing.T, s *Server, name string) string {
	t.Helper()
	for _, line := range strings.Split(metricsText(t, s), "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, name))
		}
	}
	t.Fatalf("no %s series in the scrape", name)
	return ""
}

// CANCELLED is counted where the RUN ends, not at a later resolve.
func TestCredentialReauthMetrics_CancelledCountedWhenTheRunEnds(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("resolve: %d", w.Code)
	}
	before := reauthCount(t, f.srv, "cancelled")

	f.srv.cancelRunApprovals(context.Background(), f.runID)

	after := reauthCount(t, f.srv, "cancelled")
	if after == before {
		t.Fatalf("cancelling a held run's request did not move the cancelled label (%s -> %s)", before, after)
	}
	// A LATER RESOLVE meeting the now-terminal row must NOT count again: with
	// the measured ~30 s retry cadence that is dozens of "outcomes" for one row.
	if w := f.resolve(t); w.Code != http.StatusForbidden {
		t.Fatalf("resolve after the cancel: code = %d, want 403", w.Code)
	}
	if again := reauthCount(t, f.srv, "cancelled"); again != after {
		t.Errorf("a resolve meeting the terminal row counted a second cancellation (%s -> %s)", after, again)
	}
}

// EXPIRED is counted where the sweeper acts. By then the sidecar gave up hours
// ago, so no resolve will ever meet the row — counting at a resolve counts zero.
//
// THE LOOP that calls this is pinned in cmd/wardynd by
// TestRunApprovalSweeper_CountsCredentialReauthExpiriesAtTheTransition, which
// drives runApprovalSweeper against a stale row and asserts BOTH that the row
// becomes EXPIRED and that exactly one expiry is reported (round-2 F2). This
// case is only the accumulator's own arithmetic — it is deliberately NOT the
// pin for the wiring, because a test that calls the recorder cannot see the
// wiring go.
func TestCredentialReauthMetrics_ExpiredAccumulates(t *testing.T) {
	srv := New(Config{})
	before := reauthCount(t, srv, "expired")
	srv.RecordCredentialReauthExpired(3)
	after := reauthCount(t, srv, "expired")
	if before == after {
		t.Fatalf("the sweeper's expiries did not move the expired label (%s -> %s)", before, after)
	}
	if strings.TrimSpace(after) != "3" {
		t.Errorf("expired = %s, want 3", after)
	}
	// A zero-count report is a no-op: an idle deployment's every tick must not
	// move the series.
	srv.RecordCredentialReauthExpired(0)
	if got := reauthCount(t, srv, "expired"); strings.TrimSpace(got) != "3" {
		t.Errorf("a zero report moved the series to %s", got)
	}
}

// requested and resolved, at the raise and the resolution.
func TestCredentialReauthMetrics_RequestedAndResolvedAtTheirOwnTransitions(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("resolve: %d", w.Code)
	}
	if got := reauthCount(t, f.srv, "requested"); strings.TrimSpace(got) != "1" {
		t.Errorf("requested = %s after one raise, want 1", got)
	}
	// A second resolve joins the SAME request and must not count a second one.
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("second resolve: %d", w.Code)
	}
	if got := reauthCount(t, f.srv, "requested"); strings.TrimSpace(got) != "1" {
		t.Errorf("requested = %s after a dedup'd second resolve, want 1", got)
	}

	ap := onlyReauthRow(t, f.srv)
	loginRun := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: ap.RequestedAt.Add(time.Second)}
	f.st.loginRun = loginRun
	fresh := liveSSOBlob()
	f.putBlob(t, "alice@example.com", fresh)
	f.srv.resolvePendingReauth(context.Background(), awsSSOScope{perUser: true, owner: "alice@example.com"}, "alice@example.com", loginRun)
	if got := reauthCount(t, f.srv, "resolved"); strings.TrimSpace(got) != "1" {
		t.Errorf("resolved = %s after one sign-in, want 1", got)
	}
	// …and the wait it recorded is a real duration.
	if !strings.Contains(metricsText(t, f.srv), "wardyn_credential_reauth_wait_seconds_count 1") {
		t.Error("the wait summary did not record the resolution")
	}
	_ = json.RawMessage(nil)
}
