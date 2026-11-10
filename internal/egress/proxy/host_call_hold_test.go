// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
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
