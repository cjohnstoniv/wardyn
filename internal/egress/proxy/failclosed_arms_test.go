// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// approvalStateServer is a control plane whose internal approval route always
// answers state.
func approvalStateServer(t *testing.T, state string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/internal/approvals/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"state":"`+state+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWaitForGitApproval_TerminalArms: DENIED, EXPIRED and CANCELLED each end
// the proxy-side wait at once with their own sentence. Any of them regressing
// into the default "still PENDING" arm would hold the git operation for the
// whole approval budget — for CANCELLED, on a run that has already been killed.
func TestWaitForGitApproval_TerminalArms(t *testing.T) {
	orig := gitApprovalPollInterval
	gitApprovalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { gitApprovalPollInterval = orig })
	t.Setenv(envGitApprovalTimeout, "5s")

	for _, tc := range []struct {
		state types.ApprovalState
		want  string
	}{
		{types.ApprovalDenied, "was denied by the operator"},
		{types.ApprovalExpired, "expired before a decision was made"},
		{types.ApprovalCancelled, "was cancelled: the run ended before anyone decided it"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			p, _ := newGitBrokerProxy(t, nil, upstreamAddr(approvalStateServer(t, string(tc.state))))
			start := time.Now()
			tok, _, _, err := p.waitForGitApproval(context.Background(), uuid.New(), uuid.New())
			if tok != "" || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("waitForGitApproval = (%q, %v), want no token and %q", tok, err, tc.want)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("a terminal %s took %s; it must not wait out the budget", tc.state, elapsed)
			}
		})
	}
}

// TestWaitForGitApproval_DeadlineAndContextWording: the budget timer and a
// budget-shaped ctx deadline both say "timed out ... approve it in the Wardyn
// UI", never a bare "context deadline exceeded"; a cancelled ctx (the sandbox
// hanging up) is reported as the cancellation it is, not as a timeout.
func TestWaitForGitApproval_DeadlineAndContextWording(t *testing.T) {
	orig := gitApprovalPollInterval
	gitApprovalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { gitApprovalPollInterval = orig })

	approvalID := uuid.New()
	p, _ := newGitBrokerProxy(t, nil, upstreamAddr(approvalStateServer(t, "PENDING")))

	t.Run("budget timer", func(t *testing.T) {
		t.Setenv(envGitApprovalTimeout, "150ms")
		_, _, _, err := p.waitForGitApproval(context.Background(), uuid.New(), approvalID)
		want := "timed out after 150ms waiting for credential approval " + approvalID.String()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})
	t.Run("ctx deadline", func(t *testing.T) {
		t.Setenv(envGitApprovalTimeout, "10s")
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, _, _, err := p.waitForGitApproval(ctx, uuid.New(), approvalID)
		if err == nil || !strings.Contains(err.Error(), "timed out after 10s waiting for credential approval") ||
			errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the budget sentence rather than a bare ctx deadline", err)
		}
	})
	t.Run("ctx cancelled", func(t *testing.T) {
		t.Setenv(envGitApprovalTimeout, "10s")
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, _, _, err := p.waitForGitApproval(ctx, uuid.New(), approvalID)
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want context.Canceled, not a timeout", err)
		}
	})
}

// TestHTTPApprovalReader_PassesStatusThrough: the credential-reauth hold
// classifies 401/403/410 as "run gone", 404 as "request gone" and 5xx as
// transient, so its real reader must hand the status back unchanged, carry
// the CURRENT run token, and never report a state for a non-200 or an
// undecodable body.
func TestHTTPApprovalReader_PassesStatusThrough(t *testing.T) {
	id := uuid.New()
	var status int
	var body, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	tok := newTokenSource("RUNTOK-1")
	reader := httpApprovalReader{base: srv.URL, token: tok, client: srv.Client()}

	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantState types.ApprovalState
		wantErr   bool
	}{
		{"200 approved", http.StatusOK, `{"id":"` + id.String() + `","state":"APPROVED"}`, types.ApprovalApproved, false},
		{"401", http.StatusUnauthorized, `{"state":"APPROVED"}`, "", false},
		{"410", http.StatusGone, "", "", false},
		{"404", http.StatusNotFound, "", "", false},
		{"503", http.StatusServiceUnavailable, `{"state":"APPROVED"}`, "", false},
		{"200 garbage", http.StatusOK, "not json", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body = tc.status, tc.body
			state, gotStatus, err := reader.readApproval(context.Background(), id)
			if state != tc.wantState || gotStatus != tc.status || (err != nil) != tc.wantErr {
				t.Fatalf("readApproval = (%q, %d, %v), want (%q, %d, err=%v)",
					state, gotStatus, err, tc.wantState, tc.status, tc.wantErr)
			}
			if gotPath != "/api/v1/internal/approvals/"+id.String() {
				t.Fatalf("path = %q", gotPath)
			}
		})
	}

	// A renewed run token is what the next read carries.
	tok.Set("RUNTOK-2")
	status, body = http.StatusOK, `{"state":"PENDING"}`
	if _, _, err := reader.readApproval(context.Background(), id); err != nil {
		t.Fatalf("readApproval: %v", err)
	}
	if gotAuth != "Bearer RUNTOK-2" {
		t.Fatalf("Authorization = %q, want the current run token %q", gotAuth, "Bearer RUNTOK-2")
	}
}

// TestVetTrustedHost_LiteralIPs: an operator-authored gateway host that is a
// literal IP is vetted as that IP, never resolved. The resolver here answers
// every name with a public address, so a regression that sent literals to the
// resolver would admit all five refused addresses.
func TestVetTrustedHost_LiteralIPs(t *testing.T) {
	_, ownSubnet, _ := net.ParseCIDR("172.30.0.0/16")
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Resolver:        publicResolver{},
		LocalSubnets:    []*net.IPNet{ownSubnet},
		ControlPlaneIPs: []net.IP{net.ParseIP("10.40.0.9")},
	})

	if addr, err := p.vetTrustedHost("10.1.2.3", 443); err != nil || addr != "10.1.2.3:443" {
		t.Fatalf("vetTrustedHost(10.1.2.3) = (%q, %v), want (10.1.2.3:443, nil)", addr, err)
	}
	for _, host := range []string{
		"127.0.0.1",
		"169.254.169.254",
		"::ffff:169.254.169.254",
		"172.30.0.5", // the proxy's own subnet
		"10.40.0.9",  // the control plane
	} {
		if addr, err := p.vetTrustedHost(host, 443); !errors.Is(err, errGatewayVet) {
			t.Errorf("vetTrustedHost(%s) = (%q, %v), want errGatewayVet", host, addr, err)
		}
	}
}

// TestADOCredentialRefusalFor_Mapping pins which sentence and which decision
// source each credential failure gets. The reauth errors wrap one another
// (timed-out-again ⊂ timed-out ⊂ no-credential), so the order of the checks is
// the contract: only the FIRST observer of an expiry writes the
// credential:reauth-timeout row.
func TestADOCredentialRefusalFor_Mapping(t *testing.T) {
	const fallback = "fallback-source"
	for _, tc := range []struct {
		name    string
		err     error
		wantMsg string
		wantSrc string
	}{
		{"timed out, first observer", errReauthTimedOut, adoSignInTimedOutRefusal, ruleSourceCredentialReauthTimeout},
		{"timed out, already recorded", errReauthTimedOutAgain, adoSignInTimedOutRefusal, fallback},
		{"hold ended (run gone)", reauthEndedRunGone, adoSignInEndedRefusal, fallback},
		{"no credential", errReauthNoCredential, adoSignInEndedRefusal, fallback},
		{"control-plane 403 sentence", injectionStatusError{status: http.StatusForbidden, body: `{"error":"closed by an admin"}`}, "closed by an admin", fallback},
		{"control-plane 403 without a sentence", injectionStatusError{status: http.StatusForbidden, body: "nope"}, adoCredentialFailedRefusal, fallback},
		{"control-plane 500", injectionStatusError{status: http.StatusInternalServerError, body: `{"error":"db down"}`}, adoCredentialFailedRefusal, fallback},
		{"anything else", errors.New("dial tcp: refused"), adoCredentialFailedRefusal, fallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, src := adoCredentialRefusalFor(tc.err, fallback)
			if msg != tc.wantMsg || src != tc.wantSrc {
				t.Fatalf("adoCredentialRefusalFor = (%q, %q), want (%q, %q)", msg, src, tc.wantMsg, tc.wantSrc)
			}
		})
	}
}
