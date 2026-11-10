// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type hcRow struct {
	id    uuid.UUID
	scope types.HostCallScope
	state types.ApprovalState
}

// hcCP is a control plane for host rule holds: it checks every raised scope as the real route does
// (exact keys, Validate, the key recomputed), folds a raise into the PENDING row with the same key, and
// answers polls from its rows.
type hcCP struct {
	srv *httptest.Server

	mu         sync.Mutex
	rows       []*hcRow
	raises     int
	polls      int
	newState   types.ApprovalState // the state a new row starts in; PENDING when empty
	refuse     int                 // when set, every raise is answered with this status
	pollStatus int                 // when set, every poll is answered with this status
	badScopes  int
}

func newHCCP(t *testing.T, p *Proxy) *hcCP {
	t.Helper()
	c := &hcCP{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/internal/approvals" {
			c.raises++
			var body struct {
				Kind           types.ApprovalKind `json:"kind"`
				RequestedScope json.RawMessage    `json:"requested_scope"`
				RejoinID       *uuid.UUID         `json:"rejoin_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var scope types.HostCallScope
			dec := json.NewDecoder(bytes.NewReader(body.RequestedScope))
			dec.DisallowUnknownFields()
			if body.Kind != types.ApprovalToolCall || dec.Decode(&scope) != nil || scope.Validate() != nil ||
				scope.ComputeKey() != scope.CallSHA256 || !types.IsHostCallScope(body.RequestedScope) {
				c.badScopes++
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			if c.refuse != 0 {
				http.Error(w, "refused", c.refuse)
				return
			}
			if body.RejoinID != nil {
				for _, row := range c.rows {
					if row.id == *body.RejoinID && row.state == types.ApprovalPending {
						_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
						return
					}
				}
				http.Error(w, "no pending approval to rejoin", http.StatusNotFound)
				return
			}
			for _, row := range c.rows {
				if row.state == types.ApprovalPending && row.scope.CallSHA256 == scope.CallSHA256 {
					_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
					return
				}
			}
			row := &hcRow{id: uuid.New(), scope: scope, state: cmpState(c.newState)}
			c.rows = append(c.rows, row)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: types.ApprovalPending})
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/internal/approvals/") {
			c.polls++
			if c.pollStatus != 0 {
				http.Error(w, "unreadable", c.pollStatus)
				return
			}
			for _, row := range c.rows {
				if r.URL.Path == "/api/v1/internal/approvals/"+row.id.String() {
					_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
					return
				}
			}
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(c.srv.Close)
	p.approval = newApprovalClient(c.srv.URL, newTokenSource("tok"), p.runID, c.srv.Client())
	prevPoll, prevSecond := holdPollInterval, hostCallSecond
	holdPollInterval, hostCallSecond = 2*time.Millisecond, time.Millisecond
	t.Cleanup(func() { holdPollInterval, hostCallSecond = prevPoll, prevSecond })
	return c
}

func (c *hcCP) decideAll(s types.ApprovalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range c.rows {
		row.state = s
	}
}

func (c *hcCP) counts() (rows, raises, polls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.rows), c.raises, c.polls
}

// hcHoldSet holds POST under /admin on hrHost and allows everything else there.
func hcHoldSet() types.HostRuleSet {
	return types.HostRuleSet{Rules: []types.HostRule{
		{Host: hrHost, Methods: []string{"POST"}, Paths: []string{"/admin"}, Effect: types.HostRuleHold},
		{Host: hrHost, Effect: types.HostRuleAllow},
	}}
}

// hcProxy is a proxy for spec (allowed_domains hrHost added) with a decision buffer.
func hcProxy(t *testing.T, spec types.RunPolicySpec) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	spec.AllowedDomains = append(spec.AllowedDomains, hrHost)
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(spec), Sink: &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{}})
	return p, buf
}

// hcSend judges one plain request through the host rules; sent reports that the request went on.
func hcSend(p *Proxy, method, target string) (rec *httptest.ResponseRecorder, sent bool) {
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(method, target, strings.NewReader(`{"a":1}`))
	return rec, !p.hostRuleRefused(rec, r, hrHost, 80)
}

// TestHostCallHoldWaitsThenAllowsForTheRun: a held request waits and is answered pending, never sent; once
// a person approves, every request with that host, method and path goes on for the rest of the run, any
// query or body included, with no new ask; another path asks again.
func TestHostCallHoldWaitsThenAllowsForTheRun(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	rec, sent := hcSend(p, "POST", "http://"+hrHost+"/x/../admin")
	if sent || rec.Code != http.StatusForbidden || rec.Header().Get(egressHeaderStatus) != egressRefusalPending ||
		!strings.Contains(rec.Body.String(), `"wardyn":"approval-pending"`) {
		t.Fatalf("held request: sent=%v %d %v %s", sent, rec.Code, rec.Header(), rec.Body)
	}
	if !strings.Contains(rec.Header().Get(egressHeaderDetail), "POST /admin on "+hrHost) {
		t.Errorf("detail = %q", rec.Header().Get(egressHeaderDetail))
	}
	if !strings.Contains(buf.String(), `"rule_source":"`+ruleSourceHostRuleApprovalWait+`"`) {
		t.Errorf("no wait row:\n%s", buf)
	}
	cp.mu.Lock()
	got := cp.rows[0].scope
	cp.mu.Unlock()
	want := types.HostCallScope{Host: hrHost, Method: "POST", Path: "/admin", HoldSeconds: hostCallWindow}
	want.CallSHA256 = want.ComputeKey()
	if got != want {
		t.Fatalf("raised scope = %+v, want %+v", got, want)
	}

	cp.decideAll(types.ApprovalApproved)
	for _, target := range []string{"/admin", "/admin?x=1", "//admin"} {
		if rec, sent := hcSend(p, "POST", "http://"+hrHost+target); !sent {
			t.Fatalf("approved POST %s refused: %d %s", target, rec.Code, rec.Body)
		}
	}
	rows, raises, _ := cp.counts()
	if rows != 1 || cp.badScopes != 0 {
		t.Fatalf("rows=%d bad=%d, want one row", rows, cp.badScopes)
	}
	_, _, polls := cp.counts()
	if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); !sent {
		t.Fatal("approved request refused")
	}
	if r2, ra2, p2 := cp.counts(); r2 != rows || ra2 != raises || p2 != polls {
		t.Errorf("an approved call asked again: rows %d→%d raises %d→%d polls %d→%d", rows, r2, raises, ra2, polls, p2)
	}
	if rec, sent := hcSend(p, "POST", "http://"+hrHost+"/admin/users"); sent || rec.Code != http.StatusForbidden {
		t.Fatalf("another path went on with the first path's approval: %d", rec.Code)
	}
	if rows, _, _ := cp.counts(); rows != 2 {
		t.Errorf("another path did not ask: rows=%d", rows)
	}
}

// TestHostCallDeclinedRefusedForTheRun: a declined call is refused, and refused again later in the run
// without asking.
func TestHostCallDeclinedRefusedForTheRun(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	cp.newState = types.ApprovalDenied
	for i := range 2 {
		rec, sent := hcSend(p, "POST", "http://"+hrHost+"/admin")
		if sent || rec.Code != http.StatusForbidden || rec.Header().Get(egressHeaderStatus) != egressRefusalDenied {
			t.Fatalf("declined #%d: sent=%v %d %v", i, sent, rec.Code, rec.Header())
		}
	}
	if rows, raises, _ := cp.counts(); rows != 1 || raises != 1 {
		t.Errorf("rows=%d raises=%d, want one ask", rows, raises)
	}
	if strings.Count(buf.String(), `"rule_source":"`+ruleSourceHostRuleApprovalDenied+`"`) != 2 {
		t.Errorf("rows:\n%s", buf)
	}
}

// TestHostCallHoldNeverOverridesADeny: a set that holds a request never lets it past another set that
// denies it (or names the host and matches nothing), in either order, and an approval already given does
// not either: the rules are judged afresh on every request.
func TestHostCallHoldNeverOverridesADeny(t *testing.T) {
	denyAdmin := types.HostRuleSet{Rules: []types.HostRule{{Host: hrHost, Paths: []string{"/admin"}, Effect: types.HostRuleDeny},
		{Host: hrHost, Effect: types.HostRuleAllow}}}
	onlyGET := types.HostRuleSet{Rules: []types.HostRule{{Host: hrHost, Methods: []string{"GET"}, Effect: types.HostRuleAllow}}}
	for name, sets := range map[string][]types.HostRuleSet{
		"hold, deny":     {hcHoldSet(), denyAdmin},
		"deny, hold":     {denyAdmin, hcHoldSet()},
		"hold, no match": {hcHoldSet(), onlyGET},
		"no match, hold": {onlyGET, hcHoldSet()},
	} {
		p, _ := hcProxy(t, types.RunPolicySpec{HostRules: sets})
		cp := newHCCP(t, p)
		cp.newState = types.ApprovalApproved
		if rec, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); sent || rec.Code != http.StatusForbidden {
			t.Errorf("%s: the hold let a denied request through: %d", name, rec.Code)
		}
		if rows, raises, _ := cp.counts(); rows != 0 || raises != 0 {
			t.Errorf("%s: a denied request asked a person (rows=%d raises=%d)", name, rows, raises)
		}
	}

	p, _ := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	cp.newState = types.ApprovalApproved
	if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); !sent {
		t.Fatal("approved hold refused")
	}
	p.policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{hrHost}, HostRules: []types.HostRuleSet{hcHoldSet(), denyAdmin}})
	if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); sent {
		t.Fatal("an earlier approval let a request through a set that now denies it")
	}
}

// TestHostCallFailsClosed: a raise the control plane refuses or cannot take, a proxy with no way to ask,
// an answer that cannot be read, and a method allowed_methods refuses: the request is never sent.
func TestHostCallFailsClosed(t *testing.T) {
	t.Run("raise refused", func(t *testing.T) {
		p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
		cp := newHCCP(t, p)
		cp.refuse = http.StatusTooManyRequests
		if rec, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); sent || rec.Code != http.StatusForbidden {
			t.Fatalf("sent=%v %d", sent, rec.Code)
		}
		if !strings.Contains(buf.String(), `"rule_source":"`+ruleSourceHostRuleApprovalRefused+`"`) {
			t.Errorf("rows:\n%s", buf)
		}
	})
	t.Run("raise fails", func(t *testing.T) {
		p, _ := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
		cp := newHCCP(t, p)
		cp.srv.Close()
		if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); sent {
			t.Fatal("sent with no control plane")
		}
	})
	t.Run("no way to ask", func(t *testing.T) {
		p, _ := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
		if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); sent {
			t.Fatal("sent with no approval client")
		}
	})
	t.Run("answer unreadable", func(t *testing.T) {
		p, _ := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
		cp := newHCCP(t, p)
		cp.newState = types.ApprovalApproved
		cp.pollStatus = http.StatusInternalServerError
		rec, sent := hcSend(p, "POST", "http://"+hrHost+"/admin")
		if sent || rec.Header().Get(egressHeaderStatus) != egressRefusalPending {
			t.Fatalf("sent=%v %v", sent, rec.Header())
		}
	})
	t.Run("method refused", func(t *testing.T) {
		p, _ := hcProxy(t, types.RunPolicySpec{AllowedMethods: []string{"GET"}, HostRules: []types.HostRuleSet{hcHoldSet()}})
		cp := newHCCP(t, p)
		if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); sent {
			t.Fatal("sent a method allowed_methods refuses")
		}
		if _, raises, _ := cp.counts(); raises != 0 {
			t.Errorf("asked a person about a method the run refuses: raises=%d", raises)
		}
	})
}

// TestHostCallClosedAsksAgain: an approval that closed without a decision is forgotten, so the next
// request asks again.
func TestHostCallClosedAsksAgain(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	cp.newState = types.ApprovalExpired
	if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); sent {
		t.Fatal("sent on a closed approval")
	}
	if !strings.Contains(buf.String(), `"rule_source":"`+ruleSourceHostRuleApprovalClosed+`"`) {
		t.Errorf("rows:\n%s", buf)
	}
	cp.newState = types.ApprovalApproved
	if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); !sent {
		t.Fatal("the ask after a closed approval was not answered")
	}
	if rows, _, _ := cp.counts(); rows != 2 {
		t.Errorf("rows=%d, want a second ask", rows)
	}
}

// TestHostCallHoldInsideTheTunnel: inside an opened tunnel a held request reaches the upstream only once a
// person approved it, and a declined one never does.
func TestHostCallHoldInsideTheTunnel(t *testing.T) {
	for _, tc := range []struct {
		state types.ApprovalState
		want  int
	}{
		{types.ApprovalApproved, http.StatusOK},
		{types.ApprovalDenied, http.StatusForbidden},
	} {
		cu := captureUpstream(t, true, "ok")
		p, _, caPEM := hrProxy(t, hrOpts{}, cu.srv)
		p.policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{hrHost}, HostRules: []types.HostRuleSet{hcHoldSet()}})
		cp := newHCCP(t, p)
		cp.newState = tc.state
		srv := httptest.NewServer(p)
		status, conn, _ := hrConnect(t, srv.URL, "443")
		if status != http.StatusOK {
			t.Fatalf("CONNECT = %d", status)
		}
		got := hrInner(t, conn, caPEM, "POST", "/admin")
		srv.Close()
		if got != tc.want || cu.reached != (tc.want == http.StatusOK) {
			t.Errorf("%s: inner POST /admin = %d reached=%v, want %d", tc.state, got, cu.reached, tc.want)
		}
	}
}

// hcResult is one held request's outcome, from a goroutine.
type hcResult struct {
	rec  *httptest.ResponseRecorder
	sent bool
}

// hcGo sends one POST to path on its own context in a goroutine; cancel ends it as a client that went away.
func hcGo(p *Proxy, path string) (done chan hcResult, cancel context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan hcResult, 1)
	go func() {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "http://"+hrHost+path, nil).WithContext(ctx)
		done <- hcResult{rec, !p.hostRuleRefused(rec, r, hrHost, 80)}
	}()
	return done, cancel
}

func hcWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (c *hcCP) decidePath(path string, s types.ApprovalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range c.rows {
		if row.scope.Path == path {
			row.state = s
		}
	}
}

// hcBusy reports whether rec is the at-once refusal of a request past the bound.
func hcBusy(res hcResult, buf *bytes.Buffer) bool {
	return !res.sent && res.rec.Code == http.StatusForbidden && res.rec.Header().Get(egressHeaderStatus) == egressRefusalDenied &&
		strings.Contains(buf.String(), `"rule_source":"`+ruleSourceHostRuleApprovalBusy+`"`)
}

// TestHostCallWaitsAreBounded: at most maxMCPCallsWaiting held requests wait at once. The next one, and a
// retry of a call already waiting, is refused at once and recorded. A slot is given back on every way out
// of the wait: an approval, a decline, the window ending and the client going away.
func TestHostCallWaitsAreBounded(t *testing.T) {
	for _, release := range []string{"approved", "declined", "timeout", "client gone"} {
		t.Run(release, func(t *testing.T) {
			p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
			cp := newHCCP(t, p)
			hostCallSecond = time.Second // windows outlast the test
			if release == "timeout" {
				hostCallSecond = 20 * time.Millisecond
			}
			var cancels []context.CancelFunc
			var waits []chan hcResult
			t.Cleanup(func() {
				for _, c := range cancels {
					c()
				}
				for _, d := range waits {
					<-d
				}
			})
			for i := range maxMCPCallsWaiting {
				d, c := hcGo(p, fmt.Sprintf("/admin/%d", i))
				waits, cancels = append(waits, d), append(cancels, c)
			}
			hcWait(t, "every waiter to ask", func() bool { rows, _, _ := cp.counts(); return rows == maxMCPCallsWaiting })
			for _, path := range []string{"/admin/next", "/admin/0"} {
				d, c := hcGo(p, path)
				cancels = append(cancels, c)
				select {
				case res := <-d:
					if !hcBusy(res, buf) {
						t.Fatalf("%s past the bound: sent=%v %d %v", path, res.sent, res.rec.Code, res.rec.Header())
					}
				case <-time.After(2 * time.Second):
					waits = append(waits, d)
					t.Fatalf("%s past the bound was held, not refused at once", path)
				}
			}
			if rows, _, _ := cp.counts(); rows != maxMCPCallsWaiting {
				t.Fatalf("a request past the bound asked a person: rows=%d", rows)
			}
			target := waits[0]
			waits = waits[1:]
			switch release {
			case "approved":
				cp.decidePath("/admin/0", types.ApprovalApproved)
			case "declined":
				cp.decidePath("/admin/0", types.ApprovalDenied)
			case "client gone":
				cancels[0]()
			}
			select {
			case <-target:
			case <-time.After(5 * time.Second):
				t.Fatalf("the waiter did not leave on %s", release)
			}
			if release == "timeout" {
				for _, d := range waits {
					<-d
				}
				waits = nil
			}
			d, c := hcGo(p, "/admin/after")
			waits, cancels = append(waits, d), append(cancels, c)
			hcWait(t, "a request after the release to ask", func() bool { rows, _, _ := cp.counts(); return rows == maxMCPCallsWaiting+1 })
		})
	}
}

// TestHostCallRememberedKeysAreBounded: at most maxMCPCallKeys distinct calls are remembered; a new one past
// that is refused at once, while a call already approved still goes on for the run.
func TestHostCallRememberedKeysAreBounded(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{{Rules: []types.HostRule{{Host: hrHost, Effect: types.HostRuleHold}}}}})
	cp := newHCCP(t, p)
	cp.newState = types.ApprovalApproved
	for i := range maxMCPCallKeys {
		if rec, sent := hcSend(p, "POST", fmt.Sprintf("http://%s/k/%d", hrHost, i)); !sent {
			t.Fatalf("call %d refused: %d %v", i, rec.Code, rec.Header())
		}
	}
	rec, sent := hcSend(p, "POST", "http://"+hrHost+"/k/new")
	if !hcBusy(hcResult{rec, sent}, buf) || !strings.Contains(rec.Header().Get(egressHeaderDetail), "too many different requests") {
		t.Fatalf("a call past the key bound: sent=%v %d %v", sent, rec.Code, rec.Header())
	}
	if rows, _, _ := cp.counts(); rows != maxMCPCallKeys {
		t.Errorf("a call past the key bound asked a person: rows=%d", rows)
	}
	if _, sent := hcSend(p, "POST", "http://"+hrHost+"/k/0"); !sent {
		t.Error("an approved call was refused once the key bound was reached")
	}
}

// hcPlain sends one absolute-form plain-HTTP request through the proxy's own handler and reports the status.
func hcPlain(t *testing.T, p *Proxy, method, path string) int {
	t.Helper()
	srv := httptest.NewServer(p)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, method+" http://"+hrHost+path+" HTTP/1.1\r\nHost: "+hrHost+"\r\nContent-Length: 0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestHostCallPlainHoldAfterThePolicy: on plain HTTP a held request asks a person only once the policy
// allows its host. A rule host the policy does not allow is refused without a host call raise; an allowed
// one asks, and an approval lets it reach the upstream.
func TestHostCallPlainHoldAfterThePolicy(t *testing.T) {
	cu := captureUpstream(t, false, "ok")
	plainProxy := func(allowed string) *Proxy {
		return newProxy(Options{RunID: uuid.New(), Resolver: publicResolver{}, Dial: redirectDial(upstreamAddr(cu.srv)),
			Sink:   &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
			Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{allowed}, HostRules: []types.HostRuleSet{hcHoldSet()}})})
	}
	p := plainProxy("other.example.com")
	cp := newHCCP(t, p)
	cp.newState = types.ApprovalApproved
	if got := hcPlain(t, p, "POST", "/admin"); got != http.StatusForbidden || cu.reached {
		t.Fatalf("a held request to a host the policy does not allow = %d reached=%v", got, cu.reached)
	}
	cp.mu.Lock()
	var hostRows int
	for _, row := range cp.rows {
		if row.scope.CallSHA256 != "" {
			hostRows++
		}
	}
	cp.mu.Unlock()
	if hostRows != 0 {
		t.Fatalf("a host the policy does not allow raised %d host call approvals", hostRows)
	}

	p = plainProxy(hrHost)
	p.approval = newApprovalClient(cp.srv.URL, newTokenSource("tok"), p.runID, cp.srv.Client())
	if got := hcPlain(t, p, "POST", "/admin"); got != http.StatusOK || !cu.reached {
		t.Fatalf("an approved held request to an allowed host = %d reached=%v", got, cu.reached)
	}
	if rows, _, _ := cp.counts(); rows != 1 {
		t.Errorf("rows=%d, want one host call asked", rows)
	}
}
