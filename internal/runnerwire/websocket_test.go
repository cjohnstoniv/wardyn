// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

func TestStreamURLBinding(t *testing.T) {
	id := uuid.NewString()
	for _, raw := range []string{"", "http://example.com", "https://user@example.com", "https://example.com?", "https://example.com#", "https://example.com?q=x", "ftp://example.com"} {
		if _, err := StreamURL(raw, id); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	got, err := StreamURL("https://example.com/org/", id)
	if err != nil || got != "wss://example.com/org/api/v1/runners/"+id+"/stream" {
		t.Fatalf("stream URL %q: %v", got, err)
	}
	if _, err := StreamURL("https://example.com", "not-a-runner"); err == nil {
		t.Fatal("accepted malformed runner id")
	}
	for _, path := range []string{"/org%2Ftenant", "/org%2F", "/org%20tenant"} {
		got, err := StreamURL("https://example.com"+path+"/", id)
		if err != nil || got != "wss://example.com"+path+"/api/v1/runners/"+id+"/stream" {
			t.Fatalf("org path binding changed: %q %v", got, err)
		}
	}
}

func TestWebSocketFramesAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  websocket.MessageType
		body []byte
		good bool
	}{
		{"binary", websocket.MessageBinary, encodedFrame(t, Frame{Type: TypePing, Payload: EncodePing(time.Now())}), true},
		{"text", websocket.MessageText, []byte("hello"), false},
		{"short", websocket.MessageBinary, []byte{1, 2}, false},
		{"outer bound", websocket.MessageBinary, bytes.Repeat([]byte{'x'}, HeaderLen+MaxControlFrame+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err == nil {
					defer conn.CloseNow()
					_, err = NewWebSocket(conn).ReadFrame(ctx)
				}
				result <- err
			}))
			defer server.Close()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			_ = conn.Write(ctx, tc.typ, tc.body)
			select {
			case err := <-result:
				if (err == nil) != tc.good {
					t.Fatalf("good=%v error=%v", tc.good, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func encodedFrame(t *testing.T, f Frame) []byte {
	t.Helper()
	b, err := f.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWebSocketRefusesRedirect(t *testing.T) {
	targetCalled := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalled <- struct{}{} }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	if _, err := DialWebSocket(context.Background(), source.URL, uuid.NewString(), source.Client()); err == nil {
		t.Fatal("followed redirect")
	}
	select {
	case <-targetCalled:
		t.Fatal("redirect target was contacted")
	default:
	}
}

func TestCheckedEventRefusalDoesNotAdvanceCursor(t *testing.T) {
	a, b := Loopback()
	defer a.Close()
	defer b.Close()
	refused := errors.New("durable store unavailable")
	p := NewPeer(a, PeerConfig{Org: true, OnEventChecked: func(Event) error { return refused }})
	defer p.Close()
	data, _ := Marshal(Event{Kind: EventSpool})
	err := p.handle(Frame{Type: TypeEvent, Seq: 1, Payload: data})
	if !errors.Is(err, refused) || p.LastReceivedSeq() != 0 {
		t.Fatalf("accepted failed ingest: seq=%d err=%v", p.LastReceivedSeq(), err)
	}
}

func TestKeepaliveClosesMissingPong(t *testing.T) {
	a, b := Loopback()
	defer b.Close()
	p := NewPeer(a, PeerConfig{Org: true})
	defer p.Close()
	go p.keepalive(10 * time.Millisecond)
	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("missing PONG left link online")
	}
	if !strings.Contains(p.Err().Error(), "keepalive") {
		t.Fatal(p.Err())
	}
}

func TestReconnectDelayBounded(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		for range 100 {
			d := reconnectDelay(attempt)
			if d < BackoffMin || d > BackoffMax {
				t.Fatalf("attempt %d: %v", attempt, d)
			}
		}
	}
}

func TestResumeCursorReleasesLostAckCapacity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	replay := NewReplayBuffer()
	if err := replay.Reserve(ctx, ReplayBytes); err != nil {
		t.Fatal(err)
	}
	replay.Assign(Frame{Type: TypeEvent, Payload: make([]byte, ReplayBytes)})
	a, b := Loopback()
	defer b.Close()
	p := NewPeer(a, PeerConfig{Replay: replay})
	defer p.Close()
	if err := p.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := replay.Reserve(ctx, 1); err != nil {
		t.Fatalf("lost ACK still pins full replay buffer: %v", err)
	}
	replay.unreserve(1)
}

// An upgrade the far side takes and never answers (a middlebox that holds the request) fails the dial
// at dialTimeout instead of holding the reconnect loop.
func TestDialWebSocketGivesUpOnAnUpgradeThatNeverAnswers(t *testing.T) {
	prev := dialTimeout
	dialTimeout = 100 * time.Millisecond
	t.Cleanup(func() { dialTimeout = prev })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	done := make(chan error, 1)
	go func() {
		_, err := DialWebSocket(context.Background(), server.URL, uuid.NewString(), nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("dial = %v, want the dial deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dial still waits for an upgrade that never answers")
	}
}
