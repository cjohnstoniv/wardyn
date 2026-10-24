// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// downEngineStop is StopProxy against an engine that does not answer: every call fails the same way, and
// each one moves the fake clock on a second, as the containment tick does.
type downEngineStop struct {
	calls  *atomic.Int64
	stopAt int64
	cancel context.CancelFunc
}

func (s downEngineStop) StopProxy(context.Context, string) error {
	if s.calls.Add(1) == s.stopAt {
		s.cancel()
	}
	return errors.New("docker unreachable")
}

// Containment that fails every tick keeps being retried every tick, but its ERROR line is logged once and
// then once per growing interval, not once a second (a runner log grew by hundreds of MB a day this way).
func TestContainmentRetryErrorIsLoggedOncePerInterval(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, _, _, org := managerFixture(t)
	createLocal(t, m, org)
	var calls atomic.Int64
	base := time.Now()
	m.mu.Lock()
	m.state.Revoked = true
	m.now = func() time.Time { return base.Add(time.Duration(calls.Load()) * time.Second) }
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m.stopper = downEngineStop{calls: &calls, stopAt: 5, cancel: cancel}
	_ = m.Run(ctx)
	var lines int
	for line := range strings.Lines(buf.String()) {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["level"] == "ERROR" && r["msg"] == "runner containment will retry" {
			lines++
			if r["repeated"] != float64(0) {
				t.Errorf("first line repeated = %v, want 0", r["repeated"])
			}
		}
	}
	if calls.Load() < 5 {
		t.Fatalf("containment tried %d times, want every tick", calls.Load())
	}
	if lines != 1 {
		t.Fatalf("%d ERROR lines for %d identical failures within 5 s, want 1:\n%s", lines, calls.Load(), buf.String())
	}
}
