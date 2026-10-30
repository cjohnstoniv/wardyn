// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestUIGateway_AWaitBehindHeldStreamsEndsInTheCapRefusal: behind maxUIConnsPerRun long-lived
// streams (a websocket, an event stream) no connection comes free, so a request waits no longer
// than uiConnWait and then gets the connection-cap 503 it got at once before requests could wait.
// The streams it queued behind are untouched.
func TestUIGateway_AWaitBehindHeldStreamsEndsInTheCapRefusal(t *testing.T) {
	prev := uiConnWait
	uiConnWait = 300 * time.Millisecond
	t.Cleanup(func() { uiConnWait = prev })
	arrived, release := make(chan struct{}, maxUIConnsPerRun), make(chan struct{})
	h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			arrived <- struct{}{}
			<-release
			return
		}
		_, _ = io.WriteString(w, "sandbox app")
	}))
	cookie := h.openSession()
	streams := make(chan int, maxUIConnsPerRun)
	for range maxUIConnsPerRun {
		go func() { streams <- h.relay("/events", cookie, nil).Code }()
	}
	for range maxUIConnsPerRun {
		<-arrived
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- h.relay("/ide", cookie, nil) }()
	select {
	case rec := <-answered:
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), reasonUIGatewayConnCapReached) {
			t.Errorf("a request behind %d held streams = %d %s, want 503 %s", maxUIConnsPerRun, rec.Code, rec.Body, reasonUIGatewayConnCapReached)
		}
	case <-time.After(uiConnWait + 5*time.Second):
		t.Errorf("a request behind %d held streams was still waiting %v past the %v bound", maxUIConnsPerRun, 5*time.Second, uiConnWait)
	}
	close(release)
	for range maxUIConnsPerRun {
		if code := <-streams; code != http.StatusOK {
			t.Errorf("a held stream answered %d, want 200", code)
		}
	}
}

// TestUIConnWaiter_ADeliveredConnectionIsNeverCancelled: the timer can fire a moment after the
// transport hands the request its connection. Once the connection is delivered, a late timer must
// neither cancel the request nor record the connection-cap refusal.
func TestUIConnWaiter_ADeliveredConnectionIsNeverCancelled(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &uiConnWaiter{base: base, cancel: cancel}
	w.timer = time.AfterFunc(time.Hour, w.expire)
	w.done()
	w.expire() // the timer firing just after the delivery
	if base.Err() != nil {
		t.Fatalf("a request whose connection was delivered was cancelled: %v", base.Err())
	}
}

// TestUIConnWaiter_AnUndeliveredWaitExpires pins the other side: with no connection in hand, the timer
// cancels the request.
func TestUIConnWaiter_AnUndeliveredWaitExpires(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &uiConnWaiter{base: base, cancel: cancel}
	w.timer = time.AfterFunc(time.Hour, w.expire)
	w.expire()
	if base.Err() == nil {
		t.Fatal("an undelivered wait did not cancel the request on expiry")
	}
}

// TestUIConnWait_BoundIsAtLeastFifteenSeconds: a slow but finite backlog must be able to drain
// before the connection-cap refusal (#3170).
func TestUIConnWait_BoundIsAtLeastFifteenSeconds(t *testing.T) {
	if uiConnWait < 15*time.Second {
		t.Errorf("uiConnWait = %v, want at least 15s", uiConnWait)
	}
}

// hangingRecorder holds every write of one action until hung is closed: a stuck audit sink.
type hangingRecorder struct {
	audit.Recorder
	action string
	hung   <-chan struct{}
}

func (r hangingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == r.action {
		<-r.hung
	}
	return r.Recorder.Record(ctx, ev)
}

// TestUIRelay_AHungCloseAuditNeverHoldsTheRelayOpen: a relay connection's exec is closed when the
// connection closes, though its ui.close row cannot be written yet. The row was written first, so a
// stuck audit sink held every closing relay open. The row still lands once the sink answers.
func TestUIRelay_AHungCloseAuditNeverHoldsTheRelayOpen(t *testing.T) {
	h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close") // the transport closes the relay connection after this answer
		_, _ = io.WriteString(w, "sandbox app")
	}))
	hung := make(chan struct{})
	unhang := sync.OnceFunc(func() { close(hung) })
	defer unhang()
	h.srv.cfg.Audit = hangingRecorder{Recorder: h.audit, action: "ui.close", hung: hung}
	execClosed := make(chan struct{})
	var closedOnce sync.Once
	h.runner.mu.Lock()
	dial := h.runner.execFn
	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		sess, err := dial(spec)
		if err != nil || spec.Argv[0] != "socat" {
			return sess, err
		}
		inner := sess.Close
		sess.Close = func() error {
			closedOnce.Do(func() { close(execClosed) })
			return inner()
		}
		return sess, nil
	}
	h.runner.mu.Unlock()
	cookie := h.openSession()

	answered := make(chan int, 1)
	go func() { answered <- h.relay("/ide", cookie, nil).Code }()
	select {
	case <-execClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay's exec was still open 5s after its connection closed, behind a hung ui.close write")
	}
	unhang()
	if code := <-answered; code != http.StatusOK {
		t.Errorf("relay = %d, want 200", code)
	}
	waitFor(t, "the ui.close row", func() bool { return len(h.audit.auditRows("ui.close", "success")) == 1 })
}
