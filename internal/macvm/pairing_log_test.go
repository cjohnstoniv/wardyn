// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
