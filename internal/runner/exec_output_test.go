// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"io"
	"testing"
)

type drainSpy struct {
	io.Writer
	begun, ended int
	err          error
}

func (d *drainSpy) BeginDrain()        { d.begun++ }
func (d *drainSpy) EndDrain(err error) { d.ended++; d.err = err }

type closerSpy struct {
	io.Writer
	closed int
}

func (c *closerSpy) Close() error { c.closed++; return nil }

// BeginOutputDrain reads the writer's optional contract one way for every
// driver: an OutputDrainer hears both ends, a bare io.Closer is closed when the
// copy ends, and anything else costs nothing.
func TestBeginOutputDrain(t *testing.T) {
	d := &drainSpy{Writer: io.Discard}
	end := BeginOutputDrain(d)
	if d.begun != 1 || d.ended != 0 {
		t.Fatalf("after begin: begun %d ended %d, want 1 and 0", d.begun, d.ended)
	}
	boom := errors.New("boom")
	end(boom)
	if d.ended != 1 || !errors.Is(d.err, boom) {
		t.Fatalf("after end: ended %d err %v, want 1 and the copy's error", d.ended, d.err)
	}

	c := &closerSpy{Writer: io.Discard}
	BeginOutputDrain(c)(nil)
	if c.closed != 1 {
		t.Fatalf("a bare io.Closer was closed %d times, want 1", c.closed)
	}

	BeginOutputDrain(nil)(nil)
	BeginOutputDrain(io.Discard)(errors.New("ignored"))
}
