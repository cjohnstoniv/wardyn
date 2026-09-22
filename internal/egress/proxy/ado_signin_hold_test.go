// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signInControlPlane answers an injection resolve the way a lapsed Azure DevOps
// sign-in does: 423 reauth_pending until the person has signed in (the reader
// says APPROVED), then the fresh bearer.
type signInControlPlane struct {
	srv      *httptest.Server
	mu       sync.Mutex
	resolves int
	id       uuid.UUID
	token    string
}

func newSignInControlPlane(t *testing.T, token string) *signInControlPlane {
	t.Helper()
	cp := &signInControlPlane{id: uuid.New(), token: token}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cp.mu.Lock()
		cp.resolves++
		first := cp.resolves == 1
		cp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.WriteHeader(http.StatusLocked)
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "reauth_pending", "approval_id": cp.id.String()})
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: "Bearer " + cp.token, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

// lapse points host's injection at cp, with its cached credential already due
// for renewal, and the hold lane at reader.
func lapse(t *testing.T, inj *injector, host string, cp *signInControlPlane, reader approvalReader) {
	t.Helper()
	tok := &tokenSource{}
	tok.Set("run-token")
	inj.base, inj.token, inj.client = cp.srv.URL, tok, cp.srv.Client()
	inj.reauth, inj.approvals = newReauthCoordinator(), reader
	t.Cleanup(inj.reauth.stop)
	inj.byHost[host].expiresAt = time.Now().UnixMilli()
}

// A mid-run renewal that meets a dead sign-in HOLDS the Azure DevOps request;
// the person signs in, and the held request goes through with the new bearer.
func TestADOSignInHold_HeldRequestGoesThroughAfterTheSignIn(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	h := newADOHarness(t, adoscope.CapRead)
	cp := newSignInControlPlane(t, adoToken)
	lapse(t, h.p.inject, adoHost, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalApproved)})
	rec := h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want the held request forwarded after the sign-in", rec.Code, rec.Body.String())
	}
	if reqs := h.fake.Requests(); len(reqs) != 1 || reqs[0].Token != adoToken {
		t.Fatalf("upstream saw %+v, want one request carrying the renewed bearer", reqs)
	}
}

// A hold that ends without a sign-in answers the REST client in Azure DevOps'
// own error shape — never the AWS SDK's.
func TestADOSignInHold_EndedHoldIsAnAzureDevOpsShaped403(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	h := newADOHarness(t, adoscope.CapRead)
	cp := newSignInControlPlane(t, adoToken)
	lapse(t, h.p.inject, adoHost, cp, &fakeApprovalReader{steps: steps(types.ApprovalCancelled)})
	rec := h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusForbidden || !strings.Contains(body, `"typeKey":"CredentialUnavailableException"`) ||
		!strings.Contains(body, adoSignInEndedRefusal) || strings.Contains(body, "__type") {
		t.Fatalf("status %d body %s, want an Azure DevOps-shaped 403 naming the ended sign-in", rec.Code, body)
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("upstream saw %d requests, want none", n)
	}
}

// git gets the same refusal in plain text, which it prints as "remote: …".
func TestADOSignInHold_GitGetsPlainText(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	h := newADOGitHarnessWith(t, func(p *Proxy, token string) {
		lapse(t, p.inject, "dev.azure.com", newSignInControlPlane(t, token), &fakeApprovalReader{steps: steps(types.ApprovalCancelled)})
	}, adoscope.CapRead)
	out, err := h.git(t, "clone", "https://dev.azure.com/acme/proj/_git/app", t.TempDir()+"/c")
	mustBeGitRefusal(t, out, err, "the request ended before a sign-in arrived")
}

// A held request's body is read AFTER the hold, and the inner server's
// ReadTimeout counts from the headers: without re-arming the deadline, a hold
// longer than what is left of it cuts the upload off.
func TestMITMHold_BodyAfterAHoldGetsAFreshReadDeadline(t *testing.T) {
	// The deadline the gate's own re-arm sets must lapse DURING the hold, so
	// only the re-arm after the credential resolve can save the body.
	prev := mitmReadTimeout
	mitmReadTimeout = time.Second
	t.Cleanup(func() { mitmReadTimeout = prev })
	fastPolls(t, 50*time.Millisecond)

	var got struct {
		sync.Mutex
		body, auth string
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Lock()
		got.body, got.auth = string(b), r.Header.Get("Authorization")
		got.Unlock()
	}))
	t.Cleanup(up.Close)

	const host = "files.example"
	inj := &injector{byHost: map[string]*injEntry{host: {grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer old"}}}}
	p, _ := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", strings.TrimPrefix(up.URL, "http://"), inj, nil)
	p.mitmHosts, p.mitmPorts = map[string]bool{host: true}, map[string]int{host: 443}
	p.mitmPlaintext = map[string]bool{plaintextKey(host, 443): true}
	cp := newSignInControlPlane(t, "renewed")
	// 25 polls of 50 ms: a hold past both the 250 ms ReadTimeout below and
	// the one-second deadline the gate's re-arm set before it.
	pending := make([]types.ApprovalState, 24)
	for i := range pending {
		pending[i] = types.ApprovalPending
	}
	lapse(t, inj, host, cp, &fakeApprovalReader{steps: steps(append(pending, types.ApprovalApproved)...)})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadTimeout: 250 * time.Millisecond, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.serveMITMRequest(w, r, host, 443)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	const payload = "a large upload"
	if _, err := io.WriteString(conn, "POST /upload HTTP/1.1\r\nHost: "+host+"\r\nContent-Length: 14\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // the body follows the hold, as a streamed upload does
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	got.Lock()
	defer got.Unlock()
	if resp.StatusCode != http.StatusOK || got.body != payload || got.auth != "Bearer renewed" {
		t.Fatalf("status %d, upstream got body %q auth %q; want the whole body forwarded with the renewed credential",
			resp.StatusCode, got.body, got.auth)
	}
}

// The boot resolve says it is the boot one, so the control plane fails a run
// whose Azure DevOps sign-in has ended with a hint instead of raising a
// sign-in request nothing will wait on.
func TestBuildInjectorMarksTheBootResolve(t *testing.T) {
	var query string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: "Bearer x"})
	}))
	t.Cleanup(cp.Close)
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{adoHost}})
	rules := []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: adoHost}, GrantID: uuid.New()}}
	if _, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client()); err != nil {
		t.Fatal(err)
	}
	if query != "phase=boot" {
		t.Fatalf("boot resolve query = %q, want phase=boot", query)
	}
}
