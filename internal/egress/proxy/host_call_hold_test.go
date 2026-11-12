// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	_, refused := p.hostRuleRefused(rec, r, hrHost, 80)
	return rec, !refused
}

// TestHostCallHoldWaitsThenAllowsForTheRun: a held request waits and is answered pending, never sent; once
// a person approves, every request with that host, method and path goes on for the rest of the run, any
// query or body included, with no new ask; another path asks again.
func TestHostCallHoldWaitsThenAllowsForTheRun(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	rec, sent := hcSend(p, "POST", "http://"+hrHost+"/admin")
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
	for _, target := range []string{"/admin", "/admin?x=1", "/%61dmin"} {
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
		_, refused := p.hostRuleRefused(rec, r, hrHost, 80)
		done <- hcResult{rec, !refused}
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

// TestHostCallReleaseRowNamesTheApproval: each request an approval lets through, the first and every later
// one, gets an allow row naming that approval.
func TestHostCallReleaseRowNamesTheApproval(t *testing.T) {
	p, buf := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	cp.newState = types.ApprovalApproved
	for range 2 {
		if _, sent := hcSend(p, "POST", "http://"+hrHost+"/admin"); !sent {
			t.Fatal("approved request refused")
		}
	}
	cp.mu.Lock()
	id := cp.rows[0].id
	cp.mu.Unlock()
	var released int
	for _, d := range hrRows(t, buf) {
		if d.RuleSource == ruleSourceHostRuleApprovalApproved && d.Decision == egress.Allow && d.ApprovalID != nil && *d.ApprovalID == id {
			released++
		}
	}
	if released != 2 {
		t.Fatalf("release rows naming %s = %d, want 2:\n%s", id, released, buf)
	}
}

// hcReleases counts the allow rows an approval wrote for held requests.
func hcReleases(log string) int {
	return strings.Count(log, `"rule_source":"`+ruleSourceHostRuleApprovalApproved+`"`)
}

// TestHostCallApprovedHoldIsForwarded: a held request a person approves on the plain lane or inside a
// terminated tunnel reaches the host. The forward is not judged a second time: one ask, one release row.
func TestHostCallApprovedHoldIsForwarded(t *testing.T) {
	cu := captureUpstream(t, false, "ok")
	buf := &bytes.Buffer{}
	p := newProxy(Options{RunID: uuid.New(), Resolver: publicResolver{}, Dial: redirectDial(upstreamAddr(cu.srv)),
		Sink:   &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{hrHost}, HostRules: []types.HostRuleSet{hcHoldSet()}})})
	cp := newHCCP(t, p)
	cp.newState = types.ApprovalApproved
	if got := hcPlain(t, p, "POST", "/admin"); got != http.StatusOK || !cu.reached {
		t.Errorf("plain: an approved held request = %d reached=%v, want 200 at the host", got, cu.reached)
	}
	if rows, _, _ := cp.counts(); rows != 1 || hcReleases(buf.String()) != 1 {
		t.Errorf("plain: %d asks and %d release rows, want one of each", rows, hcReleases(buf.String()))
	}

	tu := captureUpstream(t, true, "ok")
	mp, mbuf, caPEM := hrProxy(t, hrOpts{}, tu.srv)
	mp.policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{hrHost}, HostRules: []types.HostRuleSet{hcHoldSet()}})
	mcp := newHCCP(t, mp)
	mcp.newState = types.ApprovalApproved
	srv := httptest.NewServer(mp)
	defer srv.Close()
	status, conn, _ := hrConnect(t, srv.URL, "443")
	if status != http.StatusOK {
		t.Fatalf("CONNECT = %d", status)
	}
	if got := hrInner(t, conn, caPEM, "POST", "/admin"); got != http.StatusOK || !tu.reached {
		t.Errorf("tunnel: an approved held request = %d reached=%v, want 200 at the host", got, tu.reached)
	}
	if rows, _, _ := mcp.counts(); rows != 1 || hcReleases(mbuf.String()) != 1 {
		t.Errorf("tunnel: %d asks and %d release rows, want one of each", rows, hcReleases(mbuf.String()))
	}
}

// TestHostCallRetryForwardedUnderTheRunApproval: once approved, a retry of the same request is forwarded
// to the host with no new ask, on the plain lane and on a brokered route.
func TestHostCallRetryForwardedUnderTheRunApproval(t *testing.T) {
	cu := captureUpstream(t, false, "ok")
	p := newProxy(Options{RunID: uuid.New(), Resolver: publicResolver{}, Dial: redirectDial(upstreamAddr(cu.srv)),
		Sink:   &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{hrHost}, HostRules: []types.HostRuleSet{hcHoldSet()}})})
	cp := newHCCP(t, p)
	cp.newState = types.ApprovalApproved
	for i := range 2 {
		cu.reached = false
		if got := hcPlain(t, p, "POST", "/admin"); got != http.StatusOK || !cu.reached {
			t.Errorf("plain send %d = %d reached=%v, want 200 at the host", i, got, cu.reached)
		}
	}
	if rows, _, _ := cp.counts(); rows != 1 {
		t.Errorf("plain: %d asks, want one", rows)
	}

	up := newRawUpstream(t, `{}`)
	bp, _ := llmChecksProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcModelsHold()}}, "", up)
	bcp := newHCCP(t, bp)
	bcp.newState = types.ApprovalApproved
	for i := range 2 {
		if rec, reached := hcModels(t, bp, up); rec.Code != http.StatusOK || !reached {
			t.Errorf("brokered send %d = %d reached=%v, want 200 at the host", i, rec.Code, reached)
		}
	}
	if rows, _, _ := bcp.counts(); rows != 1 {
		t.Errorf("brokered: %d asks, want one", rows)
	}
}

// hcModelsHold holds GET /v1/models on the model host and allows everything else there.
func hcModelsHold() types.HostRuleSet {
	return types.HostRuleSet{Rules: []types.HostRule{
		{Host: openaiHost, Methods: []string{"GET"}, Paths: []string{"/v1/models"}, Effect: types.HostRuleHold},
		{Host: openaiHost, Effect: types.HostRuleAllow},
	}}
}

// hcModels sends the model route's GET /v1/models and reports the status and whether the host saw it.
func hcModels(t *testing.T, p *Proxy, up *rawUpstream) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	up.reached = false
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, llmOpenAIPrefix+"v1/models", nil))
	return rec, up.reached
}

// TestHostCallBrokeredRouteHeldAtTheForward: a brokered route (the git broker) meets the hold first at the
// outbound gate, which asks a person: the sandbox reads a pending answer while nobody has decided, and once
// a person approves, the retry reaches the host under the same one approval.
func TestHostCallBrokeredRouteHeldAtTheForward(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, buf := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
	p.policy.hostRules = []types.HostRuleSet{{Rules: []types.HostRule{
		{Host: githubHost, Methods: []string{"GET"}, Paths: []string{"/octocat/hello-world.git"}, Effect: types.HostRuleHold},
		{Host: githubHost, Effect: types.HostRuleAllow},
	}}}
	cp := newHCCP(t, p)
	send := func() (*httptest.ResponseRecorder, bool) {
		hits := gitHitsOf(up)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack", nil))
		return rec, gitHitsOf(up) > hits
	}
	rec, reached := send()
	if reached || rec.Code != http.StatusForbidden || rec.Header().Get(egressHeaderStatus) != egressRefusalPending ||
		!strings.Contains(rec.Header().Get(egressHeaderDetail), "GET /octocat/hello-world.git/info/refs on "+githubHost) {
		t.Fatalf("undecided: %d reached=%v %v, want a pending answer before the host", rec.Code, reached, rec.Header())
	}
	cp.decideAll(types.ApprovalApproved)
	if rec, reached = send(); rec.Code != http.StatusOK || !reached {
		t.Fatalf("approved: %d reached=%v, want 200 at the host", rec.Code, reached)
	}
	if rows, _, _ := cp.counts(); rows != 1 || hcReleases(buf.String()) != 1 {
		t.Errorf("%d asks and %d release rows, want one of each", rows, hcReleases(buf.String()))
	}
}

// TestHostCallHoldAtTheForwardKeepsADeny: a forward an approved hold would let through is refused, unasked,
// once another set denies it. A deny in any set wins at the outbound gate as at the lanes.
func TestHostCallHoldAtTheForwardKeepsADeny(t *testing.T) {
	deny := types.HostRuleSet{Rules: []types.HostRule{
		{Host: openaiHost, Methods: []string{"GET"}, Paths: []string{"/v1/models"}, Effect: types.HostRuleDeny},
		{Host: openaiHost, Effect: types.HostRuleAllow},
	}}
	for _, tc := range []struct {
		name string
		sets []types.HostRuleSet
		pass bool
	}{
		{"the hold alone", []types.HostRuleSet{hcModelsHold()}, true},
		{"the hold and a denying set", []types.HostRuleSet{hcModelsHold(), deny}, false},
		{"a denying set and the hold", []types.HostRuleSet{deny, hcModelsHold()}, false},
	} {
		up := newRawUpstream(t, `{}`)
		p, buf := llmChecksProxy(t, types.RunPolicySpec{HostRules: tc.sets}, "", up)
		cp := newHCCP(t, p)
		cp.newState = types.ApprovalApproved
		rec, reached := hcModels(t, p, up)
		rows, _, _ := cp.counts()
		if tc.pass && (rec.Code != http.StatusOK || !reached || rows != 1) {
			t.Errorf("%s: %d reached=%v asks=%d, want 200 at the host after one ask", tc.name, rec.Code, reached, rows)
		}
		if !tc.pass && (rec.Code < 300 || reached || rows != 0 ||
			!strings.Contains(buf.String(), `"rule_source":"`+ruleSourceHostRule+`"`)) {
			t.Errorf("%s: %d reached=%v asks=%d, want a policy:host-rule refusal before the host, unasked", tc.name, rec.Code, reached, rows)
		}
	}
}

// TestHostCallMarkerCoversOnlyItsOwnCall: a forward carrying a lane's admission goes on unasked only while
// its host, method and path are the ones admitted. Change any of them before the outbound gate and the
// forward is asked about again, never sent on the old admission; add an override header and it is refused.
func TestHostCallMarkerCoversOnlyItsOwnCall(t *testing.T) {
	const other = "b.example.com"
	hold := func(h string) types.HostRule {
		return types.HostRule{Host: h, Paths: []string{"/admin", "/other"}, Effect: types.HostRuleHold}
	}
	p, _ := hcProxy(t, types.RunPolicySpec{AllowedDomains: []string{other}, HostRules: []types.HostRuleSet{{Rules: []types.HostRule{hold(hrHost), hold(other)}}}})
	cp := newHCCP(t, p)
	admitted, bad := hostCallScopeOf(hrHost, "POST", "/admin")
	if bad != nil {
		t.Fatal(bad.cause)
	}
	for _, tc := range []struct {
		name, method, url string
		asks              bool
	}{
		{"unchanged", "POST", "http://" + hrHost + "/admin", false},
		{"another host", "POST", "http://" + other + "/admin", true},
		{"another method", "PUT", "http://" + hrHost + "/admin", true},
		{"another path", "POST", "http://" + hrHost + "/other", true},
	} {
		_, before, _ := cp.counts()
		req := withHostCallAdmitted(httptest.NewRequest(tc.method, tc.url, nil), admitted)
		_, err := p.outboundHostRuleRefused(req)
		_, after, _ := cp.counts()
		if !tc.asks && (err != nil || after != before) {
			t.Errorf("%s: err=%v raises %d->%d, want it sent unasked", tc.name, err, before, after)
		}
		if tc.asks && (err == nil || after == before) {
			t.Errorf("%s: err=%v raises %d->%d, want it asked about again and not sent", tc.name, err, before, after)
		}
	}
	// The admission answers the hold only: a marked request that now carries an override is refused, unasked.
	_, before, _ := cp.counts()
	req := withHostCallAdmitted(httptest.NewRequest("POST", "http://"+hrHost+"/admin", nil), admitted)
	req.Header.Set("X-HTTP-Method-Override", "DELETE")
	_, err := p.outboundHostRuleRefused(req)
	var hr *hostRuleRefusal
	if _, after, _ := cp.counts(); !errors.As(err, &hr) || hr.hold != nil || after != before || hr.log.RuleSource != ruleSourceHostRule {
		t.Errorf("marked with a method override: err=%v raises %d->%d, want a policy:host-rule refusal, unasked", err, before, after)
	}
}

// TestHostCallModelRouteAnswersPending: the brokered model route answers a held forward as the lanes do:
// the pending 403 while nobody has decided, and the host once a person approves.
func TestHostCallModelRouteAnswersPending(t *testing.T) {
	up := newRawUpstream(t, `{}`)
	p, _ := llmChecksProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcModelsHold()}}, "", up)
	cp := newHCCP(t, p)
	rec, reached := hcModels(t, p, up)
	if reached || rec.Code != http.StatusForbidden || rec.Header().Get(egressHeaderStatus) != egressRefusalPending ||
		!strings.Contains(rec.Body.String(), `"wardyn":"approval-pending"`) ||
		!strings.Contains(rec.Header().Get(egressHeaderDetail), "GET /v1/models on "+openaiHost) {
		t.Fatalf("undecided: %d reached=%v %v %s, want the pending 403 before the host", rec.Code, reached, rec.Header(), rec.Body)
	}
	cp.decideAll(types.ApprovalApproved)
	if rec, reached = hcModels(t, p, up); rec.Code != http.StatusOK || !reached {
		t.Fatalf("approved: %d reached=%v, want 200 at the host", rec.Code, reached)
	}
}

// TestHostCallProxyOwnSendNeverHeld: a request the proxy sends for itself (withProxyOwnSend) is not the
// sandbox's, so a hold on its host, method and path never asks anyone about it.
func TestHostCallProxyOwnSendNeverHeld(t *testing.T) {
	p, _ := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	req := httptest.NewRequest("POST", "http://"+hrHost+"/admin", nil)
	if _, err := p.outboundHostRuleRefused(req.WithContext(withProxyOwnSend(req.Context()))); err != nil {
		t.Fatalf("own send: %v, want it sent", err)
	}
	if _, err := p.outboundHostRuleRefused(req); err == nil {
		t.Fatal("the same request from the sandbox was sent, want it held")
	}
	if rows, _, _ := cp.counts(); rows != 1 {
		t.Errorf("%d asks, want one, for the sandbox's request only", rows)
	}
}

// hcDoorRT is an API door's own transport that records whether it was used.
type hcDoorRT struct{ sent int }

func (d *hcDoorRT) RoundTrip(*http.Request) (*http.Response, error) {
	d.sent++
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

// TestHostCallAPIDoorHoldIsBounded: a door on its own transport (the kube door) meets a hold in sendAPIDoor.
// Undecided, it waits no longer than the hold window and is never sent; approved, it goes out.
func TestHostCallAPIDoorHoldIsBounded(t *testing.T) {
	p, _ := hcProxy(t, types.RunPolicySpec{HostRules: []types.HostRuleSet{hcHoldSet()}})
	cp := newHCCP(t, p)
	rt := &hcDoorRT{}
	start := time.Now()
	_, err := p.sendAPIDoor(apiDoor{rt: rt}, httptest.NewRequest("POST", "https://"+hrHost+"/admin", nil))
	var hr *hostRuleRefusal
	if !errors.As(err, &hr) || hr.hold == nil || hr.hold.decision != egress.Pending || rt.sent != 0 {
		t.Fatalf("undecided: err=%v sent=%d, want a pending refusal before the door's transport", err, rt.sent)
	}
	if took := time.Since(start); took > 20*hostCallWindow*hostCallSecond {
		t.Errorf("undecided wait took %v, want it bounded by the %v window", took, hostCallWindow*hostCallSecond)
	}
	cp.decideAll(types.ApprovalApproved)
	if _, err := p.sendAPIDoor(apiDoor{rt: rt}, httptest.NewRequest("POST", "https://"+hrHost+"/admin", nil)); err != nil || rt.sent != 1 {
		t.Fatalf("approved: err=%v sent=%d, want it sent once", err, rt.sent)
	}
}

// The AWS portal's role credentials are the proxy's own send (getRoleCredentials): a run whose host rules
// deny that GET on the portal still fetches them, while the sandbox's own GET there is refused.
func TestSigV4RoleCredentialsNotJudgedByHostRules(t *testing.T) {
	h := newSigV4Harness(t)
	h.p.policy.hostRules = []types.HostRuleSet{{Rules: []types.HostRule{
		{Host: sigv4Portal, Methods: []string{http.MethodGet}, Paths: []string{awsSSOFederationPath}, Effect: types.HostRuleDeny},
		{Host: sigv4Portal, Effect: types.HostRuleAllow},
	}}}
	if _, err := h.p.getRoleCredentials(t.Context(), "x-amz-sso_bearer_token", sigv4Bearer); err != nil || h.portalCalls.Load() != 1 {
		t.Fatalf("own send: err=%v portal calls=%d, want the credentials fetched", err, h.portalCalls.Load())
	}
	sandbox := httptest.NewRequest(http.MethodGet, "https://"+sigv4Portal+awsSSOFederationPath, nil)
	if _, err := h.p.outboundHostRuleRefused(sandbox); !errors.Is(err, errHostRuleRefused) {
		t.Fatalf("the sandbox's GET %s on %s: err=%v, want it refused by host rules", awsSSOFederationPath, sigv4Portal, err)
	}
}

// The push comparison's forge reads are the proxy's own send (forgeRepo.read): a run whose host rules deny
// that GET on the forge still reads it, while the sandbox's own GET there is refused.
func TestForgeReadNotJudgedByHostRules(t *testing.T) {
	cu := captureUpstream(t, true, `{}`)
	p, _, _ := hrProxy(t, hrOpts{}, cu.srv)
	p.policy.hostRules = []types.HostRuleSet{{Rules: []types.HostRule{
		{Host: hrHost, Methods: []string{http.MethodGet}, Paths: []string{"/repos"}, Effect: types.HostRuleDeny},
		{Host: hrHost, Effect: types.HostRuleAllow},
	}}}
	var v map[string]any
	f := &forgeRepo{p: p}
	status, _, err := f.read(t.Context(), hrHost, "/repos/acme/app", func(context.Context, *http.Request) error { return nil }, &v)
	if err != nil || status != http.StatusOK || !cu.reached {
		t.Fatalf("own send: status %d err=%v reached=%v, want the forge read", status, err, cu.reached)
	}
	sandbox := httptest.NewRequest(http.MethodGet, "https://"+hrHost+"/repos/acme/app", nil)
	if _, err := p.outboundHostRuleRefused(sandbox); !errors.Is(err, errHostRuleRefused) {
		t.Fatalf("the sandbox's GET /repos/acme/app: err=%v, want it refused by host rules", err)
	}
}
