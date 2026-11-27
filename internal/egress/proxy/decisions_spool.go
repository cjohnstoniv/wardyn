// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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

	// decisionDrainBackoffMax caps the drain's wait after a failed replay.
	decisionDrainBackoffMax = 60 * time.Second
)

// decisionDrainBackoffMin is the drain's first wait after a failed replay; it doubles from here.
var decisionDrainBackoffMin = time.Second

// decisionSpoolWrite writes buf to the spool file; a test replaces it to fail part way.
var decisionSpoolWrite = func(f *os.File, buf []byte) (int, error) { return f.Write(buf) }

// decisionSpoolDir is where a server-run proxy keeps its spool; "" turns the spool off.
var decisionDrainEvery = 5 * time.Second // how often the drain looks for records while any remain

var decisionSpoolDir = func() string { return filepath.Join(os.TempDir(), "wardyn-decision-spool") }

type decisionSpool struct {
	mu      sync.Mutex
	path    string
	records int
	bytes   int64
}

// tmpPrefix names this spool's temporary files, so opening one run's spool never removes another's.
func (sp *decisionSpool) tmpPrefix() string {
	return strings.TrimSuffix(filepath.Base(sp.path), ".jsonl")
}

// syncDir makes a create or rename in dir durable. It is best effort: a filesystem that cannot sync a directory
// leaves the spool as safe as it was.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// openDecisionSpool opens, creating it if need be, the spool of run runID in dir; one file to a run, so a temporary
// directory two runs share never mixes their records. A line that does not decode is dropped
// from the file and counted in the second result.
func openDecisionSpool(dir string, runID uuid.UUID) (*decisionSpool, uint64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, err
	}
	sp := &decisionSpool{path: filepath.Join(dir, "decisions-"+runID.String()+".jsonl")}
	f, err := os.OpenFile(sp.path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, err
	}
	_ = f.Close()
	syncDir(dir)
	// A crash between a rewrite's create and its rename leaves a temporary file of up to the spool's size.
	if stale, _ := filepath.Glob(filepath.Join(dir, sp.tmpPrefix()+"-*.tmp")); len(stale) > 0 {
		for _, name := range stale {
			_ = os.Remove(name)
		}
	}
	lines, bad, err := sp.load()
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
	before, werr := f.Seek(0, io.SeekEnd)
	seeked := werr == nil
	if werr == nil {
		_, werr = decisionSpoolWrite(f, buf.Bytes())
	}
	if werr == nil {
		werr = f.Sync()
	}
	if werr != nil && seeked {
		// A part-written batch would leave a line with no newline for the next append to join onto. Without a known
		// size before the write there is nothing safe to cut back to.
		_ = f.Truncate(before)
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
	size := batchWrapper
	err = eachLine(f, func(line []byte, tooLong bool) bool {
		if len(lines) >= n {
			return false
		}
		var rec DecisionRecord
		if line = bytes.TrimSpace(line); tooLong || len(line) == 0 || json.Unmarshal(line, &rec) != nil {
			consumed, bad = consumed+1, bad+1
			return true
		}
		if extra := len(line) + 1; len(lines) > 0 && size+extra > maxBytes {
			return false
		}
		size += len(line) + 1
		lines = append(lines, append([]byte(nil), line...))
		consumed++
		return true
	})
	if err != nil {
		return nil, 0, 0, err
	}
	return lines, consumed, bad, nil
}

// load reads the whole file for open: the decodable lines up to the spool's caps, and how many lines were left
// out, which is every line that does not decode, is longer than a batch may be, or comes after the caps are met.
func (sp *decisionSpool) load() (lines [][]byte, dropped int, err error) {
	f, err := os.Open(sp.path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var size int64
	err = eachLine(f, func(line []byte, tooLong bool) bool {
		var rec DecisionRecord
		if line = bytes.TrimSpace(line); tooLong || len(line) == 0 || json.Unmarshal(line, &rec) != nil ||
			len(lines) >= decisionSpoolMaxRecords || size+int64(len(line))+1 > decisionSpoolMaxBytes {
			dropped++
			return true
		}
		size += int64(len(line)) + 1
		lines = append(lines, append([]byte(nil), line...))
		return true
	})
	return lines, dropped, err
}

// eachLine calls fn with each line of r, without its newline. A line longer than maxBatchBody is not held: fn gets
// it empty with tooLong true, so a reader moves past it. fn returns false to stop.
func eachLine(r io.Reader, fn func(line []byte, tooLong bool) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			line = append(line, chunk...)
			if len(line) > maxBatchBody {
				line, tooLong = nil, true
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(line) > 0 || tooLong {
			if !fn(bytes.TrimRight(line, "\r\n"), tooLong) {
				return nil
			}
		}
		line, tooLong = nil, false
		if err != nil { // io.EOF
			return nil
		}
	}
}

// ack removes the first consumed lines of the file, by writing the rest to a temporary file in the same
// directory and renaming it over the spool. A later line too long to keep is left out, and counted in lost.
func (sp *decisionSpool) ack(consumed int) (lost int, err error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	f, err := os.Open(sp.path)
	if err != nil {
		return 0, err
	}
	var rest [][]byte
	i := 0
	err = eachLine(f, func(line []byte, tooLong bool) bool {
		if i++; i > consumed {
			if tooLong {
				lost++
			} else {
				rest = append(rest, append([]byte(nil), line...))
			}
		}
		return true
	})
	_ = f.Close()
	if err != nil {
		return 0, err
	}
	return lost, sp.rewrite(rest)
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
	tmp, err := os.CreateTemp(filepath.Dir(sp.path), sp.tmpPrefix()+"-*.tmp")
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
	if err == nil {
		syncDir(filepath.Dir(sp.path))
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
func (s *decisionSink) openSpool(runID uuid.UUID) {
	dir := decisionSpoolDir()
	if dir == "" {
		return
	}
	sp, bad, err := openDecisionSpool(dir, runID)
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
	waiting:
		for {
			select {
			case <-s.drainStop:
				t.Stop()
				return
			case <-s.drainKick:
				if backoff > 0 {
					continue // a failure's wait is neither cut short nor stretched by an answer from the worker
				}
				t.Stop()
				break waiting
			case <-t.C:
				break waiting
			}
		}
		switch s.drainSpool(s.drainStopped) {
		case drainFailed:
			backoff = min(max(backoff*2, decisionDrainBackoffMin), decisionDrainBackoffMax)
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
			case code == 200 || code == 202:
			case code/100 == 2:
				return drainFailed // a success the door never sends: it did not say it took the batch, so keep them
			default:
				drop = uint64(len(lines))
				if code == 400 {
					slog.Warn("egress proxy: the control plane refused a spooled decision batch as malformed",
						slog.Int("status", code), slog.Uint64("records", drop))
				}
			}
		}
		lost, err := s.spool.ack(consumed)
		if err != nil {
			slog.Warn("egress proxy: the decision spool cannot be trimmed", slog.String("err", err.Error()))
			return drainFailed
		}
		s.dropped.Add(drop + uint64(bad) + uint64(lost))
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
