// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"
)

// A guest that fails the proof in a loop leaves the helper log one line, then at most one line a minute
// carrying how many refusals it held back, per port.
func TestPairerLogsRefusalsAtMostOnceAMinute(t *testing.T) {
	now := time.Now()
	old := pairNow
	pairNow = func() time.Time { return now }
	defer func() { pairNow = old }()
	var buf bytes.Buffer
	p := newPairer(AgentPort, nil, nil)
	p.log = slog.New(slog.NewJSONHandler(&buf, nil))

	for range 1000 {
		p.refused()
	}
	now = now.Add(59 * time.Second)
	p.refused()
	now = now.Add(time.Second)
	p.refused()
	p.refused()

	var lines []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("1003 refusals over a minute logged %d lines, want 2:\n%s", len(lines), buf.String())
	}
	for _, m := range lines {
		if m["msg"] != "macvm: refused a guest connection without the pairing proof" || m["port"] != float64(AgentPort) {
			t.Fatalf("log line %v, want the refusal for port %d", m, AgentPort)
		}
	}
	if _, ok := lines[0]["held_back"]; ok {
		t.Fatalf("the first refusal's line %v carries a held-back count", lines[0])
	}
	if lines[1]["held_back"] != float64(1000) {
		t.Fatalf("the minute's line %v, want held_back 1000", lines[1])
	}
}

// A guest connection whose peer stopped reading cannot hold take: the open byte's write times out, the
// connection is closed, and take goes on to wait for another until its context ends.
func TestPairerTakeBoundsTheOpenByteWrite(t *testing.T) {
	old := pairDeadline
	pairDeadline = 100 * time.Millisecond
	defer func() { pairDeadline = old }()
	p := newPairer(AgentPort, nil, []byte{1})
	stuck, peer := net.Pipe() // nothing reads peer, so a write to stuck blocks
	defer peer.Close()
	p.idle = stuck

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.take(ctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, errNoGuest) {
			t.Fatalf("take = %v; want errNoGuest", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("take did not return: the open byte's write has no deadline")
	}
	if _, err := stuck.Write([]byte{1}); err == nil {
		t.Fatal("the connection that timed out was left open")
	}
}
