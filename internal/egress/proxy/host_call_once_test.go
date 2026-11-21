// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (c *hcCP) decideAllAs(s types.ApprovalState, scope types.ApprovalScope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range c.rows {
		row.state, row.decided = s, scope
	}
}

// hcOutcome is a waiter's result, or a failure once it has not answered in time.
func hcOutcome(t *testing.T, d chan hcResult) hcResult {
	t.Helper()
	select {
	case res := <-d:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting request was not answered")
		return hcResult{}
	}
}

// TestHostCallOnceAdmitsOnlyTheHeldRequest: a yes decided once lets the waiting request through, and the next
// request with the same host, method and path is held and asked about again.
func TestHostCallOnceAdmitsOnlyTheHeldRequest(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	hostCallSecond = time.Second // the window outlasts the test
	d, cancel := hcGo(p, "/admin")
	defer cancel()
	hcWait(t, "the request to ask", func() bool { rows, _, _ := cp.counts(); return rows == 1 })
	cp.decideAllAs(types.ApprovalApproved, types.ScopeOnce)
	if res := hcOutcome(t, d); !res.sent {
		t.Fatalf("the held request was refused under a once yes: %d %v", res.rec.Code, res.rec.Header())
	}
	next, stop := hcGo(p, "/admin")
	hcWait(t, "the next identical request to ask again", func() bool { rows, _, _ := cp.counts(); return rows == 2 })
	select {
	case res := <-next:
		t.Fatalf("the next identical request was answered without a person: sent=%v %d", res.sent, res.rec.Code)
	case <-time.After(50 * time.Millisecond):
	}
	stop()
	if res := hcOutcome(t, next); res.sent {
		t.Fatal("the next identical request went on under the spent yes")
	}
	if n := hcReleases(buf.String()); n != 1 {
		t.Errorf("approval-approved rows = %d, want 1", n)
	}
}

// TestHostCallOnceOneOfTwoWaiters: two identical requests waiting on one approval decided once: one goes on,
// and the other asks again.
func TestHostCallOnceOneOfTwoWaiters(t *testing.T) {
	p, _ := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	hostCallSecond = time.Second
	d1, c1 := hcGo(p, "/admin")
	d2, c2 := hcGo(p, "/admin")
	defer c1()
	defer c2()
	hcWait(t, "both requests to wait on one approval", func() bool {
		rows, raises, _ := cp.counts()
		return rows == 1 && raises >= 2
	})
	cp.decideAllAs(types.ApprovalApproved, types.ScopeOnce)
	hcWait(t, "the second request to ask again", func() bool { rows, _, _ := cp.counts(); return rows == 2 })
	var first hcResult
	var other chan hcResult
	select {
	case first = <-d1:
		other = d2
	case first = <-d2:
		other = d1
	case <-time.After(5 * time.Second):
		t.Fatal("neither waiting request went on")
	}
	if !first.sent {
		t.Fatalf("the request that took the yes was refused: %d", first.rec.Code)
	}
	c1()
	c2()
	if res := hcOutcome(t, other); res.sent {
		t.Fatal("both waiting requests went on under one once yes")
	}
}

// TestHostCallOnceDeclineRefusesBothWaiters: two identical requests waiting on one approval declined once are
// both refused, the second without asking again, and a later identical request asks again.
func TestHostCallOnceDeclineRefusesBothWaiters(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	hostCallSecond = time.Second
	d1, c1 := hcGo(p, "/admin")
	d2, c2 := hcGo(p, "/admin")
	defer c1()
	defer c2()
	hcWait(t, "both requests to wait on one approval", func() bool {
		rows, raises, _ := cp.counts()
		return rows == 1 && raises >= 2
	})
	cp.decideAllAs(types.ApprovalDenied, types.ScopeOnce)
	for i, d := range []chan hcResult{d1, d2} {
		res := hcOutcome(t, d)
		if res.sent || res.rec.Code != http.StatusForbidden {
			t.Fatalf("waiter %d was not refused under a once decline: sent=%v %d %v", i+1, res.sent, res.rec.Code, res.rec.Header())
		}
		if !strings.Contains(res.rec.Header().Get(egressHeaderDetail), "for this request only") {
			t.Errorf("waiter %d: detail = %q", i+1, res.rec.Header().Get(egressHeaderDetail))
		}
	}
	if rows, _, _ := cp.counts(); rows != 1 {
		t.Errorf("approval rows after both refusals = %d, want 1: a refused waiter must not ask again", rows)
	}
	if n := strings.Count(buf.String(), `"rule_source":"`+ruleSourceHostRuleApprovalDenied+`"`); n != 2 {
		t.Errorf("approval-denied rows = %d, want 2", n)
	}
	next, stop := hcGo(p, "/admin")
	defer stop()
	hcWait(t, "a later identical request to ask again", func() bool { rows, _, _ := cp.counts(); return rows == 2 })
	select {
	case res := <-next:
		t.Fatalf("the later request was answered without a person: sent=%v %d", res.sent, res.rec.Code)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestHostCallRunAndNoAnswers: a yes for the run is kept for the run, a no for the run refuses every later
// identical request without asking, and a no decided once refuses only the held request, so the next one asks.
func TestHostCallRunAndNoAnswers(t *testing.T) {
	for _, tc := range []struct {
		state            types.ApprovalState
		scope            types.ApprovalScope
		sent             bool
		raises, declined int
	}{
		{types.ApprovalApproved, types.ScopeRun, true, 1, 0},
		{types.ApprovalDenied, types.ScopeRun, false, 1, 2},
		{types.ApprovalDenied, types.ScopeOnce, false, 2, 2},
	} {
		t.Run(string(tc.state)+"/"+string(tc.scope), func(t *testing.T) {
			p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
			cp := newHCCP(t, p)
			cp.newState, cp.newScope = tc.state, tc.scope
			hostCallSecond = time.Second // every row is decided at its first poll
			for i := range 2 {
				rec, sent := hcSend(p, "POST", "http://"+hrHost+"/admin")
				if sent != tc.sent || (!sent && rec.Code != http.StatusForbidden) {
					t.Fatalf("request %d: sent=%v %d %v", i, sent, rec.Code, rec.Header())
				}
				once := strings.Contains(rec.Header().Get(egressHeaderDetail), "for this request only")
				if once != (tc.scope == types.ScopeOnce) {
					t.Errorf("request %d: detail = %q", i, rec.Header().Get(egressHeaderDetail))
				}
			}
			if _, raises, _ := cp.counts(); raises != tc.raises {
				t.Errorf("raises = %d, want %d", raises, tc.raises)
			}
			if n := strings.Count(buf.String(), `"rule_source":"`+ruleSourceHostRuleApprovalDenied+`"`); n != tc.declined {
				t.Errorf("approval-denied rows = %d, want %d", n, tc.declined)
			}
		})
	}
}

// TestHostCallRaiseReadsOnAnEarlierControlPlane: every raise, a rejoin included, says the proxy accepts once
// beside requested_scope, and reads cleanly as a control plane from before accepts_once reads it: the body
// decoded without DisallowUnknownFields, requested_scope with exactly its five keys and DisallowUnknownFields.
func TestHostCallRaiseReadsOnAnEarlierControlPlane(t *testing.T) {
	p, _ := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	for range 2 { // the second rejoins the first's pending approval
		if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); sent {
			t.Fatal("a held request went on with no decision")
		}
	}
	cp.mu.Lock()
	bodies := slices.Clone(cp.bodies)
	cp.mu.Unlock()
	if len(bodies) != 2 || !bytes.Contains(bodies[1], []byte(`"rejoin_id"`)) {
		t.Fatalf("raises = %q, want a raise and a rejoin", bodies)
	}
	earlierScopeKeys := []string{"host", "method", "path", "call_sha256", "hold_seconds"}
	for _, raw := range bodies {
		var earlier struct { // the earlier control plane's internalApprovalRequest
			Kind           types.ApprovalKind  `json:"kind"`
			RequestedScope json.RawMessage     `json:"requested_scope"`
			PathList       *types.PushPathList `json:"path_list"`
			RejoinID       *uuid.UUID          `json:"rejoin_id"`
		}
		if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&earlier); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if err := types.DecodeOneJSON(bytes.NewReader(raw), new(struct {
			Kind           types.ApprovalKind  `json:"kind"`
			RequestedScope json.RawMessage     `json:"requested_scope"`
			PathList       *types.PushPathList `json:"path_list"`
			RejoinID       *uuid.UUID          `json:"rejoin_id"`
		}), false); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(earlier.RequestedScope, &keys); err != nil {
			t.Fatal(err)
		}
		for k := range keys {
			if !slices.Contains(earlierScopeKeys, k) {
				t.Errorf("requested_scope carries %q, which an earlier control plane refuses: %s", k, earlier.RequestedScope)
			}
		}
		var sc struct {
			Host        string `json:"host"`
			Method      string `json:"method"`
			Path        string `json:"path"`
			CallSHA256  string `json:"call_sha256"`
			HoldSeconds int    `json:"hold_seconds"`
		}
		dec := json.NewDecoder(bytes.NewReader(earlier.RequestedScope))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sc); err != nil || types.HostCallKey(sc.Host, sc.Method, sc.Path) != sc.CallSHA256 {
			t.Errorf("requested_scope %s: %v", earlier.RequestedScope, err)
		}
		var says struct {
			AcceptsOnce bool `json:"accepts_once"`
		}
		if json.Unmarshal(raw, &says) != nil || !says.AcceptsOnce || earlier.Kind != types.ApprovalToolCall {
			t.Errorf("%s: want a tool_call raise with accepts_once beside requested_scope", raw)
		}
	}
}

// hcLateRaise holds the first raise's answer until a second raise has reached the control plane, so both
// requests ask before either files the approval, and holds the second raise's answer until late is closed.
type hcLateRaise struct {
	cp    *hcCP
	next  http.RoundTripper
	late  chan struct{}
	posts atomic.Int32
}

func (l *hcLateRaise) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(r)
	if r.Method != http.MethodPost {
		return resp, err
	}
	switch l.posts.Add(1) {
	case 1:
		for _, raises, _ := l.cp.counts(); raises < 2 && r.Context().Err() == nil; _, raises, _ = l.cp.counts() {
			time.Sleep(time.Millisecond)
		}
	case 2:
		select {
		case <-l.late:
		case <-r.Context().Done(): // the request ended first
		}
	}
	return resp, err
}

// hcRowIDs is the approval each decision row with rule_source source names, in order: uuid.Nil for a row
// that names none.
func hcRowIDs(t *testing.T, buf *bytes.Buffer, source string) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var row struct {
			RuleSource string     `json:"rule_source"`
			ApprovalID *uuid.UUID `json:"approval_id"`
		}
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decode decision %q: %v", line, err)
		}
		if row.RuleSource == source {
			ids = append(ids, uuid.Nil)
			if row.ApprovalID != nil {
				ids[len(ids)-1] = *row.ApprovalID
			}
		}
	}
	return ids
}

// row is the id of the control plane's i-th approval row.
func (c *hcCP) row(i int) uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rows[i].id
}

// TestHostCallOnceAnswersARaiseThatJoinedLate: of two identical requests that both asked, the one whose
// raise was answered only after a person decided the shared approval once, and after the other request
// spent that answer, still takes it: a once no refuses it as declined on that approval, and a once yes
// already taken makes it ask again on a new one. Neither is answered that Wardyn could not ask a person,
// and a later identical request asks again on a new approval.
func TestHostCallOnceAnswersARaiseThatJoinedLate(t *testing.T) {
	scope, _ := hostCallScopeOf(hrHost, "POST", "/admin")
	for _, state := range []types.ApprovalState{types.ApprovalDenied, types.ApprovalApproved} {
		t.Run(string(state), func(t *testing.T) {
			p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
			cp := newHCCP(t, p)
			hostCallSecond = time.Second
			late := &hcLateRaise{cp: cp, next: cp.srv.Client().Transport, late: make(chan struct{})}
			p.approval = newApprovalClient(cp.srv.URL, newTokenSource("tok"), p.runID, &http.Client{Transport: late})
			d1, c1 := hcGo(p, "/admin")
			d2, c2 := hcGo(p, "/admin")
			defer c1()
			defer c2()
			hcWait(t, "both requests to ask", func() bool { rows, raises, _ := cp.counts(); return rows == 1 && raises == 2 })
			shared := cp.row(0)
			cp.decideAllAs(state, types.ScopeOnce)
			var first hcResult
			var other chan hcResult
			select {
			case first = <-d1:
				other = d2
			case first = <-d2:
				other = d1
			case <-time.After(5 * time.Second):
				t.Fatal("the request that filed the approval was not answered")
			}
			if first.sent != (state == types.ApprovalApproved) {
				t.Fatalf("the request that filed the approval: sent=%v %d %v", first.sent, first.rec.Code, first.rec.Header())
			}
			close(late.late)
			if state == types.ApprovalApproved {
				hcWait(t, "the late request to ask again", func() bool { rows, _, _ := cp.counts(); return rows == 2 })
				asked := cp.row(1)
				cp.mu.Lock()
				cp.rows[1].state, cp.rows[1].decided = types.ApprovalDenied, types.ScopeOnce
				cp.mu.Unlock()
				res := hcOutcome(t, other)
				if res.sent || res.rec.Code != http.StatusForbidden || !strings.Contains(res.rec.Header().Get(egressHeaderDetail), "for this request only") {
					t.Fatalf("the late request: sent=%v %d detail=%q", res.sent, res.rec.Code, res.rec.Header().Get(egressHeaderDetail))
				}
				if got := hcRowIDs(t, buf, ruleSourceHostRuleApprovalApproved); !slices.Equal(got, []uuid.UUID{shared}) {
					t.Errorf("approval-approved rows name %v, want one send, on %v", got, shared)
				}
				if got := hcRowIDs(t, buf, ruleSourceHostRuleApprovalDenied); !slices.Equal(got, []uuid.UUID{asked}) {
					t.Errorf("approval-denied rows name %v, want the late request's own ask %v", got, asked)
				}
				return
			}
			res := hcOutcome(t, other)
			if res.sent || res.rec.Code != http.StatusForbidden || !strings.Contains(res.rec.Header().Get(egressHeaderDetail), "for this request only") {
				t.Fatalf("the late request: sent=%v %d detail=%q", res.sent, res.rec.Code, res.rec.Header().Get(egressHeaderDetail))
			}
			if rows, _, _ := cp.counts(); rows != 1 {
				t.Errorf("approval rows = %d, want 1: the late request must not ask again", rows)
			}
			if got := hcRowIDs(t, buf, ruleSourceHostRuleApprovalDenied); !slices.Equal(got, []uuid.UUID{shared, shared}) {
				t.Errorf("approval-denied rows name %v, want two, both on %v", got, shared)
			}
			next, stop := hcGo(p, "/admin")
			defer stop()
			hcWait(t, "a later identical request to ask again", func() bool { rows, _, _ := cp.counts(); return rows == 2 })
			hcWait(t, "the later request to wait on the new approval", func() bool {
				id, _ := p.hostCalls.look(scope.CallSHA256)
				return id == cp.row(1)
			})
			if cp.row(1) == shared {
				t.Fatalf("the later request waits on the declined approval %v", shared)
			}
			select {
			case res := <-next:
				t.Fatalf("the later request was answered without a person: sent=%v %d", res.sent, res.rec.Code)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// TestHostCallLostYesAskedAgainLoopsNot: a request that lost a once yes to another identical request asks
// again. A control plane that answers that ask with the same spent yes has it refused at once, as a request
// Wardyn could not ask about, never raising in a loop.
func TestHostCallLostYesAskedAgainLoopsNot(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	spent := uuid.New()
	var raises atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			raises.Add(1)
		}
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: spent, State: types.ApprovalApproved, DecisionScope: types.ScopeOnce})
	}))
	t.Cleanup(srv.Close)
	p.approval = newApprovalClient(srv.URL, newTokenSource("tok"), p.runID, srv.Client())
	prev := hostCallSecond
	hostCallSecond = time.Second
	t.Cleanup(func() { hostCallSecond = prev })
	// Another identical request is still waiting, and has used the yes.
	scope, _ := hostCallScopeOf(hrHost, "POST", "/admin")
	p.hostCalls.admitKept(scope.CallSHA256)
	defer p.hostCalls.leave(scope.CallSHA256)
	p.hostCalls.install(scope.CallSHA256, spent)
	p.hostCalls.claim(scope.CallSHA256, spent)
	d, cancel := hcGo(p, "/admin")
	defer cancel()
	res := hcOutcome(t, d)
	if res.sent || res.rec.Code != http.StatusForbidden || res.rec.Header().Get(egressHeaderDetail) != hostCallNoAskText {
		t.Fatalf("sent=%v %d detail=%q", res.sent, res.rec.Code, res.rec.Header().Get(egressHeaderDetail))
	}
	if n := raises.Load(); n != 2 {
		t.Errorf("raises = %d, want 2: the first ask, and the one answered with the lost yes", n)
	}
	if got := hcRowIDs(t, buf, ruleSourceHostRuleApprovalRefused); !slices.Equal(got, []uuid.UUID{uuid.Nil}) {
		t.Errorf("approval-refused rows name %v, want one naming none", got)
	}
}
