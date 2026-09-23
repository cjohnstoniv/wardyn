// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// TestDecisionSinkCountsTheClosedArmDrop (first half): a decision emitted
// after the sink closed vanished with NO audit row and NO drop count — the
// closed arm returned without touching the counter, so the sink's whole
// "best-effort delivery, but the gap is summarized, not silent" posture had a
// hole exactly where it matters.
//
// It matters at SHUTDOWN specifically: MITM tunnels are served on their own
// hijacked http.Servers that Shutdown never stops, so the last requests of a run
// — the credential-injecting ones — are precisely the ones still emitting while
// the sink closes. Counting them is what keeps close()'s report honest.
//
// (The other half — tracking those inner MITM servers so Shutdown stops them —
// is a design gap, deliberately not taken here.)
func TestDecisionSinkCountsTheClosedArmDrop(t *testing.T) {
	log := decisionLog(egress.Request{Host: "x.test"}, egress.Allow, "policy:allowed")

	t.Run("an emit after close is counted", func(t *testing.T) {
		s := &decisionSink{ch: make(chan egress.DecisionLog, 4)} // out=nil: mirror is a no-op
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for range s.ch { //nolint:revive // drain so close() can complete
			}
		}()
		// NEGATIVE CONTROL: a clean shutdown with nothing dropped still returns nil.
		if err := s.close(context.Background()); err != nil {
			t.Fatalf("clean close = %v, want nil", err)
		}
		s.emit(log)
		if got := s.droppedCount(); got != 1 {
			t.Fatalf("droppedCount after a post-close emit = %d, want 1", got)
		}
	})

	t.Run("close reports a drop that lands while it drains", func(t *testing.T) {
		// A control plane that holds the worker inside post() until released, so
		// close() is provably still draining when the late emit arrives — the real
		// shutdown window, not a simulated one.
		release := make(chan struct{})
		cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/internal/decisions") {
				<-release
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer cp.Close()

		s := newDecisionSink(cp.URL, newTokenSource("tok"), 4, cp.Client(), nil)
		s.emit(log) // the worker picks this up and blocks in post()

		closed := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			closed <- s.close(ctx)
		}()

		// Wait for close() to have marked the sink closed, then emit into the
		// window a shutting-down MITM tunnel emits into.
		deadline := time.Now().Add(2 * time.Second)
		for {
			s.mu.Lock()
			isClosed := s.closed
			s.mu.Unlock()
			if isClosed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("close() never marked the sink closed")
			}
			time.Sleep(time.Millisecond)
		}
		s.emit(log)
		close(release)

		err := <-closed
		if err == nil {
			t.Fatal("close() returned nil; a decision dropped during shutdown must be reported")
		}
		if !strings.Contains(err.Error(), "1 dropped") {
			t.Fatalf("close() error = %v, want it to name the dropped record", err)
		}
	})
}
