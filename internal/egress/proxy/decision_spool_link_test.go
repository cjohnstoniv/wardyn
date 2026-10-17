// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runneractivity"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// errLinkDown stands in for errLinkDown, which this package cannot import (cycle).
var errLinkDown = errors.New("runnerwire: link down")

func linkTestSink(t *testing.T) (*decisionSink, *Proxy, string, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close(context.Background()) })
	return s, &Proxy{sink: s}, dir, id
}

// serveOne admits r and, when admitted, records its allowed decision as a served request does.
func serveOne(p *Proxy, id uuid.UUID, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	admitted, release := p.admitSpoolHTTP(w, r, "")
	if admitted != nil {
		p.sink.emit(egress.DecisionLog{Request: egress.Request{RunID: id, Host: "allowed.test", Method: r.Method, Time: time.Now(), AuditReservation: spoolTicket(admitted.Context())}, Decision: egress.Allow, RuleSource: "policy:allowed"})
		release()
	}
	return w
}

func setHeld(s *decisionSink, n int64) { s.mu.Lock(); s.heldBytes = n; s.mu.Unlock() }

func spoolStats(t *testing.T, s *decisionSink) (int, uint64) {
	t.Helper()
	depth, _, drops, err := s.local.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return depth, drops
}

func shortReq(t *testing.T, target string, d time.Duration) *http.Request {
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return httptest.NewRequest("GET", target, nil).WithContext(ctx)
}

// The runner's link to the organisation drops mid-run: replay fails with ErrLinkDown and nothing is
// acknowledged. One admission is refused while in-flight requests briefly fill the spool. Once they
// finish there is room again, and the next request (the model call) must be served while the link is
// still down, not refused until reconnect. On reconnect the refusal is reported first.
func TestLocalSpoolLinkDropKeepsAdmitting(t *testing.T) {
	s, p, dir, id := linkTestSink(t)
	ctx := t.Context()
	runnerd, err := runnerspool.Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer runnerd.Close()
	linkDown := func(context.Context, []runnerspool.Record) error { return errLinkDown }

	if err := runneractivity.Publish(dir, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if w := serveOne(p, id, httptest.NewRequest("GET", "http://allowed.test/before", nil)); w.Code != 200 {
		t.Fatalf("link up: status %d", w.Code)
	}

	if err := runneractivity.Publish(dir, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if err := runnerd.Replay(ctx, linkDown); !errors.Is(err, errLinkDown) {
		t.Fatalf("replay: %v, want link down", err)
	}
	setHeld(s, runnerspool.MaxBytes)
	w := serveOne(p, id, shortReq(t, "http://allowed.test/while-full", 300*time.Millisecond))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("full spool: status %d, want 503", w.Code)
	}
	if _, drops := spoolStats(t, s); drops != 1 {
		t.Fatalf("drops %d, want the full spool's refusal counted once", drops)
	}
	setHeld(s, 0)
	if err := runnerd.Replay(ctx, linkDown); !errors.Is(err, errLinkDown) {
		t.Fatalf("replay: %v, want link down", err)
	}
	if w := serveOne(p, id, httptest.NewRequest("POST", "http://allowed.test/v1/messages", nil)); w.Code != 200 {
		t.Fatalf("link down, spool has room: status %d body %q, want the request served", w.Code, w.Body.String())
	}
	if depth, drops := spoolStats(t, s); depth != 2 || drops != 1 {
		t.Fatalf("depth %d drops %d, want both served records and one refusal", depth, drops)
	}

	if err := runneractivity.Publish(dir, uuid.New()); err != nil {
		t.Fatal(err)
	}
	var sent []runnerspool.Record
	if err := runnerd.Replay(ctx, eachRow(func(r runnerspool.Record) error { sent = append(sent, r); return nil })); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[0].Decision.RuleSource != "builtin:spool-full" || sent[0].Count != 1 {
		t.Fatalf("reconnect replay %d rows, first %+v; want the refusal first, then both records", len(sent), sent[0])
	}
	if w := serveOne(p, id, httptest.NewRequest("GET", "http://allowed.test/after-reconnect", nil)); w.Code != 200 {
		t.Fatalf("after reconnect: status %d", w.Code)
	}
}

// A request that finds the spool full is held for room within its admission deadline: room that
// appears while it waits serves it, and nothing is counted.
func TestLocalSpoolHoldsARequestForRoom(t *testing.T) {
	s, p, _, id := linkTestSink(t)
	setHeld(s, runnerspool.MaxBytes)
	go func() { time.Sleep(300 * time.Millisecond); setHeld(s, 0) }()
	if w := serveOne(p, id, httptest.NewRequest("GET", "http://allowed.test/held", nil)); w.Code != 200 {
		t.Fatalf("held request: status %d body %q, want it served once room appeared", w.Code, w.Body.String())
	}
	if _, drops := spoolStats(t, s); drops != 0 {
		t.Fatalf("drops %d, want none for a request that was served", drops)
	}
}

// A sustained outage fills the spool: it then fails closed and counts each refusal until replay
// drains it. The runner's lapse rule bounds how long the proxy keeps running offline at all.
func TestLocalSpoolSustainedOutageFailsClosedWhenFull(t *testing.T) {
	s, p, dir, id := linkTestSink(t)
	big := egress.DecisionLog{Request: egress.Request{RunID: id, Host: "allowed.test", Path: strings.Repeat("x", runnerspool.MaxRecordBytes-1024), Time: time.Now()}, Decision: egress.Allow, RuleSource: "policy:allowed"}
	// Fill with large records, then smaller ones, until less room is left than a request reserves.
	for _, n := range []int{runnerspool.MaxRecordBytes - 1024, 16 << 10} {
		big.Request.Path = strings.Repeat("x", n)
		for {
			err := s.local.AppendAdmitted(t.Context(), big)
			if errors.Is(err, runnerspool.ErrFull) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	_, base := spoolStats(t, s)
	for i := range 2 {
		if w := serveOne(p, id, shortReq(t, "http://allowed.test/outage", 200*time.Millisecond)); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("refusal %d: status %d, want 503 from a full spool", i+1, w.Code)
		}
	}
	if _, drops := spoolStats(t, s); drops != base+2 {
		t.Fatalf("drops %d, want %d: each refusal counted once", drops, base+2)
	}
	runnerd, err := runnerspool.Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer runnerd.Close()
	if err := runnerd.Replay(t.Context(), eachRow(func(runnerspool.Record) error { return nil })); err != nil {
		t.Fatal(err)
	}
	if w := serveOne(p, id, httptest.NewRequest("GET", "http://allowed.test/drained", nil)); w.Code != 200 {
		t.Fatalf("after replay: status %d", w.Code)
	}
}

// wardyn-runnerd holding the spool lock past the old 5 s append bound delays a served request's record;
// it does not lose it or close admission for the rest of the run.
func TestLocalSpoolSlowLockDoesNotLatch(t *testing.T) {
	s, p, dir, id := linkTestSink(t)
	w := httptest.NewRecorder()
	admitted, release := p.admitSpoolHTTP(w, httptest.NewRequest("GET", "http://allowed.test/a", nil), "")
	if admitted == nil {
		t.Fatalf("status %d", w.Code)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fd := int(lock.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	unlocked := make(chan struct{})
	go func() { defer close(unlocked); time.Sleep(6 * time.Second); _ = unix.Flock(fd, unix.LOCK_UN) }()
	defer func() { <-unlocked; lock.Close() }()
	s.emit(egress.DecisionLog{Request: egress.Request{RunID: id, Host: "allowed.test", Method: "GET", Time: time.Now(), AuditReservation: spoolTicket(admitted.Context())}, Decision: egress.Allow, RuleSource: "policy:allowed"})
	release()
	if depth, _ := spoolStats(t, s); depth != 1 {
		t.Fatalf("depth %d, want the served request's record durable", depth)
	}
	if w := serveOne(p, id, httptest.NewRequest("GET", "http://allowed.test/b", nil)); w.Code != 200 {
		t.Fatalf("after a slow lock: status %d body %q, want admission open", w.Code, w.Body.String())
	}
}

// A served request whose record cannot land is an audit gap and still closes admission; a failed
// record for a request that was never served does not.
func TestLocalSpoolLatchesOnlyALostServedRecord(t *testing.T) {
	s, p, _, id := linkTestSink(t)
	s.emit(egress.DecisionLog{Request: egress.Request{RunID: uuid.New(), Host: "denied.test", Time: time.Now()}, Decision: egress.Deny, RuleSource: "policy:denied"})
	if w := serveOne(p, id, httptest.NewRequest("GET", "http://allowed.test/after-deny", nil)); w.Code != 200 {
		t.Fatalf("an unserved record's failure closed admission: status %d", w.Code)
	}
	w := httptest.NewRecorder()
	admitted, release := p.admitSpoolHTTP(w, httptest.NewRequest("GET", "http://allowed.test/lost", nil), "")
	if admitted == nil {
		t.Fatalf("status %d", w.Code)
	}
	s.emit(egress.DecisionLog{Request: egress.Request{RunID: uuid.New(), Host: "allowed.test", Time: time.Now(), AuditReservation: spoolTicket(admitted.Context())}, Decision: egress.Allow, RuleSource: "policy:allowed"})
	release()
	if w := serveOne(p, id, shortReq(t, "http://allowed.test/after-loss", 200*time.Millisecond)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a lost served record left admission open: status %d", w.Code)
	}
}

// A full spool's refusal is one the SDKs retry: 503 with X-Should-Retry: true and a Retry-After of at
// most 60 s (anthropic-sdk-python 0.86.0 _base_client.py: x-should-retry, then status >= 500; retry-after
// honoured when 0 < value <= 60; openai-python _base_client.py the same), in the SDK's own error body.
func TestSpoolRefusalShapeIsRetryable(t *testing.T) {
	for _, tc := range []struct{ name, mitm string }{{"plain", ""}, {"anthropic", anthropicHost}, {"openai", openaiHost}} {
		for _, up := range []bool{false, true} {
			s, p, dir, _ := linkTestSink(t)
			if up {
				if err := runneractivity.Publish(dir, uuid.New()); err != nil {
					t.Fatal(err)
				}
			}
			setHeld(s, runnerspool.MaxBytes)
			w := httptest.NewRecorder()
			r := shortReq(t, "http://allowed.test/v1/messages", 150*time.Millisecond)
			if tc.mitm != "" {
				r = shortReq(t, "https://"+tc.mitm+"/v1/messages", 150*time.Millisecond)
			}
			if admitted, _ := p.admitSpoolHTTP(w, r, tc.mitm); admitted != nil {
				t.Fatalf("%s: admitted into a full spool", tc.name)
			}
			ra, err := strconv.Atoi(w.Header().Get("Retry-After"))
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("X-Should-Retry") != "true" || err != nil || ra <= 0 || ra > 60 || (tc.mitm != "" && w.Header().Get(egressHeaderWardynReason) != reasonAuditUnavailable) {
				t.Fatalf("%s: status %d headers %v, want a retryable 503", tc.name, w.Code, w.Header())
			}
			var msg string
			switch tc.name {
			case "anthropic":
				var b struct {
					Type  string `json:"type"`
					Error struct{ Type, Message string }
				}
				if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Type != "error" || b.Error.Type == "" {
					t.Fatalf("anthropic body %q: %v", w.Body.String(), err)
				}
				msg = b.Error.Message
			case "openai":
				var b struct {
					Error struct{ Message, Type, Code string }
				}
				if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Error.Code != reasonAuditUnavailable || b.Error.Type == "" {
					t.Fatalf("openai body %q: %v", w.Body.String(), err)
				}
				msg = b.Error.Message
			default:
				msg = w.Body.String()
			}
			if names := strings.Contains(msg, "link to the organisation is down"); names == up {
				t.Fatalf("%s link up=%v: message %q", tc.name, up, msg)
			}
		}
	}
}
