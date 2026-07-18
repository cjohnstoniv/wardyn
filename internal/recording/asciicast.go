// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"encoding/json"
	"io"
	"sync"
	"time"
	"unicode/utf8"
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
	// pending holds the trailing bytes of a multi-byte UTF-8 rune that hadn't
	// fully arrived by the end of the last Write (a rune split across two PTY
	// reads). It is prepended to the next Write so the rune is emitted whole
	// instead of as two invalid fragments.
	// there is no Flush/Close, so bytes still in pending when the
	// session ends (no further Write) are dropped rather than emitted --
	// silent loss of a few trailing bytes is preferable to the mojibake this
	// fixes, and a genuine end-of-stream mid-rune split is rare. Add a Flush
	// if that residual ever matters.
	pending []byte
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

	n := len(p) // bytes accepted from this call, reported below regardless of buffering
	if len(w.pending) > 0 {
		p = append(w.pending, p...)
		w.pending = nil
	}

	// A multi-byte UTF-8 rune whose bytes arrive split across two Writes would
	// otherwise be converted (via string(p)) as two invalid-rune fragments —
	// json.Marshal silently replaces each with U+FFFD, mangling the character
	// in the recording. If p ends mid-rune, hold back the incomplete trailing
	// bytes and prepend them to the next Write instead of emitting them now.
	if r, size := utf8.DecodeLastRune(p); r == utf8.RuneError && size == 1 {
		if cut := incompleteTailLen(p); cut > 0 {
			w.pending = append([]byte(nil), p[len(p)-cut:]...)
			p = p[:len(p)-cut]
		}
	}
	if len(p) == 0 {
		// The whole chunk was a rune lead byte with no continuation bytes yet;
		// it's buffered in w.pending, nothing to emit this call.
		return n, nil
	}

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
	return n, nil
}

// incompleteTailLen reports how many trailing bytes of p are an incomplete
// (truncated) multi-byte UTF-8 rune -- a valid lead byte whose continuation
// bytes haven't arrived yet. It returns 0 if p already ends on a rune
// boundary, which includes plain ASCII and trailing bytes that are simply
// invalid outright rather than truncated. Only called after
// utf8.DecodeLastRune has already flagged p's tail as suspect, so the plain
// backward scan here (bounded to the last 3 bytes -- a rune is at most
// utf8.UTFMax bytes) is cheap.
func incompleteTailLen(p []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(p); i++ {
		b := p[len(p)-i]
		if utf8.RuneStart(b) {
			if b < utf8.RuneSelf || utf8.FullRune(p[len(p)-i:]) {
				return 0 // ASCII, or already a complete (valid or invalid) rune
			}
			return i
		}
	}
	return 0
}

// HadOutput reports whether any output event was recorded (beyond the header).
func (w *CastWriter) HadOutput() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hadOutput
}
