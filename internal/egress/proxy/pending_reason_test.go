// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pendingReasonCases are the ways a first-use approval refuses with nobody
// deciding it, and the still-waiting approval each must be told apart from.
var pendingReasonCases = []struct {
	name, reason string
	poll         types.ApprovalState // a seeded approval's state on the next poll; "" = none seeded
	refuse       bool                // the control plane refuses the raise for good
	capped       bool                // the run's host table is full
}{
	{name: "still pending", reason: "approval:pending"},
	{name: "expired", reason: ruleSourceApprovalExpired, poll: types.ApprovalExpired},
	{name: "cancelled", reason: ruleSourceApprovalExpired, poll: types.ApprovalCancelled},
	{name: "host cap", reason: ruleSourceApprovalHostCap, capped: true},
	{name: "refused", reason: ruleSourceApprovalRefused, refuse: true},
}

// newPendingReasonApproval builds an approval client whose next Resolve of
// host answers the case's state.
func newPendingReasonApproval(t *testing.T, host string, poll types.ApprovalState, refuse, capped bool) *approvalClient {
	t.Helper()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if refuse {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
			return
		}
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: poll})
	}))
	t.Cleanup(cp.Close)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if poll != "" {
		ap.hosts[approvalHostKey(host)] = &hostApproval{state: apPending, approvalID: uuid.New(), lastPoll: time.Now().Add(-2 * pollInterval)}
	}
	for i := 0; capped && len(ap.hosts) < maxApprovalHosts; i++ {
		ap.hosts[fmt.Sprintf("h%d.test", i)] = &hostApproval{state: apApproved}
	}
	return ap
}

// connectResponse sends one CONNECT to addr and returns its response.
func connectResponse(t *testing.T, addr, target, auth string) *http.Response {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return resp
}

// TestPendingAnswerCarriesItsReason: every approval-pending answer (CONNECT,
// a plain request, an organisation-forwarded request and the sign-in
// listener) names its decision reason, so a sandbox can tell an approval a
// person is deciding from one that expired, was cancelled or was never raised.
// The status and body stay the approval-pending 403.
func TestPendingAnswerCarriesItsReason(t *testing.T) {
	review := types.RunPolicySpec{AllowedDomains: []string{"site.test"}, FirstUseApproval: types.FirstUseDenyWithReview}
	sites := map[string]func(t *testing.T, ap func(host string) *approvalClient) (int, http.Header, string){
		"connect": func(t *testing.T, ap func(string) *approvalClient) (int, http.Header, string) {
			p, _ := newTestProxy(t, review, "127.0.0.1:1", ap("unknown.test"), nil)
			srv := httptest.NewServer(p)
			t.Cleanup(srv.Close)
			resp := connectResponse(t, strings.TrimPrefix(srv.URL, "http://"), "unknown.test:443", "")
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, resp.Header, string(b)
		},
		"plain": func(t *testing.T, ap func(string) *approvalClient) (int, http.Header, string) {
			p, _ := newTestProxy(t, review, "127.0.0.1:1", ap("unknown.test"), nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://unknown.test/"))
			return rec.Code, rec.Header(), rec.Body.String()
		},
		"via_org": func(t *testing.T, ap func(string) *approvalClient) (int, http.Header, string) {
			f := newViaOrgFixture(t, nil, func(c *Config) {
				// The destination is off the allowlist, so it asks first-use approval.
				c.Policy = types.RunPolicySpec{AllowedDomains: []string{githubHost}, FirstUseApproval: types.FirstUseDenyWithReview}
				c.Injection, c.MITMHosts = nil, nil
			})
			f.engine.srv.proxy.approval = ap(viaOrgHost)
			rec := f.inject(http.MethodGet, "/", nil)
			return rec.Code, rec.Header(), rec.Body.String()
		},
		"sign-in": func(t *testing.T, ap func(string) *approvalClient) (int, http.Header, string) {
			rig := newSigninLaneRig(t, func(o *Options) {
				o.Policy = CompilePolicy(review)
				o.Approval = ap("unknown.test")
			})
			resp := connectResponse(t, rig.signinAddr, "unknown.test:443", signinAuth(rig.goodCred))
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, resp.Header, string(b)
		},
	}
	for site, serve := range sites {
		for _, tc := range pendingReasonCases {
			t.Run(site+"/"+tc.name, func(t *testing.T) {
				code, hdr, body := serve(t, func(host string) *approvalClient {
					return newPendingReasonApproval(t, host, tc.poll, tc.refuse, tc.capped)
				})
				if code != http.StatusForbidden || hdr.Get(egressHeaderStatus) != egressRefusalPending ||
					!strings.Contains(body, `"wardyn":"approval-pending"`) {
					t.Fatalf("answer = %d %s=%q %q, want the approval-pending 403", code, egressHeaderStatus, hdr.Get(egressHeaderStatus), body)
				}
				if got := hdr.Get(egressHeaderReason); got != tc.reason {
					t.Fatalf("%s = %q, want %q", egressHeaderReason, got, tc.reason)
				}
			})
		}
	}
}
