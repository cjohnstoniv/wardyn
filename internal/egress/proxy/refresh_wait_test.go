// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hangGuard only catches a call that waits on something the test holds back; no test times a call with it.
const hangGuard = 10 * time.Second

// slowestOf runs call n times at once and returns the slowest call's time, failing on any error, and failing
// at once if any call is still out after hangGuard.
func slowestOf(t *testing.T, n int, call func() error) time.Duration {
	t.Helper()
	type result struct {
		err error
		lat time.Duration
	}
	results := make(chan result, n)
	for range n {
		go func() {
			start := time.Now()
			err := call()
			results <- result{err, time.Since(start)}
		}()
	}
	guard := time.After(hangGuard)
	lat := make([]time.Duration, 0, n)
	for len(lat) < n {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("call: %v", r.err)
			}
			lat = append(lat, r.lat)
		case <-guard:
			t.Fatalf("%d of %d calls still out after %v", n-len(lat), n, hangGuard)
		}
	}
	return slices.Max(lat)
}

// returnsWithin runs call and fails at once if it is still out after hangGuard.
func returnsWithin[T any](t *testing.T, what string, call func() T) T {
	t.Helper()
	got := make(chan T, 1)
	go func() { got <- call() }()
	select {
	case v := <-got:
		return v
	case <-time.After(hangGuard):
		t.Fatalf("%s still out after %v", what, hangGuard)
		panic("unreachable")
	}
}

// releaser returns a channel and a function that closes it once, however often it is called.
func releaser() (chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
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
// which is inside its refresh margin but not expired, and the control plane does not answer until those
// requests are back, 20 more requests to the host are each answered with the still-valid credential; the request
// that asked carries the new one.
func TestInjectionRefreshServesTheStillValidHeader(t *testing.T) {
	var asks atomic.Int32
	release, free := releaser()
	defer free() // before the servers close: their handlers wait on it
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		<-release
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
	// The control plane answers only once all 20 are back: a request that waited on the refresh never returns.
	slowest := slowestOf(t, 20, get)
	t.Logf("slowest of 20 requests: %v", slowest) // the row's figure, read off a run without -race
	free()
	if err := returnsWithin(t, "the request that re-resolved", func() error { return <-asked }); err != nil {
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
// re-resolve in flight, and leaves when its own request ends; the control plane does not answer until the end.
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
	if err := returnsWithin(t, "the waiter", func() error { _, _, err := inj.resolveCtx(ctx, host); return err }); err == nil {
		t.Fatal("a waiter whose request ended got a credential")
	}
	if n := asks.Load(); n != 1 {
		t.Errorf("%d re-resolves, want 1: the waiter joins the one in flight", n)
	}
}

// TestAWSRoleCredentialsRefreshInTheBackground: inside the 5-minute renewal margin, 20 concurrent signings are
// each answered with the still-valid role credentials while one refresh, whose portal does not answer until
// they are back, runs behind them.
func TestAWSRoleCredentialsRefreshInTheBackground(t *testing.T) {
	h := newSigV4Harness(t)
	h.lifetime.Store(int64(2 * time.Minute)) // the first credentials arrive inside the margin
	if _, err := h.p.awsSigV4Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.lifetime.Store(0) // the refresh's answer lives an hour
	release, free := releaser()
	defer free()
	h.portalHold.Store(&release)
	before := h.portalCalls.Load()
	sign := func() error {
		c, err := h.p.awsSigV4Credentials(context.Background())
		if err == nil && c.AccessKeyID != sigv4RoleKey {
			err = fmt.Errorf("access key %q", c.AccessKeyID)
		}
		return err
	}
	// The portal answers only once all 20 are back: a signing that waited on the refresh never returns.
	slowest := slowestOf(t, 20, sign)
	t.Logf("slowest of 20 signings: %v", slowest) // the row's figure, read off a run without -race
	free()
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
// refresh in flight, and leaves when its own request ends; the portal does not answer until the end.
func TestAWSRoleCredentialsWaiterLeavesWithItsRequest(t *testing.T) {
	h := newSigV4Harness(t)
	release, free := releaser()
	defer free()
	h.portalHold.Store(&release)
	go func() { _, _ = h.p.awsSigV4Credentials(context.Background()) }()
	waitUntil(t, "the refresh to reach the portal", func() bool { return h.portalCalls.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := returnsWithin(t, "the signing", func() error { _, err := h.p.awsSigV4Credentials(ctx); return err }); err == nil {
		t.Fatal("a signing whose request ended got credentials")
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
	prev := mitmReadTimeout
	mitmReadTimeout = time.Second
	t.Cleanup(func() { mitmReadTimeout = prev })
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
		// One byte every 200 ms: twice the read time in all, with 800 ms of headroom between bytes, so a late
		// wake-up never reaches the read time.
		for i := range 10 {
			time.Sleep(200 * time.Millisecond)
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

// TestAWSRoleCredentialsSecondRefusedRenewalStopsSigning: once awsSigV4RetryAfter has passed after a refused
// renewal, the next signing asks again; a renewal refused again fails that signing too, and the held
// credentials, though not expired, sign nothing.
func TestAWSRoleCredentialsSecondRefusedRenewalStopsSigning(t *testing.T) {
	h := newSigV4Harness(t)
	d := h.p.sigv4
	h.lifetime.Store(int64(2 * time.Minute)) // inside the margin
	if _, err := h.p.awsSigV4Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.portalCode.Store(http.StatusForbidden)
	_, _ = h.p.awsSigV4Credentials(context.Background()) // asks for the renewal, which is refused
	waitUntil(t, "the refused renewal", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.flight == nil && d.lastErr != nil
	})
	d.mu.Lock()
	d.nextAsk = time.Now().Add(-time.Millisecond) // awsSigV4RetryAfter has passed
	d.mu.Unlock()
	if c, err := h.p.awsSigV4Credentials(context.Background()); err == nil || c.HasKeys() {
		t.Fatalf("a renewal refused again answered key %q err %v, want an error and no credentials", c.AccessKeyID, err)
	}
	if n := h.portalCalls.Load(); n != 3 {
		t.Errorf("%d portal calls, want 3: the second renewal is asked", n)
	}
}

// TestInjectionStaleHintDuringRefreshIsKept: Azure DevOps refuses credential A while an expiry refresh is out,
// and the refresh, asked without the hint, answers A again; the stale re-resolve still gets B, the answer to
// the hint, not the refused A.
func TestInjectionStaleHintDuringRefreshIsKept(t *testing.T) {
	var asks atomic.Int32
	release, free := releaser()
	defer free()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := asks.Add(1)
		answer := types.ResolvedInjection{Header: "Authorization", Value: "Bearer A", JTI: "A", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
		if r.URL.Query().Get("stale_jti") == "A" {
			answer.Value, answer.JTI = "Bearer B", "B"
		} else if n == 1 {
			<-release
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(cp.Close)
	const host = "dev.example"
	e := &injEntry{grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer A", jti: "A"},
		expiresAt: time.Now().Add(2 * time.Minute).UnixMilli()} // inside the refresh margin
	inj := &injector{base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(), byHost: map[string]*injEntry{host: e}}
	refreshed := make(chan struct{})
	go func() { defer close(refreshed); _, _, _ = inj.resolveCtx(context.Background(), host) }()
	waitUntil(t, "the refresh to reach the control plane", func() bool { return asks.Load() == 1 })
	stale := make(chan injectedHeader, 1)
	go func() { h, _ := inj.reresolveStale(context.Background(), host, "A"); stale <- h }()
	// The refusal drops A while the refresh is out. Where reMu is held across the refresh instead, dropStale waits
	// and drops what the refresh installed, and the next answer is B either way; TryLock keeps this probe from
	// waiting on that lock too.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if !e.reMu.TryLock() {
			continue
		}
		dropped := e.staleJTI == "A"
		e.reMu.Unlock()
		if dropped {
			break
		}
	}
	free()
	if h := returnsWithin(t, "the stale re-resolve", func() injectedHeader { return <-stale }); h.value != "Bearer B" {
		t.Fatalf("the re-resolve after A was refused got %q, want Bearer B: the stale hint was lost", h.value)
	}
	returnsWithin(t, "the refresh", func() struct{} { return <-refreshed })
}

// endsAsFlightLands is a request context that ends when it is first asked for Done, after land has run: a
// waiter's select then finds both the flight it waits on and its own request ended.
type endsAsFlightLands struct {
	context.Context
	land func()
	once sync.Once
	done chan struct{}
}

func newEndsAsFlightLands(land func()) *endsAsFlightLands {
	return &endsAsFlightLands{Context: context.Background(), land: land, done: make(chan struct{})}
}

func (c *endsAsFlightLands) Done() <-chan struct{} {
	c.once.Do(func() { c.land(); close(c.done) })
	return c.done
}

func (c *endsAsFlightLands) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// waitOn fails at once if ch is still open after hangGuard. Called on the test goroutine only.
func waitOn(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hangGuard):
		t.Fatalf("%s still out after %v", what, hangGuard)
	}
}

// TestInjectionRefreshWaiterWhoseRequestEndedTakesNoAnswer: a waiter whose request ends as the re-resolve
// lands takes no credential, whichever of the two its select sees first. 64 trials: a waiter that took the
// answer whenever select picked the landed flight would pass them all with odds of 2^-64.
func TestInjectionRefreshWaiterWhoseRequestEndedTakesNoAnswer(t *testing.T) {
	answer := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-answer:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: "Bearer new",
			JTI: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}))
	t.Cleanup(cp.Close)
	const host = "api.test"
	for trial := range 64 {
		e := &injEntry{grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer old"},
			expiresAt: time.Now().Add(-time.Second).UnixMilli()} // run out
		inj := &injector{base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(), byHost: map[string]*injEntry{host: e}}
		refreshed := make(chan struct{})
		go func() { defer close(refreshed); _, _, _ = inj.resolveCtx(context.Background(), host) }()
		waitUntil(t, "the re-resolve to open", func() bool {
			e.reMu.Lock()
			defer e.reMu.Unlock()
			return e.refreshing != nil
		})
		ctx := newEndsAsFlightLands(func() {
			select {
			case answer <- struct{}{}:
			case <-time.After(hangGuard):
				t.Fatalf("trial %d: the control plane was never asked", trial)
			}
			waitOn(t, "the re-resolve", refreshed)
		})
		if h, _, err := inj.resolveCtx(ctx, host); err == nil {
			t.Fatalf("trial %d: a waiter whose request ended took the answer %q", trial, h.value)
		}
	}
}

// TestAWSRoleCredentialsWaiterWhoseRequestEndedTakesNoAnswer: a signing whose request ends as the renewal it
// waits on lands takes no credentials, whichever of the two its select sees first (64 trials, as above).
func TestAWSRoleCredentialsWaiterWhoseRequestEndedTakesNoAnswer(t *testing.T) {
	h := newSigV4Harness(t)
	d := h.p.sigv4
	for trial := range 64 {
		d.mu.Lock()
		d.creds = aws.Credentials{} // none held: the signing waits for the renewal it starts
		d.mu.Unlock()
		ctx := newEndsAsFlightLands(func() {
			waitUntil(t, "the renewal to land", func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return d.flight == nil && d.creds.HasKeys()
			})
		})
		if c, err := h.p.awsSigV4Credentials(ctx); err == nil {
			t.Fatalf("trial %d: a signing whose request ended took key %q", trial, c.AccessKeyID)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// resolvedResponse is a control plane's 200 answer to r carrying a fresh credential.
func resolvedResponse(r *http.Request) *http.Response {
	b, _ := json.Marshal(types.ResolvedInjection{Header: "Authorization", Value: "Bearer new",
		JTI: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(b)), Request: r}
}

// TestInjectionRefreshThatPanicsEndsItsFlight: a re-resolve whose ask panics still ends its flight, so the next
// caller re-resolves instead of waiting on it for good.
func TestInjectionRefreshThatPanicsEndsItsFlight(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			panic("the control plane client panicked")
		}
		return resolvedResponse(r), nil
	})}
	const host = "api.test"
	e := &injEntry{grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer old"},
		expiresAt: time.Now().Add(-time.Second).UnixMilli()} // run out
	inj := &injector{base: "http://cp.invalid", token: newTokenSource("RUNTOK"), client: client, byHost: map[string]*injEntry{host: e}}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the ask did not panic")
			}
		}()
		_, _, _ = inj.resolveCtx(context.Background(), host)
	}()
	e.reMu.Lock()
	open := e.refreshing != nil
	e.reMu.Unlock()
	if open {
		t.Fatal("a re-resolve that panicked left its flight open: every later caller waits on it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()
	if h, _, err := inj.resolveCtx(ctx, host); err != nil || h.value != "Bearer new" {
		t.Fatalf("the next caller got %q err %v, want Bearer new", h.value, err)
	}
}

// TestMITMBodyRearmAfterTheRequestEndedSetsNothing: the body re-arm runs on whatever reads the body, the
// transport's write loop among them, which can read after the handler returned while the connection serves its
// next request. A re-arm once the request has ended sets no read deadline, so it cannot cut that next request.
func TestMITMBodyRearmAfterTheRequestEndedSetsNothing(t *testing.T) {
	up := slowUpstream(t)
	const host = "files.example"
	var state atomic.Pointer[networkBody]
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if s, ok := r.Context().Value(networkBodyKey{}).(*networkBody); ok {
			state.Store(s) // the request's tracked body, reached through the context the re-resolve is asked with
		}
		return resolvedResponse(r), nil
	})}
	inj := &injector{base: "http://cp.invalid", token: newTokenSource("RUNTOK"), client: client, byHost: map[string]*injEntry{host: {
		grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer old"},
		expiresAt: time.Now().Add(-time.Second).UnixMilli(), // run out: the request re-resolves with its own context
	}}}
	p, _ := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", upstreamAddr(up), inj, nil)
	p.mitmHosts, p.mitmPorts = map[string]bool{host: true}, map[string]int{host: 443}
	p.mitmPlaintext = map[string]bool{plaintextKey(host, 443): true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+"/up", strings.NewReader("body"))
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	p.serveMITMRequest(w, req, host, 443)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %q, want 200", w.Code, w.Body.String())
	}
	s := state.Load()
	if s == nil || s.rearm == nil {
		t.Fatal("the request's tracked body was not reached")
	}
	cancel() // the handler has returned: net/http ends the request's context
	w.deadline = time.Time{}
	s.done.Store(false) // a late read that brings bytes and is not the last
	s.rearm()
	if !w.deadline.IsZero() {
		t.Fatalf("a re-arm after the request ended set the read deadline to %v: it would cut the next request on the connection", w.deadline)
	}
}
