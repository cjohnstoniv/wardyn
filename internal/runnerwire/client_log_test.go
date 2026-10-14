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
	if err := goaway(payload); !errors.Is(err, ErrGoaway) || !strings.Contains(err.Error(), "runner_unclaimed") {
		t.Fatalf("goaway = %v", err)
	}
	if err := goaway(nil); err != ErrGoaway { //nolint:errorlint // the bare sentinel is the expected value
		t.Fatalf("goaway without a reason = %v", err)
	}
}
