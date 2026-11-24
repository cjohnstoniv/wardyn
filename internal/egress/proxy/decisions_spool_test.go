// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

func init() {
	// No test writes the real spool directory. A test that wants a spool opens its own.
	decisionSpoolDir = func() string { return "" }
}

// spooledSink is a paused sink with a spool in a temporary directory; start runs the worker.
func spooledSink(t *testing.T, c *stubCP, n int) (*decisionSink, func()) {
	t.Helper()
	s, start := pausedSink(c, n, "tok")
	sp, bad, err := openDecisionSpool(t.TempDir())
	if err != nil || bad != 0 {
		t.Fatalf("open spool: %v (bad %d)", err, bad)
	}
	s.spool = sp
	return s, start
}

// failBatches answers every batch post that is not a replay with status, and every other post with 202.
func failBatches(status int) func(int, cpPost) (int, bool) {
	return func(_ int, p cpPost) (int, bool) {
		if p.batch && !p.replay {
			return status, true
		}
		return http.StatusAccepted, true
	}
}

func spoolFile(t *testing.T, s *decisionSink) string {
	t.Helper()
	b, err := os.ReadFile(s.spool.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A batch whose every attempt failed is kept in the spool with its ids and times, and is not counted dropped.
func TestDecisionSpool_FinalAttemptSpoolsNotDrops(t *testing.T) {
	fastBackoff(t)
	c := newStubCP(t, failBatches(http.StatusServiceUnavailable))
	s, start := spooledSink(t, c, 16)
	for i := range 3 {
		s.emit(hostLog(i))
	}
	start()
	closeSinkKeepingSpool(t, s)
	if d := s.droppedCount(); d != 0 {
		t.Fatalf("dropped %d, want 0: the batch is in the spool", d)
	}
	// The first record goes alone, before the control plane has said it takes batches; the other two are the batch.
	if n := s.spool.pending(); n != 2 {
		t.Fatalf("the spool holds %d records, want 2", n)
	}
	var tries []cpPost
	for _, p := range c.got() {
		if p.batch {
			tries = append(tries, p)
		}
	}
	if len(tries) != 4 {
		t.Fatalf("batch attempts %d, want 4", len(tries))
	}
	for i, p := range tries {
		if p.final {
			t.Errorf("attempt %d carries the final mark though the spool had room", i+1)
		}
	}
	for _, r := range tries[3].records {
		if !strings.Contains(spoolFile(t, s), r.ID.String()) {
			t.Errorf("record %s is not in the spool", r.ID)
		}
	}
}

// closeSinkKeepingSpool closes the sink's worker the way closeSink does, but without stopSpool's clearing:
// the tests below read what the spool holds after the last attempt.
func closeSinkKeepingSpool(t *testing.T, s *decisionSink) {
	t.Helper()
	s.mu.Lock()
	s.closed = true
	close(s.ch)
	s.mu.Unlock()
	s.wg.Wait()
}

// The drain posts the spooled records with their own ids and times, marked as a replay and not final, and
// acknowledges them only on an answer that landed.
func TestDecisionSpool_DrainKeepsIDsAndTimes(t *testing.T) {
	fastBackoff(t)
	c := newStubCP(t, failBatches(http.StatusServiceUnavailable))
	s, start := spooledSink(t, c, 16)
	for i := range 3 {
		s.emit(hostLog(i))
	}
	start()
	closeSinkKeepingSpool(t, s)
	var first cpPost
	for _, p := range c.got() {
		if p.batch {
			first = p
		}
	}
	if res := s.drainSpool(func() bool { return false }); res != drainDone {
		t.Fatalf("drain = %v", res)
	}
	if n := s.spool.pending(); n != 0 {
		t.Fatalf("the spool still holds %d records after a landed replay", n)
	}
	var replays []cpPost
	for _, p := range c.got() {
		if p.replay {
			replays = append(replays, p)
		}
	}
	if len(replays) != 1 || replays[0].final || len(replays[0].records) != 2 {
		t.Fatalf("%d replays (first: final %v, %d records), want one non-final batch of 2", len(replays), replays[0].final, len(replays[0].records))
	}
	for i, r := range replays[0].records {
		if r.ID != first.records[i].ID || !r.Time.Equal(first.records[i].Time) {
			t.Errorf("record %d: id %s time %v, want the first attempt's %s %v", i, r.ID, r.Time, first.records[i].ID, first.records[i].Time)
		}
	}
}

// No advertisement, no replay: a drain with the control plane's mode unknown posts nothing, and an answer
// without the advertisement stops it and keeps the records.
func TestDecisionSpool_DrainNeedsAdvertisement(t *testing.T) {
	fastBackoff(t)
	c := newStubCP(t, failBatches(http.StatusServiceUnavailable))
	s, start := spooledSink(t, c, 16)
	for i := range 3 {
		s.emit(hostLog(i))
	}
	start()
	closeSinkKeepingSpool(t, s)
	posts := len(c.got())
	s.batchMax.Store(0)
	if res := s.drainSpool(func() bool { return false }); res != drainDone || len(c.got()) != posts || s.spool.pending() != 2 {
		t.Fatalf("a drain with no advertisement posted or lost records: %v, %d posts, %d held", res, len(c.got())-posts, s.spool.pending())
	}
	c.answer = func(int, cpPost) (int, bool) { return http.StatusAccepted, false }
	s.batchMax.Store(MaxDecisionBatch)
	if res := s.drainSpool(func() bool { return false }); res != drainDone || s.spool.pending() != 2 || s.batchMax.Load() != 0 {
		t.Fatalf("a header-less answer: %v, %d held, mode %d; want the records kept and the mode single", res, s.spool.pending(), s.batchMax.Load())
	}
}

// The status table of the drain (answers with the advertisement).
func TestDecisionSpool_DrainStatusTable(t *testing.T) {
	logs := captureSlog(t)
	for _, tc := range []struct {
		status  int
		failed  bool // the records stay and the drain backs off
		dropped uint64
		warn    bool
	}{
		{http.StatusAccepted, false, 0, false},
		{http.StatusOK, false, 0, false},
		{http.StatusTooManyRequests, true, 0, false},
		{http.StatusServiceUnavailable, true, 0, false},
		{http.StatusBadGateway, true, 0, false},
		{http.StatusBadRequest, false, 2, true},
		{http.StatusUnauthorized, false, 2, false},
		{http.StatusForbidden, false, 2, false},
		{http.StatusNotFound, false, 2, false},
		{http.StatusRequestEntityTooLarge, false, 2, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			logs.Reset()
			c := newStubCP(t, func(int, cpPost) (int, bool) { return tc.status, true })
			s, _ := spooledSink(t, c, 4)
			if err := s.spool.append([]batchRecord{mustRecord(t, 0), mustRecord(t, 1)}); err != nil {
				t.Fatal(err)
			}
			s.batchMax.Store(MaxDecisionBatch)
			res := s.drainSpool(func() bool { return false })
			if failed := res == drainFailed; failed != tc.failed {
				t.Fatalf("drain failed = %v, want %v", failed, tc.failed)
			}
			wantHeld := 0
			if tc.failed {
				wantHeld = 2
			}
			if s.spool.pending() != wantHeld || s.droppedCount() != tc.dropped {
				t.Fatalf("held %d dropped %d, want %d and %d", s.spool.pending(), s.droppedCount(), wantHeld, tc.dropped)
			}
			if got := strings.Count(logs.String(), "refused a spooled decision batch"); (got == 1) != tc.warn || got > 1 {
				t.Fatalf("warn lines %d, want warn=%v: %s", got, tc.warn, logs.String())
			}
		})
	}
	t.Run("no response", func(t *testing.T) {
		c := newStubCP(t, nil)
		s, _ := spooledSink(t, c, 4)
		if err := s.spool.append([]batchRecord{mustRecord(t, 0)}); err != nil {
			t.Fatal(err)
		}
		s.batchMax.Store(MaxDecisionBatch)
		c.srv.Close()
		if res := s.drainSpool(func() bool { return false }); res != drainFailed || s.spool.pending() != 1 || s.droppedCount() != 0 {
			t.Fatalf("no response: %v, %d held, %d dropped; want failed, 1, 0", res, s.spool.pending(), s.droppedCount())
		}
	})
}

func mustRecord(t *testing.T, i int) batchRecord {
	t.Helper()
	r, err := encodeBatchRecord(queuedDecision{log: hostLog(i), id: uuid.New(), at: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A batch the spool cannot take is dropped and counted once: when the spool has no room the last attempt carries
// the final mark, and when an append the spool had room for is refused the drop count carries the batch. The
// summary reports the two records once either way.
func TestDecisionSpool_FullSpoolReportsDropOnce(t *testing.T) {
	fastBackoff(t)
	for name, prep := range map[string]func(*testing.T, *decisionSink){
		"no room": func(_ *testing.T, s *decisionSink) { s.spool.bytes = decisionSpoolMaxBytes - 1 },
		"append refused": func(t *testing.T, s *decisionSink) {
			s.spool.path = filepath.Join(t.TempDir(), "gone", decisionSpoolFile)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var summaries atomic.Int32
			c := newStubCP(t, func(_ int, p cpPost) (int, bool) {
				if !p.batch {
					if strings.Contains(p.single.RuleSource, "dropped-decisions-2") {
						summaries.Add(1)
					}
					return http.StatusAccepted, true
				}
				return http.StatusServiceUnavailable, true
			})
			s, start := spooledSink(t, c, 16)
			prep(t, s)
			for i := range 3 {
				s.emit(hostLog(i))
			}
			start()
			closeSink(t, s)
			if d := s.droppedCount(); d != 2 {
				t.Fatalf("dropped %d, want 2", d)
			}
			if summaries.Load() != 1 {
				t.Fatalf("summaries naming 2 drops: %d, want 1", summaries.Load())
			}
		})
	}
}

// A directory that cannot be opened turns the spool off: one log line naming the error and nothing else, and
// the batch path is part 1's.
func TestDecisionSpool_UnopenableDirFallsBack(t *testing.T) {
	logs := captureSlog(t)
	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	decisionSpoolDir = func() string { return filepath.Join(file, "sub") }
	t.Cleanup(func() { decisionSpoolDir = func() string { return "" } })
	c := newStubCP(t, nil)
	s := newDecisionSink(c.srv.URL, newTokenSource("tok"), 4, c.srv.Client(), nil)
	if s.spool != nil {
		t.Fatal("the spool opened in a directory under a regular file")
	}
	if got := strings.Count(logs.String(), "the decision spool is off"); got != 1 {
		t.Fatalf("log lines %d, want one: %s", got, logs.String())
	}
	closeSink(t, s)
}

// What the spool still holds at close is replayed within the budget, and what could not be is counted dropped
// in the final summary and the file emptied.
func TestDecisionSpool_CloseCountsRemainder(t *testing.T) {
	fastBackoff(t)
	var summaries atomic.Int32
	c := newStubCP(t, func(_ int, p cpPost) (int, bool) {
		if !p.batch {
			if strings.Contains(p.single.RuleSource, "dropped-decisions-2") {
				summaries.Add(1)
			}
			return http.StatusAccepted, true
		}
		return http.StatusServiceUnavailable, true
	})
	s, start := spooledSink(t, c, 16)
	for i := range 3 {
		s.emit(hostLog(i))
	}
	start()
	closeSink(t, s)
	if n := s.spool.pending(); n != 0 {
		t.Fatalf("the spool holds %d records after close", n)
	}
	if d := s.droppedCount(); d != 2 {
		t.Fatalf("dropped %d, want the 2 left in the spool", d)
	}
	if summaries.Load() != 1 {
		t.Fatalf("summaries naming 2 drops: %d, want 1", summaries.Load())
	}
}

// A line that does not decode is counted once and skipped, at open and at read, and never logged.
func TestDecisionSpool_BadLinesCountedOnce(t *testing.T) {
	dir := t.TempDir()
	sp, _, err := openDecisionSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.append([]batchRecord{mustRecord(t, 0)}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(sp.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()
	if err := sp.append([]batchRecord{mustRecord(t, 1)}); err != nil {
		t.Fatal(err)
	}
	lines, consumed, bad, err := sp.head(10, maxBatchBody)
	if err != nil || len(lines) != 2 || consumed != 3 || bad != 1 {
		t.Fatalf("read: %d lines, consumed %d, bad %d, %v; want 2, 3, 1", len(lines), consumed, bad, err)
	}
	reopened, badAtOpen, err := openDecisionSpool(dir)
	if err != nil || badAtOpen != 1 || reopened.pending() != 2 {
		t.Fatalf("reopen: bad %d, %d held, %v; want 1 and 2", badAtOpen, reopened.pending(), err)
	}
	if _, _, again, _ := reopened.head(10, maxBatchBody); again != 0 {
		t.Fatalf("the bad line is still in the file: %d", again)
	}
}

// The spool's log lines name an error or a count and never a host, a path of a request, a token or a body.
func TestDecisionSpool_LogsNoData(t *testing.T) {
	fastBackoff(t)
	const token, host, path = "tok-SECRET-spool", "secret-spool-host.example", "/secret/spool/path"
	logs := captureSlog(t)
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusBadRequest} {
		c := newStubCP(t, func(_ int, p cpPost) (int, bool) {
			if p.batch {
				return status, true
			}
			return http.StatusAccepted, true
		})
		s, start := pausedSink(c, 16, token)
		sp, _, err := openDecisionSpool(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.spool = sp
		for range 2 {
			s.emit(decisionLog(egress.Request{RunID: uuid.New(), Host: host, Path: path, Method: "GET"}, egress.Deny, "policy:denied"))
		}
		start()
		closeSink(t, s) // replays, fails, and counts what is left at close
	}
	out := logs.String()
	if !strings.Contains(out, "spool") {
		t.Fatalf("the capture saw no spool line; the test proves nothing: %q", out)
	}
	for _, secret := range []string{token, host, path} {
		if strings.Contains(out, secret) {
			t.Fatalf("a log line names %q: %s", secret, out)
		}
	}
}

// With an open spool that has room the last attempt has no final mark; with the spool off, full or a relayed
// batch it has one.
func TestDecisionSpool_NoFinalMarkWhileSpoolHasRoom(t *testing.T) {
	fastBackoff(t)
	for name, setup := range map[string]struct {
		prep      func(*decisionSink)
		wantFinal bool
	}{
		"room":  {func(*decisionSink) {}, false},
		"off":   {func(s *decisionSink) { s.spool = nil }, true},
		"full":  {func(s *decisionSink) { s.spool.records = decisionSpoolMaxRecords }, true},
		"bytes": {func(s *decisionSink) { s.spool.bytes = decisionSpoolMaxBytes }, true},
	} {
		t.Run(name, func(t *testing.T) {
			c := newStubCP(t, failBatches(http.StatusServiceUnavailable))
			s, start := spooledSink(t, c, 16)
			setup.prep(s)
			s.emit(hostLog(0))
			s.emit(hostLog(1))
			start()
			closeSinkKeepingSpool(t, s)
			var last cpPost
			for _, p := range c.got() {
				if p.batch {
					last = p
				}
			}
			if last.final != setup.wantFinal {
				t.Fatalf("last attempt final mark = %v, want %v", last.final, setup.wantFinal)
			}
		})
	}
}

// The spool is trimmed only by an answer that landed the batch: 200 and 202 trim it, and no other status, nor a
// transport failure, removes a record that was not also counted dropped.
func TestDecisionSpool_AckOnlyOn200Or202(t *testing.T) {
	for _, tc := range []struct {
		status  int
		trimmed bool
	}{
		{http.StatusOK, true},
		{http.StatusAccepted, true},
		{http.StatusNoContent, true}, // any 2xx: the door answers 202, and 200 on an older one
		{http.StatusTooManyRequests, false},
		{http.StatusServiceUnavailable, false},
	} {
		c := newStubCP(t, func(int, cpPost) (int, bool) { return tc.status, true })
		s, _ := spooledSink(t, c, 4)
		if err := s.spool.append([]batchRecord{mustRecord(t, 0), mustRecord(t, 1)}); err != nil {
			t.Fatal(err)
		}
		s.batchMax.Store(MaxDecisionBatch)
		s.drainSpool(func() bool { return false })
		if got := s.spool.pending() == 0; got != tc.trimmed || s.droppedCount() != 0 {
			t.Errorf("status %d: trimmed %v dropped %d, want trimmed %v and no drops", tc.status, got, s.droppedCount(), tc.trimmed)
		}
	}
}
