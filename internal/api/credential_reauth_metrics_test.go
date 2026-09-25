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

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every label at its own transition (general S1, security NIT-3). The first
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

// A hold expiry is not a policy denial. Policy allowed the host and allowed the
// request; what ran out was a person's time. Counting it on
// wardyn_egress_denies_total — the series whose HELP promises "denial by
// policy" and which operators alert on — pages security for somebody at lunch.
func TestCredentialReauthMetrics_TimeoutIsNotAPolicyDenial(t *testing.T) {
	if isPolicyDeny(ruleSourceCredentialReauthTimeout) {
		t.Error("credential:reauth-timeout counts as a policy denial; a person not signing in would page security")
	}
	// The exclusion is narrow: every genuine denial still counts.
	for _, src := range []string{"policy:denied", "policy:default-deny", "approval:denied", "builtin:private-ip", "builtin:upstream-protocol-mismatch", "policy:tool-deny"} {
		if !isPolicyDeny(src) {
			t.Errorf("%q stopped counting as a policy denial — the exclusion is too wide", src)
		}
	}
}

// …and it IS counted, on its own series, where the daemon learns of it: the
// expiry happens in the sidecar and the approval row deliberately stays
// PENDING, so this decision row is the only signal that reaches the daemon.
//
// Driven through the real ingest (round-2 F2). The first shape called the
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

// The proxy writes the SAME credential:reauth-timeout rule source for an Azure
// DevOps sign-in hold that ran out (ado_hold.go's adoCredentialRefusalFor), on
// the Azure DevOps host itself. wardyn_credential_reauth_total's HELP promises
// the AWS SSO re-auth population alone (#971), so that decision must not move
// the series — only a non-Azure-DevOps host (the AWS/model-provider lane) does.
func TestCredentialReauthMetrics_TimeoutCountsOnlyTheAWSLane(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)

	post := func(host string) {
		t.Helper()
		body, err := json.Marshal(egress.DecisionLog{
			Request:    egress.Request{Host: host, Port: 443, Method: http.MethodGet},
			Decision:   egress.Deny,
			RuleSource: ruleSourceCredentialReauthTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, string(body)); w.Code >= 300 {
			t.Fatalf("post decision for host %s: code = %d; body=%s", host, w.Code, w.Body.String())
		}
	}

	for _, host := range adoEntraHosts("myorg") {
		before := reauthCount(t, srv, "timeout")
		post(host)
		if after := reauthCount(t, srv, "timeout"); after != before {
			t.Errorf("an Azure DevOps sign-in timeout (host=%s) moved the timeout label (%s -> %s) — "+
				"the series is the AWS SSO re-auth population alone", host, before, after)
		}
	}

	before := reauthCount(t, srv, "timeout")
	post("portal.sso.eu-west-2.amazonaws.com")
	if after := reauthCount(t, srv, "timeout"); after == before {
		t.Errorf("an AWS SSO timeout did not move the timeout label (%s -> %s)", before, after)
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
	// A later resolve meeting the now-terminal row must not count again: with
	// the measured ~30 s retry cadence that is dozens of "outcomes" for one row.
	if w := f.resolve(t); w.Code != http.StatusForbidden {
		t.Fatalf("resolve after the cancel: code = %d, want 403", w.Code)
	}
	if again := reauthCount(t, f.srv, "cancelled"); again != after {
		t.Errorf("a resolve meeting the terminal row counted a second cancellation (%s -> %s)", after, again)
	}
}

// humanWinsStore models a human deciding one row between CancelForRun's list
// and its CAS: the row still lists PENDING, but its CAS answers
// ErrAlreadyDecided because the human's decision landed first.
type humanWinsStore struct {
	*evictionApprovalStore
	decidedByHuman uuid.UUID
}

func (s humanWinsStore) DecideApproval(ctx context.Context, id uuid.UUID, d types.ApprovalDecision) (types.ApprovalRequest, error) {
	if id == s.decidedByHuman {
		return types.ApprovalRequest{}, approval.ErrAlreadyDecided
	}
	return s.evictionApprovalStore.DecideApproval(ctx, id, d)
}

// humanWinsApprovals lists through the underlying store (so the raced row still
// reads PENDING) and cancels through humanWinsStore.
type humanWinsApprovals struct {
	evictionApprovals
	race humanWinsStore
}

func (a humanWinsApprovals) CancelForRun(ctx context.Context, runID uuid.UUID, reason string) (map[string]int, error) {
	return approval.CancelForRun(ctx, a.race, runID, reason)
}

// …and only for AWS SSO re-auth rows the cancel actually MOVED (#151). A row a
// human decides between the list and the CAS is theirs, not a cancellation:
// counting it from a pre-read of PENDING rows scored an outcome that never
// happened. And an Azure DevOps sign-in or consent row is credential_reauth
// too, but not the AWS SSO re-auth this series' HELP describes.
func TestCredentialReauthMetrics_CancelledCountsOnlyMovedRows(t *testing.T) {
	runID := uuid.New()
	st := &evictionApprovalStore{rows: map[uuid.UUID]types.ApprovalRequest{}, rec: &syncAudit{}}
	raced, moved, adoSignIn, adoConsent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	scopes := map[uuid.UUID]string{
		raced: `{"mechanism":"bedrock_sso","credential_source":"per_user","owner":"alice"}`,
		moved: `{"mechanism":"bedrock_sso","credential_source":"per_user","owner":"bob"}`,
		adoSignIn: `{"lane":"` + adoApprovalLane + `","mechanism":"` + adoSignInMechanism +
			`","owner":"alice","provider_id":"p"}`,
		adoConsent: `{"lane":"` + adoApprovalLane + `","mechanism":"` + adoConsentMechanism +
			`","owner":"alice","provider_id":"p","scopes":["s"]}`,
	}
	for id, scope := range scopes {
		st.rows[id] = types.ApprovalRequest{
			ID: id, RunID: runID, Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending,
			RequestedScope: json.RawMessage(scope),
		}
	}
	srv := New(Config{Approvals: humanWinsApprovals{
		evictionApprovals: evictionApprovals{st: st},
		race:              humanWinsStore{evictionApprovalStore: st, decidedByHuman: raced},
	}})
	before := reauthCount(t, srv, "cancelled")

	srv.cancelRunApprovals(context.Background(), runID)

	if got := reauthCount(t, srv, "cancelled"); strings.TrimSpace(got) != "1" || strings.TrimSpace(before) != "0" {
		t.Errorf("cancelled = %s -> %s, want 0 -> 1: only the AWS SSO row the cancel moved counts — "+
			"not the one a human decided first, nor the Azure DevOps sign-in and consent rows", before, got)
	}
	for _, id := range []uuid.UUID{moved, adoSignIn, adoConsent} {
		if got := st.rows[id].State; got != types.ApprovalCancelled {
			t.Errorf("row %s is %s, want CANCELLED — the split changes what is counted, not what is cancelled", id, got)
		}
	}
}

// TestApprovalTallyKeyAgreesWithTheLanes: approval.TallyKey restates this
// package's own row definitions (it cannot import them), so each shape is built
// here from the structs and constants the raising code uses and checked against
// both definitions — the tally key and the lane's own predicate must agree.
func TestApprovalTallyKeyAgreesWithTheLanes(t *testing.T) {
	grant := uuid.New()
	mk := func(kind types.ApprovalKind, scope any, grantID *uuid.UUID) types.ApprovalRequest {
		raw, err := json.Marshal(scope)
		if err != nil {
			t.Fatal(err)
		}
		return types.ApprovalRequest{ID: uuid.New(), Kind: kind, RequestedScope: raw, GrantID: grantID}
	}
	escalation := mk(types.ApprovalToolCall, adoCapabilityScope{
		Lane: adoApprovalLane, GrantID: grant, Capability: "code_write", Tool: "azure_devops", Cmd: "x",
	}, &grant)
	if _, ok := adoEscalationScope(escalation); !ok {
		t.Fatal("fixture: the escalation row is not one adoEscalationScope accepts")
	}
	signIn := mk(types.ApprovalCredentialReauth, adoSignInScopeBody{
		Lane: adoApprovalLane, Mechanism: adoSignInMechanism, Owner: "alice", ProviderID: "p",
	}, nil)
	if _, ok := adoSignInScope(signIn); !ok {
		t.Fatal("fixture: the sign-in row is not one adoSignInScope accepts")
	}
	consent := mk(types.ApprovalCredentialReauth, adoConsentScopeBody{
		Lane: adoApprovalLane, Mechanism: adoConsentMechanism, Owner: "alice", ProviderID: "p", Scopes: []string{"s"},
	}, nil)
	if _, ok := adoConsentScope(consent); !ok {
		t.Fatal("fixture: the consent row is not one adoConsentScope accepts")
	}
	// The AWS raise's own scope shape (holdOrRefuseCredentialReauth).
	aws := mk(types.ApprovalCredentialReauth, awsSSOReauthScopeBody{
		Mechanism: string(types.AgentMechanismBedrockSSO), CredentialSource: string(types.CredentialSourcePerUser), Owner: "alice",
	}, nil)
	if !reauthResolvableBy(types.ApprovalRequest{Kind: aws.Kind, State: types.ApprovalPending, RequestedScope: aws.RequestedScope},
		awsSSOScope{perUser: true, owner: "alice"}, types.AgentRun{CreatedAt: time.Now().Add(time.Hour)}) {
		t.Fatal("fixture: the AWS row is not one reauthResolvableBy accepts")
	}
	// A tool_call whose grant_id column is unset is a hook's, whatever its scope says.
	hook := mk(types.ApprovalToolCall, adoCapabilityScope{Lane: adoApprovalLane, GrantID: grant}, nil)
	if _, ok := adoEscalationScope(hook); ok {
		t.Fatal("fixture: a grant-less tool_call must not be an escalation")
	}

	for _, tc := range []struct {
		name string
		ap   types.ApprovalRequest
		want string
	}{
		{"ado escalation", escalation, approval.TallyToolCallADO},
		{"ado sign-in", signIn, approval.TallyReauthADOSignIn},
		{"ado consent", consent, approval.TallyReauthADOConsent},
		{"aws sso re-auth", aws, approval.TallyReauthAWSSSO},
		{"hook tool_call", hook, string(types.ApprovalToolCall)},
	} {
		if got := approval.TallyKey(tc.ap); got != tc.want {
			t.Errorf("%s: TallyKey = %q, want %q", tc.name, got, tc.want)
		}
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
