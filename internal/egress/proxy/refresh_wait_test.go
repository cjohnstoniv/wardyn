// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// slowestOf runs call n times at once and returns the slowest call's time, failing on any error.
func slowestOf(t *testing.T, n int, call func() error) time.Duration {
	t.Helper()
	lat := make([]time.Duration, n)
	var wg sync.WaitGroup
	for i := range lat {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if err := call(); err != nil {
				t.Errorf("call %d: %v", i, err)
			}
			lat[i] = time.Since(start)
		}()
	}
	wg.Wait()
	return slices.Max(lat)
}

// waitUntil polls cond for up to 5 s.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestInjectionRefreshServesTheStillValidHeader: while one request re-resolves an inspected host's credential,
// which is inside its refresh margin but not expired, and the control plane takes 2 s to answer, 20 more
// requests to the host are each answered at once with the still-valid credential; the request that asked
// carries the new one.
func TestInjectionRefreshServesTheStillValidHeader(t *testing.T) {
	var asks atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		time.Sleep(2 * time.Second)
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: "Bearer new",
			JTI: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}))
	t.Cleanup(cp.Close)
	var mu sync.Mutex
	var sent []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(up.Close)
	const host = "files.example"
	inj := &injector{base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(), byHost: map[string]*injEntry{host: {
		grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer old"},
		expiresAt: time.Now().Add(2 * time.Minute).UnixMilli(), // inside the 5-minute refresh margin
	}}}
	p, _ := newLocalRouteProxy(t, cp.URL, "RUNTOK", upstreamAddr(up), inj, nil)
	p.mitmHosts, p.mitmPorts = map[string]bool{host: true}, map[string]int{host: 443}
	p.mitmPlaintext = map[string]bool{plaintextKey(host, 443): true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.serveMITMRequest(w, r, host, 443)
	}))
	t.Cleanup(srv.Close)
	get := func() error {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
		if err != nil {
			return err
		}
		req.Host = host
		resp, err := srv.Client().Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}
	asked := make(chan error, 1)
	go func() { asked <- get() }() // inside the margin: this request re-resolves
	waitUntil(t, "the re-resolve to reach the control plane", func() bool { return asks.Load() == 1 })
	slowest := slowestOf(t, 20, get)
	t.Logf("slowest of 20 requests: %v", slowest) // the row's figure, read off a run without -race
	if slowest >= 500*time.Millisecond {
		t.Fatalf("slowest of 20 requests took %v, want far under the refresh's 2s: the still-valid credential is served while the refresh runs", slowest)
	}
	if err := <-asked; err != nil {
		t.Fatalf("the request that re-resolved: %v", err)
	}
	if n := asks.Load(); n != 1 {
		t.Errorf("%d re-resolves, want 1: one single flight for 21 requests", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 21 {
		t.Fatalf("upstream saw %d requests, want 21", len(sent))
	}
	for i, h := range sent[:20] {
		if h != "Bearer old" {
			t.Errorf("request %d carried %q, want the still-valid %q", i, h, "Bearer old")
		}
	}
	if last := sent[20]; last != "Bearer new" {
		t.Errorf("the request that re-resolved carried %q, want %q", last, "Bearer new")
	}
}

// TestInjectionRefreshWaiterLeavesWithItsRequest: once the credential has run out, a request waits for the
// re-resolve in flight, and leaves as soon as its own request ends instead of when the control plane answers.
func TestInjectionRefreshWaiterLeavesWithItsRequest(t *testing.T) {
	var asks atomic.Int32
	release := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		<-release
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: "Bearer new",
			JTI: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}))
	t.Cleanup(func() { close(release); cp.Close() })
	const host = "api.test"
	inj := &injector{base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(), byHost: map[string]*injEntry{host: {
		grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer old"},
		expiresAt: time.Now().Add(-time.Second).UnixMilli(), // run out
	}}}
	go func() { _, _, _ = inj.resolveCtx(context.Background(), host) }() // the re-resolve in flight
	waitUntil(t, "the first re-resolve to reach the control plane", func() bool { return asks.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := inj.resolveCtx(ctx, host); err == nil {
		t.Fatal("a waiter whose request ended got a credential")
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("the waiter left after %v, want soon after its 100ms", took)
	}
	if n := asks.Load(); n != 1 {
		t.Errorf("%d re-resolves, want 1: the waiter joins the one in flight", n)
	}
}

// TestAWSRoleCredentialsRefreshInTheBackground: inside the 5-minute renewal margin, 20 concurrent signings are
// each answered at once with the still-valid role credentials while one refresh, whose portal answer takes 2 s,
// runs behind them.
func TestAWSRoleCredentialsRefreshInTheBackground(t *testing.T) {
	h := newSigV4Harness(t)
	h.lifetime.Store(int64(2 * time.Minute)) // the first credentials arrive inside the margin
	if _, err := h.p.awsSigV4Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.lifetime.Store(0) // the refresh's answer lives an hour
	h.portalDelay.Store(int64(2 * time.Second))
	before := h.portalCalls.Load()
	sign := func() error {
		c, err := h.p.awsSigV4Credentials(context.Background())
		if err == nil && c.AccessKeyID != sigv4RoleKey {
			err = fmt.Errorf("access key %q", c.AccessKeyID)
		}
		return err
	}
	slowest := slowestOf(t, 20, sign)
	t.Logf("slowest of 20 signings: %v", slowest) // the row's figure, read off a run without -race
	if slowest >= 500*time.Millisecond {
		t.Fatalf("slowest of 20 signings took %v, want far under the refresh's 2s: the still-valid credentials are served while the refresh runs", slowest)
	}
	waitUntil(t, "the background refresh", func() bool {
		h.p.sigv4.mu.Lock()
		defer h.p.sigv4.mu.Unlock()
		return time.Until(h.p.sigv4.creds.Expires) > 30*time.Minute
	})
	if n := h.portalCalls.Load() - before; n != 1 {
		t.Errorf("%d portal calls, want 1: one single flight for 20 signings", n)
	}
}

// TestAWSRoleCredentialsFailedRefreshIsAskedOnce: with no usable credentials and a portal that refuses, 20
// concurrent signings share one portal call and all fail.
func TestAWSRoleCredentialsFailedRefreshIsAskedOnce(t *testing.T) {
	h := newSigV4Harness(t)
	h.portalCode.Store(http.StatusForbidden)
	h.portalDelay.Store(int64(300 * time.Millisecond))
	fails := func() error {
		if _, err := h.p.awsSigV4Credentials(context.Background()); err == nil {
			return fmt.Errorf("signed with no credentials")
		}
		return nil
	}
	_ = slowestOf(t, 20, fails)
	if n := h.portalCalls.Load(); n != 1 {
		t.Errorf("%d portal calls for 20 signings, want 1 (one refresh, then its failure is reused for a short while)", n)
	}
}

// TestAWSRoleCredentialsWaiterLeavesWithItsRequest: with no usable credentials, a signing waits for the
// refresh in flight, and leaves as soon as its own request ends instead of when the portal answers.
func TestAWSRoleCredentialsWaiterLeavesWithItsRequest(t *testing.T) {
	h := newSigV4Harness(t)
	h.portalDelay.Store(int64(2 * time.Second))
	go func() { _, _ = h.p.awsSigV4Credentials(context.Background()) }()
	waitUntil(t, "the refresh to reach the portal", func() bool { return h.portalCalls.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := h.p.awsSigV4Credentials(ctx); err == nil {
		t.Fatal("a signing whose request ended got credentials")
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("the signing left after %v, want soon after its 100ms", took)
	}
}

// TestAWSRoleCredentialsRefusedRenewalStopsSigning: once a renewal inside the margin is refused, signing
// fails although the held credentials have not expired, as it did before renewals ran in the background.
func TestAWSRoleCredentialsRefusedRenewalStopsSigning(t *testing.T) {
	h := newSigV4Harness(t)
	h.lifetime.Store(int64(2 * time.Minute)) // inside the margin
	if _, err := h.p.awsSigV4Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.portalCode.Store(http.StatusForbidden)
	_, _ = h.p.awsSigV4Credentials(context.Background()) // asks for the renewal, which is refused
	waitUntil(t, "the refused renewal", func() bool { return h.portalCalls.Load() == 2 })
	waitUntil(t, "signing to stop", func() bool {
		_, err := h.p.awsSigV4Credentials(context.Background())
		return err != nil
	})
	if n := h.portalCalls.Load(); n != 2 {
		t.Errorf("%d portal calls, want 2: the refusal is reused, not asked again per signing", n)
	}
}

// TestMITMBodyReadTimeCountsIdleTime: an upload through an inspected host that keeps sending is never cut by
// the read time, however long it runs in all; one that stops sending for the read time is.
func TestMITMBodyReadTimeCountsIdleTime(t *testing.T) {
	shortReadTime(t) // 200 ms
	up := slowUpstream(t)
	const host = "files.example"
	proxyFor := func() *Proxy {
		inj := &injector{byHost: map[string]*injEntry{host: {grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer t"}}}}
		p, _ := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", upstreamAddr(up), inj, nil)
		p.mitmHosts, p.mitmPorts = map[string]bool{host: true}, map[string]int{host: 443}
		p.mitmPlaintext = map[string]bool{plaintextKey(host, 443): true}
		return p
	}
	t.Run("a body that keeps moving arrives whole", func(t *testing.T) {
		k := newKeptAlive(t, proxyFor(), host)
		k.write(t, "POST /up HTTP/1.1\r\nHost: "+host+"\r\nTransfer-Encoding: chunked\r\n\r\n")
		var want strings.Builder
		for i := range 10 { // one byte every 100 ms: five times the read time in all
			time.Sleep(100 * time.Millisecond)
			k.write(t, "1\r\n"+strconv.Itoa(i)+"\r\n")
			want.WriteString(strconv.Itoa(i))
		}
		k.write(t, "0\r\n\r\n")
		if status, body, err := k.answer(); err != nil || status != http.StatusOK || body != "answer POST /up "+want.String() {
			t.Fatalf("trickled upload: status %d body %q err %v, want 200 and the whole body", status, body, err)
		}
	})
	t.Run("a body that stops for the read time is cut", func(t *testing.T) {
		k := newKeptAlive(t, proxyFor(), host)
		k.write(t, "POST /up HTTP/1.1\r\nHost: "+host+"\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n")
		time.Sleep(mitmReadTimeout + 300*time.Millisecond)
		_, _ = io.WriteString(k.conn, "1\r\nb\r\n0\r\n\r\n") // too late; the connection may already be gone
		if status, body, err := k.answer(); err == nil && status == http.StatusOK && strings.HasPrefix(body, "answer") {
			t.Fatalf("stalled upload: status %d body %q, want it cut", status, body)
		}
	})
}
