// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// No sandbox connection open, the caller gives Shutdown
// the same 15s wardyn-proxy main.go gives it, and the sink holds a backlog
// needing ~3s to post. Every row should land.
func TestShutdownDrainUsesTheWholeDeadlineWhenNoConnectionIsOpen(t *testing.T) {
	var rows atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_, _ = io.ReadAll(r.Body)
		rows.Add(1)
	}))
	defer cp.Close()
	sink := newDecisionSink(cp.URL, newTokenSource("t"), 200, cp.Client(), io.Discard)
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}), Sink: sink, Resolver: publicResolver{}})
	for i := 0; i < 100; i++ {
		sink.emit(egress.DecisionLog{Request: egress.Request{Host: "h.example", Port: 443, Time: time.Now()}, Decision: egress.Allow, RuleSource: "probe"})
	}
	srv := &Server{proxy: p, sink: sink, http: &http.Server{}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	err := srv.Shutdown(ctx)
	t.Logf("Shutdown returned after %v: err=%v rows=%d/100", time.Since(start).Round(time.Millisecond), err, rows.Load())
	if err != nil || rows.Load() != 100 {
		t.Errorf("drain cut short: err=%v rows=%d/100", err, rows.Load())
	}
}
