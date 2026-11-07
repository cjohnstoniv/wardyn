// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

import (
	"bytes"
	"io"
	"testing"
)

// The parser sits in a tee on the provider response, so a Write must always report the full length with
// a nil error, however the parse went: a short write would abort the response.
func TestWriteReportsFullLengthOnAMalformedFrame(t *testing.T) {
	data := encodeFrame("metadata", "event", []byte(`oops`))
	for _, chunk := range []int{0, 1, 5} {
		p := NewParser(ShapeBedrockConverseStream, Options{})
		if chunk <= 0 {
			chunk = len(data)
		}
		for i := 0; i < len(data); i += chunk {
			part := data[i:min(i+chunk, len(data))]
			if n, err := p.Write(part); n != len(part) || err != nil {
				t.Fatalf("chunk %d: Write = (%d, %v), want (%d, nil)", chunk, n, err, len(part))
			}
		}
		if got := p.Result(); got.State != StateUncounted || got.Reason != ReasonParseError {
			t.Errorf("chunk %d: got %+v, want uncounted/parse_error", chunk, got)
		}
	}
}

func TestWriteReportsFullLengthWhenAHandlerPanics(t *testing.T) {
	boom := func([]byte) { panic("boom") }
	lines := NewParser(ShapeOpenAIChat, Options{Stream: true})
	lines.(*parser).onLine = boom
	line := []byte("data: {}\n")
	if n, err := lines.Write(line); n != len(line) || err != nil {
		t.Fatalf("onLine panic: Write = (%d, %v), want (%d, nil)", n, err, len(line))
	}
	if got := lines.Result(); got.State != StateUncounted || got.Reason != ReasonMeterError {
		t.Errorf("onLine panic: got %+v, want uncounted/meter_error", got)
	}
	frames := NewParser(ShapeBedrockConverseStream, Options{})
	frames.(*parser).onFrame = func(frame) { panic("boom") }
	f := encodeFrame("metadata", "event", []byte(`{}`))
	if n, err := frames.Write(f); n != len(f) || err != nil {
		t.Fatalf("onFrame panic: Write = (%d, %v), want (%d, nil)", n, err, len(f))
	}
	if got := frames.Result(); got.State != StateUncounted || got.Reason != ReasonMeterError {
		t.Errorf("onFrame panic: got %+v, want uncounted/meter_error", got)
	}
}

// A tee that puts the parser ahead of the client's writer still delivers every original byte.
func TestTeeDeliversEveryByteWhenTheParseFails(t *testing.T) {
	data := append(encodeFrame("metadata", "event", []byte(`oops`)), encodeFrame("metadata", "event", []byte(`{}`))...)
	p := NewParser(ShapeBedrockConverseStream, Options{})
	var dst bytes.Buffer
	w := io.MultiWriter(p, &dst)
	for i := 0; i < len(data); i += 7 {
		part := data[i:min(i+7, len(data))]
		if n, err := w.Write(part); n != len(part) || err != nil {
			t.Fatalf("tee Write = (%d, %v), want (%d, nil)", n, err, len(part))
		}
	}
	if !bytes.Equal(dst.Bytes(), data) {
		t.Fatalf("client received %d of %d bytes", dst.Len(), len(data))
	}
	if got := p.Result(); got.State != StateUncounted || got.Reason != ReasonParseError {
		t.Errorf("got %+v, want uncounted/parse_error", got)
	}
}
