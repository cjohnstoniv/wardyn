// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Azure DevOps capability HOLD, from the sandbox's side: a write beyond the
// grant is parked on the SAME connection while a person decides, and forwarded
// or refused by the answer.

const adoWorkItemPatch = `[{"op":"add","path":"/fields/System.Title","value":"x"}]`

// capControlPlane is a control plane that answers the capability ask the way
// answerADOCapability does, scripted: a FIRST ask (no approval named) raises a
// request; a re-resolve naming an approval answers with the capability set.
type capControlPlane struct {
	srv *httptest.Server
	mu  sync.Mutex
	// asks is every resolve's query, in order.
	asks []url.Values
	// refuse, when set, answers every first ask 403 with this sentence.
	refuse string
	// sameID answers every first ask with ONE approval id (the server's dedup).
	sameID uuid.UUID
	// raised is every approval id a first ask answered with.
	raised []uuid.UUID
	// forRun makes a re-resolve answer as if approved for the run.
	forRun bool
	// consent is answered to the FIRST re-resolve, as a reauth_pending 423.
	consent    uuid.UUID
	reResolves int
	// failFirstReResolve answers the first re-resolve 503 (a transient failure).
	failFirstReResolve bool
	// firstAskOK answers a first ask 200 WITHOUT the capability — an older
	// control plane that ignores the ask.
	firstAskOK bool
}

func newCapControlPlane(t *testing.T) *capControlPlane {
	t.Helper()
	cp := &capControlPlane{}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		q := r.URL.Query()
		cp.asks = append(cp.asks, q)
		w.Header().Set("Content-Type", "application/json")
		if q.Get("approval") == "" {
			if cp.firstAskOK {
				_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
					Header: "Authorization", Value: "Bearer " + adoToken, Capabilities: []string{"read"},
				})
				return
			}
			if cp.refuse != "" {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": cp.refuse})
				return
			}
			id := cp.sameID
			if id == uuid.Nil {
				id = uuid.New()
			}
			cp.raised = append(cp.raised, id)
			w.WriteHeader(http.StatusLocked)
			_ = json.NewEncoder(w).Encode(map[string]string{"state": capabilityPendingState, "approval_id": id.String()})
			return
		}
		cp.reResolves++
		if cp.failFirstReResolve && cp.reResolves == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if cp.consent != uuid.Nil && cp.reResolves == 1 {
			w.WriteHeader(http.StatusLocked)
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "reauth_pending", "approval_id": cp.consent.String()})
			return
		}
		caps := []string{"read"}
		if cp.forRun {
			caps = append(caps, q.Get("capability"))
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: "Bearer " + adoToken, Capabilities: caps,
		})
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (cp *capControlPlane) snapshot() ([]url.Values, []uuid.UUID) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]url.Values(nil), cp.asks...), append([]uuid.UUID(nil), cp.raised...)
}

// newADOHoldHarness is newADOHarness (a read-only grant) with the injector's
// hold lane pointed at cp and reader.
func newADOHoldHarness(t *testing.T, cp *capControlPlane, reader approvalReader) *adoHarness {
	t.Helper()
	fastPolls(t, 5*time.Millisecond)
	h := newADOHarness(t, adoscope.CapRead)
	tok := &tokenSource{}
	tok.Set("run-token")
	inj := h.p.inject
	inj.base, inj.token, inj.client = cp.srv.URL, tok, cp.srv.Client()
	inj.reauth, inj.approvals = newReauthCoordinator(), reader
	t.Cleanup(inj.reauth.stop)
	return h
}

func (h *adoHarness) patchWorkItem(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1?api-version=7.1", adoWorkItemPatch, nil)
}

func steps(s ...types.ApprovalState) []approvalStep {
	out := make([]approvalStep, 0, len(s))
	for _, st := range s {
		out = append(out, approvalStep{state: st, status: http.StatusOK})
	}
	return out
}

// A write beyond the grant under wait_for_review is HELD, approved once, goes
// through ONCE — and the next attempt raises a NEW approval.
func TestADOHold_ApprovedOnceGoesThroughOnceAndTheNextAsksAgain(t *testing.T) {
	cp := newCapControlPlane(t)
	reader := &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalPending, types.ApprovalApproved)}
	h := newADOHoldHarness(t, cp, reader)

	if rec := h.patchWorkItem(t); rec.Code != http.StatusOK {
		t.Fatalf("held write: status %d body %s, want it forwarded once approved", rec.Code, rec.Body.String())
	}
	if n := len(h.fake.Requests()); n != 1 {
		t.Fatalf("upstream saw %d requests, want exactly the one approved write", n)
	}
	if reader.count() < 3 {
		t.Errorf("the request went through after %d approval reads; it was never held", reader.count())
	}
	asks, raised := cp.snapshot()
	if len(asks) != 2 || asks[0].Get("capability") != string(adoscope.CapWorkWrite) || asks[0].Get("approval") != "" ||
		asks[1].Get("approval") != raised[0].String() {
		t.Fatalf("control plane saw %v, want one ask for work_write then one re-resolve naming %s", asks, raised[0])
	}
	if asks[0].Get("path") != "/acme/proj/_apis/wit/workitems/1" || asks[0].Get("method") != http.MethodPatch {
		t.Errorf("the ask did not carry the request for the card: %v", asks[0])
	}

	// The second attempt is a new question: the once approval does not widen
	// the run.
	if rec := h.patchWorkItem(t); rec.Code != http.StatusOK {
		t.Fatalf("second write: status %d body %s", rec.Code, rec.Body.String())
	}
	asks, raised = cp.snapshot()
	if len(raised) != 2 || raised[0] == raised[1] || asks[2].Get("approval") != "" {
		t.Fatalf("second attempt did not raise a new approval: asks=%v raised=%v", asks, raised)
	}
}

// Approve-for-this-run widens the run: the next request needs no second ask.
func TestADOHold_ApprovedForTheRunWidensTheRun(t *testing.T) {
	cp := newCapControlPlane(t)
	cp.forRun = true
	h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalApproved)})

	for i := 0; i < 3; i++ {
		if rec := h.patchWorkItem(t); rec.Code != http.StatusOK {
			t.Fatalf("write %d: status %d body %s", i, rec.Code, rec.Body.String())
		}
	}
	if asks, _ := cp.snapshot(); len(asks) != 2 {
		t.Errorf("control plane saw %d calls, want 2 (one ask, one re-resolve) — the run was not widened", len(asks))
	}
	if n := len(h.fake.Requests()); n != 3 {
		t.Errorf("upstream saw %d, want 3", n)
	}
}

// Deny ends the hold in Azure DevOps' own shape, 403, and nothing is forwarded.
func TestADOHold_DenyIsAnAzureDevOpsShaped403(t *testing.T) {
	cp := newCapControlPlane(t)
	h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalDenied)})
	h.mustRefuse(t, h.patchWorkItem(t), "it was not approved")
	if asks, _ := cp.snapshot(); len(asks) != 1 {
		t.Errorf("a denied hold re-resolved: %v", asks)
	}
}

// Above the ceiling and always_deny are the CONTROL PLANE's refusals: relayed
// in Azure DevOps' shape, with no hold and no poll.
func TestADOHold_ControlPlaneRefusalIsRelayedWithoutAHold(t *testing.T) {
	const sentence = "Wardyn refused this Azure DevOps request: it needs \"Write work items\", which is outside what an administrator allows for this organisation."
	cp := newCapControlPlane(t)
	cp.refuse = sentence
	reader := &fakeApprovalReader{steps: steps(types.ApprovalApproved)}
	h := newADOHoldHarness(t, cp, reader)
	h.mustRefuse(t, h.patchWorkItem(t), "outside what an administrator allows")
	if reader.count() != 0 || h.p.inject.reauth.capCounted != 0 {
		t.Errorf("a refusal was held: reads=%d workflows=%d", reader.count(), h.p.inject.reauth.capCounted)
	}
}

// The proxy's own budget: once spent, no further hold is opened — and it is a
// SEPARATE budget from the sign-in one, in both directions.
func TestADOHold_BudgetIsItsOwn(t *testing.T) {
	cp := newCapControlPlane(t)
	h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalApproved)})
	coord := h.p.inject.reauth
	coord.counted = maxReauthHolds // the sign-in budget spent does not block an escalation
	if _, _, ok := coord.admitCapability(uuid.New(), url.Values{}); !ok {
		t.Fatal("a spent sign-in budget refused a capability hold")
	}
	coord.capCounted = maxCapabilityHolds
	h.mustRefuse(t, h.patchWorkItem(t), "too many")
	coord.counted = 0
	if _, _, ok := coord.admit(uuid.New(), time.Second); !ok {
		t.Fatal("a spent capability budget refused a sign-in hold")
	}
}

// ONE `once` approval, many parked requests: exactly one is forwarded.
func TestADOHold_OnceIsOneRequestUnderConcurrency(t *testing.T) {
	cp := newCapControlPlane(t)
	cp.sameID = uuid.New() // the control plane dedups them onto one request
	reader := &fakeApprovalReader{steps: append(pending(10), approvalStep{state: types.ApprovalApproved, status: http.StatusOK})}
	h := newADOHoldHarness(t, cp, reader)

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = h.patchWorkItem(t).Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 || len(h.fake.Requests()) != 1 {
		t.Fatalf("codes=%v upstream=%d, want exactly one request through on one once approval", codes, len(h.fake.Requests()))
	}
}

// A re-resolve that answers a CONSENT request is chained: the hold polls it,
// and re-resolves the same escalation once the person has signed in.
func TestADOHold_ChainsOntoAConsentRequest(t *testing.T) {
	cp := newCapControlPlane(t)
	cp.consent = uuid.New()
	reader := &fakeApprovalReader{steps: steps(types.ApprovalApproved)}
	h := newADOHoldHarness(t, cp, reader)
	if rec := h.patchWorkItem(t); rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want forwarded after consent", rec.Code, rec.Body.String())
	}
	asks, raised := cp.snapshot()
	if len(asks) != 3 || asks[1].Get("approval") != raised[0].String() || asks[2].Get("approval") != raised[0].String() {
		t.Fatalf("asks = %v, want both re-resolves to name the escalation %s", asks, raised[0])
	}
	reader.mu.Lock()
	last := reader.lastID
	reader.mu.Unlock()
	if last != cp.consent {
		t.Errorf("the hold last polled %s, want the consent request %s", last, cp.consent)
	}
}

// The hold is clamped under the MITM server's 5-minute read timeout.
func TestCapabilityHoldBudget_ClampedBelowTheReadTimeout(t *testing.T) {
	t.Setenv(envCredentialReauthTimeout, "")
	if got := capabilityHoldBudget(); got != maxCapabilityHoldTimeout || got > 240*time.Second {
		t.Errorf("default = %v, want %v", got, maxCapabilityHoldTimeout)
	}
	t.Setenv(envCredentialReauthTimeout, "30m")
	if got := capabilityHoldBudget(); got != 240*time.Second {
		t.Errorf("30m = %v, want the 240s clamp", got)
	}
	t.Setenv(envCredentialReauthTimeout, "30s")
	if got := capabilityHoldBudget(); got != 30*time.Second {
		t.Errorf("30s = %v, want 30s", got)
	}
}

// With no hold lane the gate refuses exactly as before.
func TestADOHold_NoHoldLaneRefusesAsBefore(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead)
	h.mustRefuse(t, h.patchWorkItem(t), "this run was not granted it")
}

// The sandbox's approval route refuses a `lane` key, top-level or in the
// payload, in any letter case — and never reaches the control plane.
func TestLocalApprovalRoute_RefusesALaneKey(t *testing.T) {
	var hits int
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	t.Cleanup(cp.Close)
	p, _ := newLocalRouteProxy(t, cp.URL, "RUNTOK", "127.0.0.1:1", nil, nil)
	for _, body := range []string{
		`{"kind":"tool_call","lane":"azure_devops","payload":{"tool":"az","cmd":"x"}}`,
		`{"kind":"tool_call","payload":{"tool":"az","cmd":"x","Lane":"azure_devops"}}`,
		`{"kind":"tool_call","LANE":"azure_devops","payload":{"tool":"az","cmd":"x"}}`,
	} {
		rec := httptest.NewRecorder()
		p.handleBrokerCreateApproval(rec, mustLocalReq(t, http.MethodPost, routeApprovalsCreate, strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "lane") {
			t.Errorf("%s: status %d body %q, want a 400 naming the lane", body, rec.Code, rec.Body.String())
		}
	}
	if hits != 0 {
		t.Errorf("control plane was reached %d times", hits)
	}
}

// A capability hold that ran out with its request still open does not stick:
// the retry waits again (and is counted). The sign-in hold keeps its sticky
// terminal result.
func TestADOHold_ATimedOutHoldIsNotStickyForTheRetry(t *testing.T) {
	coord := newReauthCoordinator()
	id := uuid.New()
	expire := func(wf *reauthWorkflow) {
		wf.err = errReauthTimedOut
		close(wf.done)
	}
	wf, fresh, ok := coord.admitCapability(id, url.Values{})
	if !fresh || !ok {
		t.Fatal("first admit")
	}
	expire(wf)
	again, fresh, ok := coord.admitCapability(id, url.Values{})
	if !ok || !fresh || again == wf || coord.capCounted != 2 {
		t.Fatalf("retry after a timeout joined the dead hold (fresh=%v counted=%d)", fresh, coord.capCounted)
	}

	sign := uuid.New()
	swf, _, _ := coord.admit(sign, time.Second)
	expire(swf)
	if same, fresh, _ := coord.admit(sign, time.Second); fresh || same != swf {
		t.Fatal("the sign-in hold lost its sticky terminal result")
	}
}

// F1: a transient failure of the one re-resolve must not wedge an approved,
// unspent `once`. The control plane keeps naming that approval to every retry;
// the retry waits again and goes through once.
func TestADOHold_AFailedReResolveDoesNotWedgeTheApproval(t *testing.T) {
	cp := newCapControlPlane(t)
	cp.sameID = uuid.New()
	cp.failFirstReResolve = true
	h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalApproved)})

	h.mustRefuse(t, h.patchWorkItem(t), "ended without one")
	if rec := h.patchWorkItem(t); rec.Code != http.StatusOK {
		t.Fatalf("retry: status %d body %s, want it to wait again and go through", rec.Code, rec.Body.String())
	}
	if n := len(h.fake.Requests()); n != 1 {
		t.Errorf("upstream saw %d, want exactly one", n)
	}
	if cp.reResolves != 2 {
		t.Errorf("re-resolves = %d, want 2 (the failed one and the retry's)", cp.reResolves)
	}
}

// F3: a 200 to a first ask that does not name the capability is an older
// control plane ignoring the ask — refused, never forwarded.
func TestADOHold_AFirstAskAnsweredWithoutTheCapabilityIsRefused(t *testing.T) {
	cp := newCapControlPlane(t)
	cp.firstAskOK = true
	h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalApproved)})
	h.mustRefuse(t, h.patchWorkItem(t), "this run was not granted it")
	if h.p.inject.reauth.holds(adoscope.CapWorkWrite) {
		t.Error("the run was widened to a capability nobody granted")
	}
}
