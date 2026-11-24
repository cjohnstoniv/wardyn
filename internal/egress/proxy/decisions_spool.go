// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The decision spool keeps the batches a server-run proxy could not deliver. It is a file of the masked JSON
// lines of DecisionRecord, the ids and queue times a retried batch carries, so a record replayed later lands
// once and under its own time. It is the proxy's own file: the runner's spool (runnerspool) has no place for
// a record id or time and its digest is fixed by what older runners replay.
const (
	// DecisionReplayHeader marks a batch the proxy replays from its spool. A control plane that took the
	// batches' advertisement keeps each record's own time inside a wider window for it.
	DecisionReplayHeader = "Wardyn-Decision-Replay"

	// decisionSpoolMaxRecords and decisionSpoolMaxBytes bound the file; an append past either is refused.
	decisionSpoolMaxRecords = 50_000
	decisionSpoolMaxBytes   = 32 << 20

	decisionSpoolFile = "decisions.jsonl"
	// decisionDrainBackoffMax caps the drain's wait after a failed replay.
	decisionDrainBackoffMax = 60 * time.Second
)

// decisionSpoolDir is where a server-run proxy keeps its spool; "" turns the spool off.
var decisionDrainEvery = 5 * time.Second // how often the drain looks for records while any remain

var decisionSpoolDir = func() string { return filepath.Join(os.TempDir(), "wardyn-decision-spool") }

type decisionSpool struct {
	mu      sync.Mutex
	path    string
	records int
	bytes   int64
}

// openDecisionSpool opens, creating it if need be, the spool in dir. A line that does not decode is dropped
// from the file and counted in the second result.
func openDecisionSpool(dir string) (*decisionSpool, uint64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, err
	}
	sp := &decisionSpool{path: filepath.Join(dir, decisionSpoolFile)}
	f, err := os.OpenFile(sp.path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, err
	}
	_ = f.Close()
	lines, _, bad, err := sp.read(decisionSpoolMaxRecords, decisionSpoolMaxBytes)
	if err != nil {
		return nil, 0, err
	}
	if bad > 0 {
		if err := sp.rewrite(lines); err != nil {
			return nil, 0, err
		}
	}
	sp.records = len(lines)
	for _, l := range lines {
		sp.bytes += int64(len(l)) + 1
	}
	return sp, uint64(bad), nil
}

// room reports whether n records of size bytes in all fit.
func (sp *decisionSpool) room(n int, size int64) bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.records+n <= decisionSpoolMaxRecords && sp.bytes+size <= decisionSpoolMaxBytes
}

// pending is how many records the spool holds.
func (sp *decisionSpool) pending() int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.records
}

var errDecisionSpoolFull = errors.New("decision spool is full")

// append writes the records, each a masked JSON line, and syncs once. It writes nothing when they do not all fit.
func (sp *decisionSpool) append(recs []batchRecord) error {
	var size int64
	for _, r := range recs {
		size += int64(len(r.body)) + 1
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.records+len(recs) > decisionSpoolMaxRecords || sp.bytes+size > decisionSpoolMaxBytes {
		return errDecisionSpoolFull
	}
	f, err := os.OpenFile(sp.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, r := range recs {
		buf.Write(r.body)
		buf.WriteByte('\n')
	}
	_, werr := f.Write(buf.Bytes())
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	sp.records += len(recs)
	sp.bytes += size
	return nil
}

// head returns the lines of the first records that fit in at most n records and maxBytes of body, how many
// lines of the file they and any undecodable lines before them take (consumed), and how many of those were
// undecodable. It reads and removes nothing.
func (sp *decisionSpool) head(n int, maxBytes int) (lines [][]byte, consumed, bad int, err error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.read(n, maxBytes)
}

func (sp *decisionSpool) read(n, maxBytes int) (lines [][]byte, consumed, bad int, err error) {
	f, err := os.Open(sp.path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxBatchBody)
	size := batchWrapper
	for len(lines) < n && sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		var rec DecisionRecord
		if len(line) == 0 || json.Unmarshal(line, &rec) != nil {
			consumed, bad = consumed+1, bad+1
			continue
		}
		if extra := len(line) + 1; len(lines) > 0 && size+extra > maxBytes {
			break
		}
		size += len(line) + 1
		lines = append(lines, append([]byte(nil), line...))
		consumed++
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return nil, 0, 0, err
	}
	return lines, consumed, bad, nil
}

// ack removes the first consumed lines of the file, by writing the rest to a temporary file in the same
// directory and renaming it over the spool.
func (sp *decisionSpool) ack(consumed int) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	f, err := os.Open(sp.path)
	if err != nil {
		return err
	}
	var rest [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxBatchBody)
	for i := 0; sc.Scan(); i++ {
		if i >= consumed {
			rest = append(rest, append([]byte(nil), sc.Bytes()...))
		}
	}
	err = sc.Err()
	_ = f.Close()
	if err != nil {
		return err
	}
	return sp.rewrite(rest)
}

// clear empties the spool and returns how many records it held.
func (sp *decisionSpool) clear() (int, error) {
	sp.mu.Lock()
	n := sp.records
	sp.mu.Unlock()
	if err := sp.rewriteLocked(nil); err != nil {
		return 0, err
	}
	return n, nil
}

func (sp *decisionSpool) rewriteLocked(lines [][]byte) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.rewrite(lines)
}

// rewrite replaces the file with lines. The caller holds sp.mu, or owns sp before it is shared.
func (sp *decisionSpool) rewrite(lines [][]byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(sp.path), "decisions-*.tmp")
	if err != nil {
		return err
	}
	var size int64
	w := bufio.NewWriter(tmp)
	for _, l := range lines {
		_, _ = w.Write(l)
		_ = w.WriteByte('\n')
		size += int64(len(l)) + 1
	}
	err = w.Flush()
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), sp.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	sp.records, sp.bytes = len(lines), size
	return nil
}

// openSpool opens the proxy's spool, unless it is off; a spool that cannot be opened leaves the proxy as it
// was before the spool, with one line naming the error.
func (s *decisionSink) openSpool() {
	dir := decisionSpoolDir()
	if dir == "" {
		return
	}
	sp, bad, err := openDecisionSpool(dir)
	if err != nil {
		slog.Warn("egress proxy: the decision spool is off", slog.String("err", err.Error()))
		return
	}
	s.spool = sp
	s.dropped.Add(bad)
}

// spoolHasRoom reports whether recs fit the spool, in which case the last attempt at them goes without the
// final mark.
func (s *decisionSink) spoolHasRoom(recs []batchRecord) bool {
	if s.spool == nil {
		return false
	}
	var size int64
	for _, r := range recs {
		size += int64(len(r.body)) + 1
	}
	return s.spool.room(len(recs), size)
}

// startSpoolDrain starts the drain goroutine, when there is a spool.
func (s *decisionSink) startSpoolDrain() {
	if s.spool == nil {
		return
	}
	s.drainStop = make(chan struct{})
	s.drainKick = make(chan struct{}, 1)
	s.drainWG.Add(1)
	go s.drainLoop()
}

// kickDrain asks the drain to look now: the control plane answered after a failure.
func (s *decisionSink) kickDrain() {
	if s.drainKick == nil {
		return
	}
	select {
	case s.drainKick <- struct{}{}:
	default:
	}
}

func (s *decisionSink) drainStopped() bool {
	select {
	case <-s.drainStop:
		return true
	default:
		return false
	}
}

// drainLoop replays the spool: when kicked, and every decisionDrainEvery while records remain, waiting longer
// (1, 2, 4 ... capped) after a failure.
func (s *decisionSink) drainLoop() {
	defer s.drainWG.Done()
	wait, backoff := decisionDrainEvery, time.Duration(0)
	for {
		t := time.NewTimer(wait)
		select {
		case <-s.drainStop:
			t.Stop()
			return
		case <-s.drainKick:
			t.Stop()
			if backoff > 0 {
				continue // a failure's wait is not cut short by an answer from the worker
			}
		case <-t.C:
		}
		switch s.drainSpool(s.drainStopped) {
		case drainFailed:
			backoff = min(max(backoff*2, time.Second), decisionDrainBackoffMax)
			wait = backoff
		default:
			backoff, wait = 0, decisionDrainEvery
		}
	}
}

type drainResult int

const (
	drainDone   drainResult = iota // nothing left, or nothing to do now
	drainFailed                    // a replay got no usable answer: the records stay
)

// drainSpool replays spooled records, one batch at a time, until none are left, one cannot be delivered, or
// stop says to give up.
func (s *decisionSink) drainSpool(stop func() bool) drainResult {
	for s.spool.pending() > 0 {
		if stop() {
			return drainDone
		}
		if s.batchMax.Load() < 2 {
			return drainDone // no advertisement, no replay
		}
		lines, consumed, bad, err := s.spool.head(MaxDecisionBatch, maxBatchBody)
		if err != nil {
			slog.Warn("egress proxy: the decision spool cannot be read", slog.String("err", err.Error()))
			return drainFailed
		}
		if consumed == 0 {
			return drainDone
		}
		var drop uint64
		if len(lines) > 0 {
			code, advertised, err := s.postReplay(lines)
			switch {
			case err == nil && !advertised:
				return drainDone // the answer read no batch: keep the records, resume once one advertises again
			case err != nil || code == 429 || code/100 == 5:
				return drainFailed
			case code/100 == 2:
			default:
				drop = uint64(len(lines))
				if code == 400 {
					slog.Warn("egress proxy: the control plane refused a spooled decision batch as malformed",
						slog.Int("status", code), slog.Uint64("records", drop))
				}
			}
		}
		if err := s.spool.ack(consumed); err != nil {
			slog.Warn("egress proxy: the decision spool cannot be trimmed", slog.String("err", err.Error()))
			return drainFailed
		}
		s.dropped.Add(drop + uint64(bad))
	}
	return drainDone
}

// postReplay posts lines, spooled records, as one batch with the replay mark and without the final one.
func (s *decisionSink) postReplay(lines [][]byte) (int, bool, error) {
	var body bytes.Buffer
	body.WriteString(`{"records":[`)
	for i, l := range lines {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(l)
	}
	body.WriteString(`]}`)
	return s.postBatchBodyWith(body.Bytes(), false, false, true)
}

// stopSpool stops the drain, then replays what it can until ctx is done, and counts what is left dropped.
func (s *decisionSink) stopSpool(done func() bool) {
	if s.spool == nil {
		return
	}
	if s.drainStop != nil {
		close(s.drainStop)
		s.drainWG.Wait()
	}
	s.drainSpool(done)
	if n, err := s.spool.clear(); err != nil {
		slog.Warn("egress proxy: the decision spool cannot be emptied", slog.String("err", err.Error()))
	} else if n > 0 {
		s.dropped.Add(uint64(n))
		slog.Warn("egress proxy: decisions still in the spool at close were dropped", slog.Int("records", n))
	}
}
