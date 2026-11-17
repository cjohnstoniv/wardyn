// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A via_org engine bounds the total hold time of one request, so a held request gets the hold's own
// sentence before the relay's idle cut would reset the stream. A sidecar sets none.

const holdCeilingTest = 2 * time.Second

// serveWithin runs serve and fails the test if it outlasts limit: a ceiling that is not applied
// leaves the hold on its own 120 s to 600 s window.
func serveWithin(t *testing.T, limit time.Duration, serve func() *httptest.ResponseRecorder) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	start := time.Now()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve() }()
	select {
	case rec := <-done:
		return rec, time.Since(start)
	case <-time.After(limit):
		t.Fatalf("the hold outlasted %s: the ceiling was not applied", limit)
		return nil, 0
	}
}

// workflowsEndWithin fails the test if a hold workflow the coordinator opened runs past start+limit:
// the workflow's own deadline is capped too, not only the request's wait for it.
func workflowsEndWithin(t *testing.T, c *reauthCoordinator, start time.Time, limit time.Duration) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.workflows) == 0 {
		t.Error("the request opened no hold workflow")
	}
	for id, wf := range c.workflows {
		if wf.deadline.Sub(start) > limit {
			t.Errorf("hold workflow %s runs %s from the request, want at most %s", id, wf.deadline.Sub(start), limit)
		}
	}
}

// doHeld is adoHarness.do on a request carrying a hold budget, as the engine gives one.
func (h *adoHarness) doHeld(t *testing.T, ceiling time.Duration, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: " + adoHost + "\r\n"
	if body != "" {
		raw += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n" + body)))
	if err != nil {
		t.Errorf("build %s %s: %v", method, target, err)
		return httptest.NewRecorder()
	}
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, withHoldBudget(req, ceiling), adoHost, 443)
	return rec
}

func TestHoldCeiling_CapabilityHoldEndsAtTheCeiling(t *testing.T) {
	cp := newCapControlPlane(t)
	h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending)})
	start := time.Now()
	rec, took := serveWithin(t, 3*holdCeilingTest, func() *httptest.ResponseRecorder {
		return h.doHeld(t, holdCeilingTest, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1?api-version=7.1", adoWorkItemPatch)
	})
	h.mustRefuse(t, rec, adoHoldTimedOutRefusal)
	workflowsEndWithin(t, h.p.inject.reauth, start, holdCeilingTest+100*time.Millisecond)
	if took < holdCeilingTest || took > 3*holdCeilingTest {
		t.Errorf("the capability hold lasted %s, want about %s", took, holdCeilingTest)
	}
}

func TestHoldCeiling_ReauthHoldEndsAtTheCeiling(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	h := newADOHarness(t, adoscope.CapProjectRead)
	cp := newSignInControlPlane(t, adoToken)
	lapse(t, h.p.inject, adoHost, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending)})
	start := time.Now()
	rec, took := serveWithin(t, 3*holdCeilingTest, func() *httptest.ResponseRecorder {
		return h.doHeld(t, holdCeilingTest, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "")
	})
	workflowsEndWithin(t, h.p.inject.reauth, start, holdCeilingTest+100*time.Millisecond)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), adoSignInTimedOutRefusal) {
		t.Fatalf("status %d body %s, want the sign-in hold's own timeout sentence", rec.Code, rec.Body.String())
	}
	if took < holdCeilingTest || took > 3*holdCeilingTest {
		t.Errorf("the re-auth hold lasted %s, want about %s", took, holdCeilingTest)
	}
}

func TestHoldCeiling_PushApprovalEndsAtTheCeiling(t *testing.T) {
	p, _, up, _, _ := newAppLaneHold(t, reviewSpec(60, []string{".github/workflows/**"}), types.ApprovalPending)
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
	rec, took := serveWithin(t, 3*holdCeilingTest, func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, withHoldBudget(mustLocalReq(t, http.MethodPost,
			"/wardyn/gh/octocat/hello-world/git-receive-pack", bytes.NewReader(body)), holdCeilingTest))
		return rec
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "nobody decided within 2s") {
		t.Fatalf("status %d body %q, want the push hold's own timeout sentence at the ceiling", rec.Code, rec.Body.String())
	}
	if took < holdCeilingTest || up.gitHits != 0 {
		t.Errorf("the push hold lasted %s and the forge saw %d hits, want about %s and none", took, up.gitHits, holdCeilingTest)
	}
}

// One request chains holds: a push the rules hold, then the capability it needs. They share the
// ceiling, so the second ends when the first's time is spent, not a full ceiling later.
func TestHoldCeiling_ChainedHoldsShareOneBudget(t *testing.T) {
	const ceiling, firstDecided = 3 * time.Second, 2 * time.Second
	fastPolls(t, 5*time.Millisecond)
	p, _, _ := newEntraProxy(t, reviewSpec(60, []string{".github/workflows/**"}), "127.0.0.1:1", adoscope.CapCodeRead)
	push := newPushCP(t, types.ApprovalPending)
	push.wire(t, p)
	cap := newCapControlPlane(t)
	tok := &tokenSource{}
	tok.Set("run-token")
	inj := p.inject
	inj.base, inj.token, inj.client = cap.srv.URL, tok, cap.srv.Client()
	inj.reauth, inj.approvals = newReauthCoordinator(), &fakeApprovalReader{steps: steps(types.ApprovalPending)}
	t.Cleanup(inj.reauth.stop)
	timer := time.AfterFunc(firstDecided, func() { push.set(types.ApprovalApproved) })
	defer timer.Stop()

	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
	rec, took := serveWithin(t, 2*ceiling, func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, withHoldBudget(mustLocalReq(t, http.MethodPost,
			"/wardyn/git/dev.azure.com/acme/proj/_git/app/git-receive-pack", bytes.NewReader(body)), ceiling))
		return rec
	})
	if !strings.Contains(rec.Body.String(), adoHoldTimedOutRefusal) {
		t.Fatalf("status %d body %q, want the capability hold's own timeout sentence", rec.Code, rec.Body.String())
	}
	if raises, _ := push.counts(); raises != 1 {
		t.Errorf("the push hold raised %d approvals, want 1", raises)
	}
	if took < ceiling-100*time.Millisecond || took > ceiling+time.Second {
		t.Errorf("the chain lasted %s, want about the ceiling %s (a per-hold cap alone gives %s)", took, ceiling, firstDecided+ceiling)
	}
}

// A hold that finds the request's time spent ends at once with its own sentence, raising nothing.
func TestHoldCeiling_SpentBudgetRefusesAtOnce(t *testing.T) {
	p, _, up, cp, _ := newAppLaneHold(t, reviewSpec(60, []string{".github/workflows/**"}), types.ApprovalPending)
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
	req := withHoldBudget(mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack", bytes.NewReader(body)), time.Nanosecond)
	if _, ok := holdLeft(req.Context()); !ok {
		t.Fatal("the request carries no hold budget")
	}
	time.Sleep(time.Millisecond)
	rec := httptest.NewRecorder()
	start := time.Now()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no hold time left") || time.Since(start) > time.Second {
		t.Fatalf("status %d body %q after %s, want an at-once refusal naming the spent hold time", rec.Code, rec.Body.String(), time.Since(start))
	}
	if raises, _ := cp.counts(); raises != 0 || up.gitHits != 0 {
		t.Errorf("raises %d, forge hits %d, want none", raises, up.gitHits)
	}
}

// A sidecar's request carries no budget: its holds keep their knobs.
func TestHoldCeiling_SidecarKeepsTheKnobs(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if withHoldBudget(r, 0) != r {
		t.Error("a ceiling of 0 changed the request")
	}
	if got := holdFor(r.Context(), defaultCredentialReauthTimeout); got != defaultCredentialReauthTimeout {
		t.Errorf("a sidecar's re-auth hold is %s, want defaultCredentialReauthTimeout (%s)", got, defaultCredentialReauthTimeout)
	}
	if got := holdFor(withHoldBudget(r, holdCeilingTest).Context(), defaultCredentialReauthTimeout); got < holdCeilingTest-50*time.Millisecond || got > holdCeilingTest {
		t.Errorf("an engine's first hold is %s, want the ceiling %s", got, holdCeilingTest)
	}
	if got := holdFor(withHoldBudget(r, holdCeilingTest).Context(), time.Second); got != time.Second {
		t.Errorf("a knob under the ceiling became %s, want it kept at 1s", got)
	}
}

// The engine puts the budget on each request it serves, and the GitHub broker's mint-approval wait
// (WARDYN_GIT_APPROVAL_TIMEOUT, unclamped) is under it.
func TestHoldCeiling_EngineBoundsAMintApprovalWait(t *testing.T) {
	orig := gitApprovalPollInterval
	gitApprovalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { gitApprovalPollInterval = orig })
	t.Setenv(envGitApprovalTimeout, "1h")
	up := newOrgUpstream(t)
	org := &fakeOrg{gated: true}
	cfg := &Config{
		RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK", DecisionBufferSize: defaultBufferSize,
		Policy: types.RunPolicySpec{AllowedDomains: []string{viaOrgHost, githubHost}},
		Injection: []InjectionConfig{{GrantID: uuid.New(),
			InjectionRule: egress.InjectionRule{Host: viaOrgHost, Header: "Authorization", SecretName: "corp-secret"}}},
		MITMHosts:    []string{viaOrgHost},
		GitGrants:    map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
		TrustedCAPEM: string(up.caPEM),
	}
	e, err := NewViaOrgEngine(context.Background(), cfg, ViaOrgEngineOptions{
		Client: &http.Client{Transport: org}, Dial: redirectDial(upstreamAddr(up.srv)),
		LookupIP: publicResolver{}.LookupIP, HoldCeiling: holdCeilingTest,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	rec, took := serveWithin(t, 3*holdCeilingTest, func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		e.ServeBroker(w, mustLocalReq(t, http.MethodGet, "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack", nil))
		return w
	})
	if rec.Code == http.StatusOK || took < holdCeilingTest-100*time.Millisecond {
		t.Errorf("status %d after %s, want a refusal at about %s", rec.Code, took, holdCeilingTest)
	}
}

// The first-use hold (wait_for_review) is the first of a chain: it spends from the same budget.
func TestHoldCeiling_FirstUseHoldEndsAtTheCeiling(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	ap := newApprovalClient("https://wardynd.test", newTokenSource("tok"), uuid.New(), &http.Client{Transport: &fakeOrg{}})
	ap.configureHold(types.FirstUseWaitForReview, 60*time.Second, 4)
	ctx := withHoldBudget(httptest.NewRequest(http.MethodGet, "/", nil), holdCeilingTest).Context()
	start := time.Now()
	done := make(chan resolveResult, 1)
	go func() { done <- ap.ResolveWait(ctx, "new.example.test") }()
	select {
	case r := <-done:
		if took := time.Since(start); r.State != apPending || took < holdCeilingTest-100*time.Millisecond {
			t.Errorf("state %v after %s, want pending at about %s", r.State, took, holdCeilingTest)
		}
	case <-time.After(3 * holdCeilingTest):
		t.Fatalf("the first-use hold outlasted %s: the ceiling was not applied", 3*holdCeilingTest)
	}
}
