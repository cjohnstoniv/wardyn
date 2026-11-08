// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// createBackend is a stand-in control plane for POST /api/v1/runs. keyed false behaves like a replica older
// than 0.10.0: it ignores the key and makes a run for every POST.
type createBackend struct {
	mu     sync.Mutex
	keyed  bool
	posts  []string // the Idempotency-Key of each POST
	bodies []string
	health int // GET /healthz calls
	runs   map[string]string
	n      int
	// script, when set, is asked about each POST (1-based) before the backend answers; it returns true if it
	// wrote the answer itself.
	script func(n int, w http.ResponseWriter) bool
	// dropFirst creates the run, then closes the connection without answering.
	dropFirst bool
	// truncateFirst creates the run, then sends headers and half a body.
	truncateFirst bool
}

func (b *createBackend) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if r.URL.Path == "/healthz" {
			b.health++
			if b.keyed {
				_, _ = io.WriteString(w, `{"idempotent_run_create":true}`)
			} else {
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}
			return
		}
		raw, _ := io.ReadAll(r.Body)
		key := r.Header.Get(IdempotencyKeyHeader)
		b.posts = append(b.posts, key)
		b.bodies = append(b.bodies, string(raw))
		n := len(b.posts)
		if b.script != nil && b.script(n, w) {
			return
		}
		id, replayed := "", false
		if b.keyed && key != "" {
			if prev, ok := b.runs[key]; ok {
				id, replayed = prev, true
			}
		}
		if id == "" {
			b.n++
			id = "00000000-0000-0000-0000-00000000000" + string(rune('0'+b.n))
			if b.keyed && key != "" {
				b.runs[key] = id
			}
		}
		if n == 1 && (b.dropFirst || b.truncateFirst) {
			conn, buf, _ := w.(http.Hijacker).Hijack()
			if b.truncateFirst {
				_, _ = buf.WriteString("HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 400\r\n\r\n{\"id\":")
				_ = buf.Flush()
			}
			_ = conn.Close()
			return
		}
		if replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"`+id+`","state":"PENDING"}`)
	})
}

// v returns a copy of the backend's counters, read under its lock so -race sees the handler's writes ordered.
func (b *createBackend) v() createBackendView {
	b.mu.Lock()
	defer b.mu.Unlock()
	return createBackendView{posts: append([]string(nil), b.posts...), bodies: append([]string(nil), b.bodies...), health: b.health, n: b.n, runs: func() map[string]string {
		m := map[string]string{}
		for k, v := range b.runs {
			m[k] = v
		}
		return m
	}()}
}

type createBackendView struct {
	posts, bodies []string
	health, n     int
	runs          map[string]string
}

func newBackend(t *testing.T, keyed bool) (*createBackend, *Client) {
	t.Helper()
	b := &createBackend{keyed: keyed, runs: map[string]string{}}
	srv := httptest.NewServer(b.handler())
	t.Cleanup(srv.Close)
	// A connection the server drops must not be silently re-sent by the transport: that would hide a retry.
	c := &Client{BaseURL: srv.URL, Token: "tok", HTTPClient: &http.Client{
		CheckRedirect: noRedirect,
		Transport:     &http.Transport{DisableKeepAlives: true},
	}}
	return b, c
}

func noSleep(t *testing.T) {
	t.Helper()
	old := createRunBackoff
	createRunBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { createRunBackoff = old })
}

func TestCreateRunSendsKey(t *testing.T) {
	b, c := newBackend(t, true)
	if _, err := c.CreateRun(context.Background(), CreateRunRequest{Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a"}, CreateRunOptions{IdempotencyKey: "build-7-job-2"}); err != nil {
		t.Fatal(err)
	}
	if len(b.v().posts) != 2 || len(b.v().posts[0]) != 36 || b.v().posts[1] != "build-7-job-2" {
		t.Fatalf("keys sent = %q, want a fresh UUID then the chosen key", b.v().posts)
	}
}

func TestCreateRunDefaultDoesNotRetry(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		b, c := newBackend(t, keyed)
		b.dropFirst = true
		_, err := c.CreateRun(context.Background(), CreateRunRequest{Agent: "a"})
		var te *transportError
		if !errors.As(err, &te) {
			t.Fatalf("keyed=%v: err = %v, want the lost answer returned", keyed, err)
		}
		if len(b.v().posts) != 1 || b.v().n != 1 {
			t.Fatalf("keyed=%v: %d POSTs and %d runs, want 1 and 1", keyed, len(b.v().posts), b.v().n)
		}
		if b.v().health != 0 {
			t.Fatalf("keyed=%v: the default call read /healthz", keyed)
		}
	}
}

func TestCreateRunRetrySameKeyAndBytes(t *testing.T) {
	noSleep(t)
	for name, tc := range map[string]func(*createBackend){
		"dropped":   func(b *createBackend) { b.dropFirst = true },
		"truncated": func(b *createBackend) { b.truncateFirst = true },
	} {
		b, c := newBackend(t, true)
		tc(b)
		res, err := c.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a", Task: "t"}, CreateRunOptions{Retry: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(b.v().posts) != 2 || b.v().posts[0] == "" || b.v().posts[0] != b.v().posts[1] || b.v().bodies[0] != b.v().bodies[1] {
			t.Fatalf("%s: POSTs %q bodies %q, want one key and one body twice", name, b.v().posts, b.v().bodies)
		}
		if b.v().n != 1 || !res.Replayed || res.ID.String() != b.v().runs[b.v().posts[0]] {
			t.Fatalf("%s: runs=%d replayed=%v id=%s, want one run replayed", name, b.v().n, res.Replayed, res.ID)
		}
		if b.v().health != 1 {
			t.Fatalf("%s: /healthz read %d times, want 1", name, b.v().health)
		}
	}
}

func TestCreateRunNoRetryAgainstOldServer(t *testing.T) {
	noSleep(t)
	b, c := newBackend(t, false)
	b.dropFirst = true
	_, err := c.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a"}, CreateRunOptions{Retry: true})
	if err == nil || !strings.Contains(err.Error(), "may have been created") {
		t.Fatalf("err = %v, want the run-may-exist advice", err)
	}
	var te *transportError
	if !errors.As(err, &te) {
		t.Fatalf("the original error is not wrapped: %v", err)
	}
	if len(b.v().posts) != 1 || b.v().n != 1 || b.v().health != 1 {
		t.Fatalf("%d POSTs, %d runs, %d healthz reads; want 1, 1, 1", len(b.v().posts), b.v().n, b.v().health)
	}
}

func TestCreateRunRetriesInFlight409(t *testing.T) {
	noSleep(t)
	b, c := newBackend(t, true)
	b.script = func(n int, w http.ResponseWriter) bool {
		if n > 1 {
			return false
		}
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"x","reason":"idempotency_key_in_flight"}`)
		return true
	}
	if _, err := c.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a"}, CreateRunOptions{Retry: true}); err != nil {
		t.Fatal(err)
	}
	if len(b.v().posts) != 2 || b.v().posts[0] != b.v().posts[1] || b.v().health != 0 {
		t.Fatalf("POSTs %q healthz %d, want the same key twice and no probe", b.v().posts, b.v().health)
	}
}

func TestCreateRunInFlightGivesUpAfterThreeAttempts(t *testing.T) {
	noSleep(t)
	b, c := newBackend(t, true)
	b.script = func(_ int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"x","reason":"idempotency_key_in_flight"}`)
		return true
	}
	_, err := c.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a"}, CreateRunOptions{Retry: true})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || len(b.v().posts) != 3 {
		t.Fatalf("err = %v after %d POSTs, want the 409 after 3", err, len(b.v().posts))
	}
}

func TestCreateRunNoRetryOnWardynReason503(t *testing.T) {
	noSleep(t)
	b, c := newBackend(t, true)
	b.script = func(_ int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"pool full","reason":"pool_full"}`)
		return true
	}
	_, err := c.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a"}, CreateRunOptions{Retry: true})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Reason != "pool_full" || len(b.v().posts) != 1 {
		t.Fatalf("err = %v after %d POSTs, want the refusal after 1", err, len(b.v().posts))
	}
}

func TestCreateRunRetriesProxy503AndStopsOnOtherRefusals(t *testing.T) {
	noSleep(t)
	b, c := newBackend(t, true)
	b.script = func(n int, w http.ResponseWriter) bool {
		if n > 1 {
			return false
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
		return true
	}
	if _, err := c.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a"}, CreateRunOptions{Retry: true}); err != nil {
		t.Fatal(err)
	}
	if len(b.v().posts) != 2 || b.v().health != 1 {
		t.Fatalf("%d POSTs, %d healthz reads, want 2 and 1", len(b.v().posts), b.v().health)
	}
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusConflict, http.StatusInternalServerError} {
		b2, c2 := newBackend(t, true)
		b2.script = func(_ int, w http.ResponseWriter) bool {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"no","reason":"other"}`)
			return true
		}
		if _, err := c2.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a"}, CreateRunOptions{Retry: true}); err == nil || len(b2.v().posts) != 1 {
			t.Fatalf("status %d: err=%v after %d POSTs, want 1", status, err, len(b2.v().posts))
		}
	}
}

func TestCreateRunRetryStopsWithContext(t *testing.T) {
	old := createRunBackoff
	createRunBackoff = []time.Duration{time.Hour, time.Hour}
	t.Cleanup(func() { createRunBackoff = old })
	b, c := newBackend(t, true)
	b.dropFirst = true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.CreateRunWith(ctx, CreateRunRequest{Agent: "a"}, CreateRunOptions{Retry: true}); err == nil {
		t.Fatal("want the lost answer returned when ctx ends during the wait")
	}
	if time.Since(start) > 5*time.Second || len(b.v().posts) != 1 {
		t.Fatalf("took %v with %d POSTs", time.Since(start), len(b.v().posts))
	}
}

func TestRetryAfterIsHonouredAndCapped(t *testing.T) {
	h := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	for _, tc := range []struct {
		hdr  http.Header
		want time.Duration
	}{
		{h("1"), time.Second}, {h("3"), 3 * time.Second}, {h("999"), maxRetryAfter},
		{h("soon"), 7 * time.Millisecond}, {h("-4"), 7 * time.Millisecond}, {http.Header{}, 7 * time.Millisecond},
	} {
		if got := retryAfter(tc.hdr, 7*time.Millisecond); got != tc.want {
			t.Errorf("Retry-After %q = %v, want %v", tc.hdr.Get("Retry-After"), got, tc.want)
		}
	}
}

// keepAliveClient uses the stock transport with keep-alives ON, as the SDK's default client does. The
// Idempotency-Key header makes net/http treat a body-bearing POST as replayable, so on a reused connection
// that dies before the response headers the transport would send it again by itself.
func keepAliveClient(url string) *Client {
	return &Client{BaseURL: url, Token: "tok", HTTPClient: &http.Client{
		CheckRedirect: noRedirect,
		Transport:     &http.Transport{},
	}}
}

// dropAfterCommit counts the run creates a server committed. After the connection is warmed it commits each POST
// and then closes the connection before sending any header.
type dropAfterCommit struct {
	mu      sync.Mutex
	creates int
	keyed   bool
}

func (d *dropAfterCommit) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if d.keyed {
				_, _ = io.WriteString(w, `{"idempotent_run_create":true}`)
			} else {
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}
			return
		}
		_, _ = io.ReadAll(r.Body)
		d.mu.Lock()
		d.creates++
		d.mu.Unlock()
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
}

func (d *dropAfterCommit) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.creates
}

// TestCreateRunDefaultIssuesOnePostOnAWarmConnection: the default call is one POST even when a reused keep-alive
// connection resets after the server committed the run, against a server that predates the key.
func TestCreateRunDefaultIssuesOnePostOnAWarmConnection(t *testing.T) {
	d := &dropAfterCommit{}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	c := keepAliveClient(srv.URL)
	if _, err := c.Healthz(context.Background()); err != nil { // warms the connection
		t.Fatal(err)
	}
	if _, err := c.CreateRun(context.Background(), CreateRunRequest{Agent: "a"}); err == nil {
		t.Fatal("want the lost answer returned")
	}
	if got := d.count(); got != 1 {
		t.Fatalf("server committed %d creates, want exactly 1", got)
	}
}

// TestCreateRunRetryCountsOnlyTheLoopsAttempts: with Retry on, a server that predates the key but is reported as
// supporting it is retried only by the SDK loop, so the server sees one POST per attempt and no more.
func TestCreateRunRetryCountsOnlyTheLoopsAttempts(t *testing.T) {
	noSleep(t)
	d := &dropAfterCommit{keyed: true}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	c := keepAliveClient(srv.URL)
	if _, err := c.Healthz(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRunWith(context.Background(), CreateRunRequest{Agent: "a"}, CreateRunOptions{Retry: true}); err == nil {
		t.Fatal("want the lost answer returned after the attempts")
	}
	if got := d.count(); got != createRunAttempts {
		t.Fatalf("server saw %d POSTs, want %d (the loop's attempts)", got, createRunAttempts)
	}
}
