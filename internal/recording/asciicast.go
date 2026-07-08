// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// asciicast v2 format: a header object on line 1, then one JSON event array per
// line. An output event is ["<elapsed seconds>", "o", "<data>"].
// Reference: https://docs.asciinema.org/manual/asciicast/v2/
//
// CastWriter incrementally serializes an asciicast v2 stream. It is used to
// record an interactive attach session: the header is written once at Start,
// then each chunk of PTY OUTPUT (server->client bytes, already secret-masked by
// the caller) is appended as a timed "o" event via Write. The serialized bytes
// accumulate in the wrapped io.Writer (e.g. a bytes.Buffer) so the whole cast
// can be persisted to the RecordingStore when the session ends.
//
// CastWriter is safe for concurrent use by a single producer goroutine; callers
// that write from one goroutine (the attach Read pump) and read the buffer from
// another after Close need no extra locking beyond the internal mutex here.
type CastWriter struct {
	mu      sync.Mutex
	dst     io.Writer
	start   time.Time
	now     func() time.Time
	started bool
	// hadOutput reports whether at least one output event was written, so the
	// caller can skip persisting an empty (header-only) cast if it wishes.
	hadOutput bool
}

// CastHeader is the asciicast v2 header (line 1 of the stream).
type CastHeader struct {
	Version   int   `json:"version"`
	Width     int   `json:"width"`
	Height    int   `json:"height"`
	Timestamp int64 `json:"timestamp,omitempty"`
}

// NewCastWriter returns a CastWriter that serializes events into dst. width and
// height are the initial terminal size recorded in the header (0 values fall
// back to a sane 80x24 so the replay player has a valid geometry). startedAt is
// the wall-clock session start; event timestamps are elapsed seconds from it.
func NewCastWriter(dst io.Writer, width, height int, startedAt time.Time) *CastWriter {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	cw := &CastWriter{dst: dst, start: startedAt, now: time.Now}
	_ = cw.writeHeader(width, height)
	return cw
}

func (w *CastWriter) writeHeader(width, height int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return nil
	}
	w.started = true
	h := CastHeader{Version: 2, Width: width, Height: height, Timestamp: w.start.Unix()}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.dst.Write(b)
	return err
}

// Write appends p as a timed asciicast OUTPUT event ["t","o",string(p)]. The
// elapsed time is computed from the writer's start time. p is the (already
// masked) terminal output. It satisfies io.Writer so it can sit directly behind
// a secretmask.MaskingWriter. A zero-length write is a no-op.
func (w *CastWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	elapsed := w.now().Sub(w.start).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	// json.Marshal of a []any produces ["<float>","o","<json-escaped string>"];
	// asciicast time is a number, so marshal the slice with the float as a
	// number (not a string) per the v2 spec.
	event := []any{elapsed, "o", string(p)}
	b, err := json.Marshal(event)
	if err != nil {
		return 0, err
	}
	b = append(b, '\n')
	if _, err := w.dst.Write(b); err != nil {
		return 0, err
	}
	w.hadOutput = true
	return len(p), nil
}

// HadOutput reports whether any output event was recorded (beyond the header).
func (w *CastWriter) HadOutput() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hadOutput
}
