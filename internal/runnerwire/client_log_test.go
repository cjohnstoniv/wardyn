// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// records returns each logged line at level.
func (l *logBuffer) records(t *testing.T, level string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(l.b.String()) {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["level"] == level {
			out = append(out, r)
		}
	}
	return out
}

// CaptureLog routes the default logger into a buffer for the test's duration.
func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	logs := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

func idleHooks() RunnerHooks {
	return RunnerHooks{
		Call:        func(context.Context, *Peer, uint32, string, json.RawMessage) (any, error) { return nil, nil },
		Open:        func(*Peer, *Stream, Open) error { return nil },
		Clock:       func(Hello, time.Time) error { return nil },
		Pending:     func(context.Context, *Peer, []PendingAction) error { return nil },
		StatePages:  func(context.Context, func(StatePage) error) error { return nil },
		Replay:      func(context.Context, *Peer) error { return nil },
		BeforeReady: func(context.Context, *Peer, bool) error { return nil },
		Lease:       func(context.Context, Lease) error { return nil },
		Revoked:     func(context.Context) error { return nil },
		Offline:     func(context.Context) {},
	}
}

func TestClientRunWarnsOnceWithTheOrgRefusalReason(t *testing.T) {
	logs := captureLog(t)
	dials := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dials <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"runner stream connection rate exceeded","reason":"runner_stream_rate_limited"}`))
	}))
	defer server.Close()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	client, err := NewClient(ClientConfig{OrgURL: server.URL, RunnerID: uuid.NewString(), PrivateKey: key, Hooks: idleHooks()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	// Run logs a failure before it waits to redial, so the second dial
	// proves the first failure was already logged.
	for range 2 {
		select {
		case <-dials:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	cancel()
	<-done
	warns := logs.records(t, "WARN")
	if len(warns) != 1 {
		t.Fatalf("got %d warnings for one repeated refusal, want 1: %v", len(warns), warns)
	}
	msg, _ := warns[0]["err"].(string)
	if !strings.Contains(msg, "429") || !strings.Contains(msg, "runner_stream_rate_limited") || warns[0]["retry_in"] == nil {
		t.Fatalf("warning lacks the status, reason code or retry delay: %v", warns[0])
	}
}

func TestConnectLogWarnsWhenTheReasonChanges(t *testing.T) {
	logs := captureLog(t)
	var l connectLog
	refused, auth := errors.New("refused"), ErrAuth
	for _, err := range []error{refused, refused, auth, nil, auth} {
		l.failed(t.Context(), err, time.Second)
	}
	warns := logs.records(t, "WARN")
	if len(warns) != 2 || warns[0]["err"] != refused.Error() || warns[1]["err"] != auth.Error() {
		t.Fatalf("warnings = %v, want one per distinct reason", warns)
	}
}

func TestGoawayCarriesTheOrgReason(t *testing.T) {
	payload, err := Marshal(Reason{Reason: "runner_unclaimed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := goaway(payload); !errors.Is(err, ErrGoaway) || !strings.Contains(err.Error(), "runner_unclaimed") || !strings.Contains(err.Error(), "wardyn runner claim") {
		t.Fatalf("goaway = %v, want the reason code and what to do", err)
	}
	if err := goaway(nil); err != ErrGoaway { //nolint:errorlint // the bare sentinel is the expected value
		t.Fatalf("goaway without a reason = %v", err)
	}
}

// A runner whose connection attempt waits says it is still offline, with the last failure, at
// offlineLogEvery: offline and silent cannot pass for connected.
func TestClientRunSaysItIsStillOfflineWhileAConnectWaits(t *testing.T) {
	logs := captureLog(t)
	prev := offlineLogEvery
	offlineLogEvery = 20 * time.Millisecond
	t.Cleanup(func() { offlineLogEvery = prev })
	waiting := make(chan struct{})
	dials := 0
	dial := func(ctx context.Context) (Conn, error) {
		if dials++; dials == 1 {
			return nil, errors.New("boom")
		}
		close(waiting)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	client, err := NewClient(ClientConfig{OrgURL: "https://org.example", RunnerID: uuid.NewString(), PrivateKey: key, Hooks: idleHooks(), Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("the second dial never started")
	}
	offline := func() []map[string]any {
		var out []map[string]any
		for _, r := range logs.records(t, "WARN") {
			if r["msg"] == "runner still offline, retrying" {
				out = append(out, r)
			}
		}
		return out
	}
	before := len(offline())
	for len(offline()) <= before && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	got := offline() // read while the dial still waits, before cancelling changes the last error
	cancel()
	<-done
	if len(got) <= before {
		t.Fatal("no \"runner still offline, retrying\" while the dial waited")
	}
	if last := got[len(got)-1]; last["last_error"] != "boom" || last["offline_for"] == nil {
		t.Fatalf("offline warning lacks the last failure or how long: %v", last)
	}
}

// A link that is up logs nothing.
func TestOfflineLogIsQuietWhileTheLinkIsUp(t *testing.T) {
	logs := captureLog(t)
	prev := offlineLogEvery
	offlineLogEvery = time.Millisecond
	t.Cleanup(func() { offlineLogEvery = prev })
	var l linkState
	l.set(true, "")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	l.logWhileOffline(ctx)
	if warns := logs.records(t, "WARN"); len(warns) != 0 {
		t.Fatalf("an up link logged %v", warns)
	}
}

// An attempt that is stuck says which step it is on and how long ago the last one failed, so it reads
// differently from one that keeps failing.
func TestOfflineLogSaysWhatItWaitsOn(t *testing.T) {
	logs := captureLog(t)
	prev := offlineLogEvery
	offlineLogEvery = 20 * time.Millisecond
	t.Cleanup(func() { offlineLogEvery = prev })
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	const orgURL = "https://org.example"
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(orgURL), State: types.RunnerClaimed}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	inventory := make(chan struct{})
	dials := 0
	dial := func(context.Context) (Conn, error) {
		if dials++; dials == 1 {
			return nil, errors.New("boom")
		}
		org, run := Loopback()
		go func() {
			defer org.Close()
			if _, err := AuthenticateRunner(ctx, org, row, orgURL); err != nil {
				return
			}
			if WriteControl(ctx, org, TypeAuthOK, AuthOK{SessionID: uuid.NewString(), PingInterval: PingInterval}) != nil {
				return
			}
			peer := NewPeer(org, PeerConfig{Org: true})
			if peer.Start(ctx, 0) != nil {
				return
			}
			defer peer.Close()
			if payload, err := Marshal([]PendingAction{}); err == nil {
				_ = peer.Send(ctx, Frame{Type: TypePending, Payload: payload})
			}
			<-ctx.Done()
		}()
		return run, nil
	}
	hooks := idleHooks()
	hooks.StatePages = func(ctx context.Context, _ func(StatePage) error) error {
		close(inventory)
		<-ctx.Done()
		return ctx.Err()
	}
	client, err := NewClient(ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-inventory:
	case <-ctx.Done():
		t.Fatal("the attempt never reached the inventory")
	}
	var got map[string]any
	for got == nil && ctx.Err() == nil {
		for _, r := range logs.records(t, "WARN") {
			if r["msg"] == "runner still offline, retrying" && r["waiting_on"] == "inventory" {
				got = r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got == nil {
		t.Fatal("no offline warning named the inventory as the step it waits on")
	}
	if got["last_error"] != "boom" || got["last_error_age"] == nil {
		t.Fatalf("offline warning lacks the last failure or its age: %v", got)
	}
}
