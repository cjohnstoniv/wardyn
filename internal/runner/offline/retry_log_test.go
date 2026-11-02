// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
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

// refStop fails StopProxy for one ref every time and records, on the fake clock, when each other ref
// was first stopped.
type refStop struct {
	failing string
	now     func() time.Time
	mu      sync.Mutex
	calls   map[string]int
	first   map[string]time.Time
}

func (s *refStop) StopProxy(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[ref]++
	if ref == s.failing {
		return errors.New("docker unreachable")
	}
	if _, ok := s.first[ref]; !ok {
		s.first[ref] = s.now()
	}
	return nil
}

// One run's containment fails on every pass for ten minutes of fake time. It is still tried on every
// pass; a run that lapses five minutes in gets its first containment within one controlled fake-clock
// iteration, with no added backoff from the other's failures (not a wall-clock guarantee: a pass can
// wait on its backend's timeouts); and the repeated ERROR line is one line plus at most one a minute.
func TestContainmentFailureDelaysNoOtherRunAndLogsBounded(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, _, _, org := managerFixture(t)
	failing, lapsing := createLocal(t, m, org), createLocal(t, m, org)
	var seconds atomic.Int64
	base := time.Now()
	now := func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	stop := &refStop{now: now, calls: map[string]int{}, first: map[string]time.Time{}}
	const lapseAt, total = 300, 600
	ticks := make(chan time.Time)
	m.mu.Lock()
	r := m.state.Runs[failing]
	r.FailedCreation = true
	m.state.Runs[failing] = r
	stop.failing = r.Ref
	lapsingRef := m.state.Runs[lapsing].Ref
	m.now, m.stopper, m.ticks = now, stop, ticks
	// Each pass starts one fake second after the last; the second run lapses at lapseAt.
	m.tick = func(context.Context) {
		if seconds.Add(1) == lapseAt {
			m.mu.Lock()
			r := m.state.Runs[lapsing]
			r.FailedCreation = true
			m.state.Runs[lapsing] = r
			m.mu.Unlock()
		}
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- m.Run(ctx) }()
	for range total - 1 {
		ticks <- time.Time{}
	}
	ticks <- time.Time{} // returns once the last pass has run
	cancel()
	<-done
	stop.mu.Lock()
	defer stop.mu.Unlock()
	if got := stop.calls[stop.failing]; got < total {
		t.Errorf("the failing run was tried %d times in %d ticks, want every tick", got, total)
	}
	first, ok := stop.first[lapsingRef]
	if !ok || first.Sub(base) != lapseAt*time.Second {
		t.Fatalf("the run that lapsed at %ds was first contained at %v (tried=%v), want within that fake-clock iteration", lapseAt, first.Sub(base), ok)
	}
	var lines int
	for line := range strings.Lines(buf.String()) {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["level"] == "ERROR" && r["msg"] == "runner containment will retry" {
			lines++
		}
	}
	if lines == 0 || lines > 1+total/60 {
		t.Errorf("%d ERROR lines over %d s of the same failure, want one plus at most one a minute", lines, total)
	}
}

// phaseStop is StopProxy whose answer is chosen by the fake second of each call; it records how many
// calls each second saw.
type phaseStop struct {
	seconds *atomic.Int64
	answer  func(second int64) error
	mu      sync.Mutex
	calls   map[int64]int
}

func (s *phaseStop) StopProxy(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec := s.seconds.Load()
	s.calls[sec]++
	return s.answer(sec)
}

// One run's containment fails the same way for 70 fake seconds, then with a changed error, then holds
// (#3704). It is tried exactly once on every pass throughout; the identical failure logs one line and a
// one-minute summary, the changed error logs at once, and so does the success. CheckLapses and
// BeforeReady still return every failure: only the line is spaced out.
func TestContainmentLogIsPerRunSummarisedAndAttemptsAreUnchanged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, _, _, org := managerFixture(t)
	failing := createLocal(t, m, org)
	var seconds atomic.Int64
	base := time.Now()
	down, changed := errors.New("docker unreachable"), errors.New("docker: stop proxy: error during connect")
	stop := &phaseStop{seconds: &seconds, calls: map[int64]int{}, answer: func(sec int64) error {
		switch {
		case sec <= 70:
			return down
		case sec <= 75:
			return changed
		}
		return nil
	}}
	const total = 80
	ticks := make(chan time.Time)
	m.mu.Lock()
	r := m.state.Runs[failing]
	r.FailedCreation = true
	m.state.Runs[failing] = r
	m.now = func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	m.stopper, m.ticks = stop, ticks
	m.tick = func(context.Context) { seconds.Add(1) } // each pass starts one fake second after the last
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- m.Run(ctx) }()
	for range total {
		ticks <- time.Time{} // the last returns once pass total has run
	}
	cancel()
	<-done

	stop.mu.Lock()
	for sec := int64(1); sec <= total; sec++ {
		if stop.calls[sec] != 1 {
			t.Errorf("pass %d tried containment %d times, want exactly 1 on every pass", sec, stop.calls[sec])
		}
	}
	stop.mu.Unlock()
	type line struct{ msg, err string }
	var lines []line
	var repeated []float64
	for l := range strings.Lines(buf.String()) {
		var r map[string]any
		if json.Unmarshal([]byte(l), &r) != nil || r["run_id"] != failing.String() {
			continue
		}
		if msg := r["msg"].(string); msg == "runner containment will retry" || msg == "runner containment recovered" {
			e, _ := r["error"].(string)
			lines = append(lines, line{msg, e})
			repeated = append(repeated, r["repeated"].(float64))
		}
	}
	want := []line{
		{"runner containment will retry", "contain run " + failing.String() + ": docker unreachable"},
		{"runner containment will retry", "contain run " + failing.String() + ": docker unreachable"},
		{"runner containment will retry", "contain run " + failing.String() + ": " + changed.Error()},
		{"runner containment recovered", ""},
	}
	// The recovery counts every repeat not logged: 9 of the first error after its summary, 4 of the changed one.
	if !slices.Equal(lines, want) || !slices.Equal(repeated, []float64{0, 59, 0, 13}) {
		t.Fatalf("lines %v repeated %v; want %v repeated [0 59 0 13]:\n%s", lines, repeated, want, buf.String())
	}

	// The refusal is unchanged: a failing pass still fails CheckLapses and BeforeReady every time.
	stop.answer = func(int64) error { return down }
	for range 3 {
		if err := m.CheckLapses(t.Context()); !errors.Is(err, down) || !strings.Contains(err.Error(), "contain run "+failing.String()+": docker unreachable") {
			t.Fatalf("CheckLapses = %v, want the containment failure", err)
		}
		if err := m.BeforeReady(t.Context(), &runnerwire.Peer{}, false); !errors.Is(err, down) {
			t.Fatalf("BeforeReady = %v, want the containment failure", err)
		}
	}
}

// A run whose containment fails with two errors in turn, as an engine coming back up can answer, logs each
// error once and then each one's summary at most once a minute, not a line on every pass (#3704). A run
// that leaves the loop without a success forgets its failures.
func TestContainmentLogBoundsInterleavedErrors(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, _, _, org := managerFixture(t)
	failing := createLocal(t, m, org)
	var seconds atomic.Int64
	base := time.Now()
	refused, timeout := errors.New("connect: connection refused"), errors.New("context deadline exceeded")
	stop := &phaseStop{seconds: &seconds, calls: map[int64]int{}, answer: func(sec int64) error {
		if sec%2 == 0 {
			return refused
		}
		return timeout
	}}
	const total = 120
	ticks := make(chan time.Time)
	m.mu.Lock()
	r := m.state.Runs[failing]
	r.FailedCreation = true
	m.state.Runs[failing] = r
	m.now = func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	m.stopper, m.ticks = stop, ticks
	m.tick = func(context.Context) { seconds.Add(1) }
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- m.Run(ctx) }()
	for range total {
		ticks <- time.Time{}
	}
	cancel()
	<-done

	lines := strings.Count(buf.String(), `"msg":"runner containment will retry"`)
	if lines < 2 || lines > 2*(1+total/60) {
		t.Fatalf("%d ERROR lines over %d passes of two interleaved errors, want each error once plus at most one a minute:\n%s", lines, total, buf.String())
	}

	// A retired run forgets its failures: back in the loop, an error it logged within the last minute logs
	// again at once. Run's first pass is second 122; kept, the timeout's summary would be due at second 181.
	setRetired := func(on bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		r := m.state.Runs[failing]
		r.Retired = on
		m.state.Runs[failing] = r
	}
	setRetired(true)
	_ = m.CheckLapses(t.Context())
	setRetired(false)
	stop.mu.Lock()
	stop.answer = func(int64) error { return timeout }
	stop.mu.Unlock()
	before := strings.Count(buf.String(), `"msg":"runner containment will retry"`)
	ctx, cancel = context.WithCancel(t.Context())
	go func() { done <- m.Run(ctx) }()
	ticks <- time.Time{} // returns once the first pass (second 122) has logged
	cancel()
	<-done
	if after := strings.Count(buf.String(), `"msg":"runner containment will retry"`); after != before+1 {
		t.Fatalf("%d new lines for a retired run's known error back in the loop, want 1 (its failures forgotten):\n%s", after-before, buf.String())
	}
}

// replaySendStub is the peer's EventsAck with one run's events never acknowledged: it records, on the
// fake clock, each run's sends.
type replaySendStub struct {
	failing uuid.UUID
	now     func() time.Time
	mu      sync.Mutex
	calls   map[uuid.UUID]int
	first   map[uuid.UUID]time.Time
}

func (s *replaySendStub) send(_ context.Context, evs []runnerwire.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := *evs[0].RunID
	s.calls[id]++
	if id == s.failing {
		return errors.New("the organisation did not acknowledge")
	}
	if _, ok := s.first[id]; !ok {
		s.first[id] = s.now()
	}
	return nil
}

// One run's replay fails on every try for ten minutes of fake time. A run after it is still replayed
// and acknowledged in the first fake-clock iteration; the failing run waits its own doubling delay
// between tries rather than going on every pass; and its WARN line stays a handful of lines.
func TestReplayFailureBlocksNoLaterRunAndLogsBounded(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, _, _, org := managerFixture(t)
	failing := createLocalAs(t, m, org, uuid.MustParse("00000000-0000-4000-8000-000000000001"))
	later := createLocalAs(t, m, org, uuid.MustParse("ffffffff-0000-4000-8000-000000000002"))
	for _, id := range []uuid.UUID{failing, later} {
		spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
		if err != nil {
			t.Fatal(err)
		}
		appendDecision(t, spool, id)
		appendDecision(t, spool, id)
		if err := spool.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var seconds atomic.Int64
	base := time.Now()
	now := func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	stub := &replaySendStub{failing: failing, now: now, calls: map[uuid.UUID]int{}, first: map[uuid.UUID]time.Time{}}
	const total = 600
	ticks := make(chan time.Time)
	m.mu.Lock()
	m.now, m.ticks, m.replayTicks, m.replaySend = now, make(chan time.Time), ticks, stub.send
	m.replayTick = func() { seconds.Add(1) } // each pass starts one fake second after the last
	m.peer, m.online, m.replayReady = &runnerwire.Peer{}, true, true
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- m.Run(ctx) }()
	for range total - 1 {
		ticks <- time.Time{}
	}
	ticks <- time.Time{} // returns once the last pass has run
	cancel()
	<-done
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if first, ok := stub.first[later]; !ok || first.Sub(base) != time.Second {
		t.Errorf("the later run was first acknowledged at %v (sent=%v), want in the first fake-clock iteration, at 1s", first.Sub(base), ok)
	}
	if n := stub.calls[failing]; n < 2 || n > 15 {
		t.Errorf("the failing run was tried %d times in %d passes, want a backed-off handful", n, total)
	}
	var lines int
	for line := range strings.Lines(buf.String()) {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["level"] == "WARN" && r["msg"] == "runner audit replay will retry" {
			lines++
		}
	}
	if lines == 0 || lines > 15 {
		t.Errorf("%d WARN lines over %d s of the same failure, want a handful", lines, total)
	}
}
