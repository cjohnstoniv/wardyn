// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import "bytes"

// Detector is the detect-only form of MaskingWriter: it reports whether any of a Masker's values occurs in
// a byte stream written to it in chunks, and forwards nothing. It keeps MaskingWriter's hold-back rule:
// after each Write it holds exactly the longest end of the stream that is the start of a value
// (cutPrefixLen), so a value split across two Writes is found once its last byte arrives, and nothing else
// of the stream is kept. Found is final after the last Write: a held end is only ever the start of a value,
// never a whole one.
//
// A Detector reads one stream. Reset starts another, so a value split across two streams is never joined.
// A Masker with no values finds nothing. It is not safe for concurrent use.
type Detector struct {
	m     Masker
	tail  []byte
	found bool
}

// NewDetector returns a Detector over m's values.
func NewDetector(m Masker) *Detector { return &Detector{m: m} }

// Write looks for a value in p and in a value that the held end and p's start make together. It never
// fails, so it can sit beside a digest in an io.MultiWriter.
func (d *Detector) Write(p []byte) (int, error) {
	if d.found || len(p) == 0 || len(d.m.secrets) == 0 {
		return len(p), nil
	}
	if d.m.containsAny(p) {
		d.found = true
		return len(p), nil
	}
	// A value that crosses into p begins in the held end and is shorter than maxLen, so the held end and
	// p's first maxLen-1 bytes hold it whole.
	head := p[:min(len(p), d.m.maxLen-1)]
	join := append(d.tail, head...) //nolint:gocritic // tail is ours, re-used on purpose
	if d.m.containsAny(join) {
		d.found = true
		return len(p), nil
	}
	// The next held end is the longest end of the stream so far that starts a value. It is shorter than
	// maxLen, so the last maxLen-1 bytes of the held end and p decide it.
	var window []byte
	if len(p) >= d.m.maxLen-1 {
		window = p[len(p)-(d.m.maxLen-1):]
	} else {
		window = join[max(0, len(join)-(d.m.maxLen-1)):]
	}
	d.tail = bytes.Clone(window[len(window)-d.m.cutPrefixLen(window):])
	return len(p), nil
}

// Found reports whether a value occurred in the stream written so far.
func (d *Detector) Found() bool { return d.found }

// Reset forgets the stream read so far, so the next Write starts a new one.
func (d *Detector) Reset() { d.tail, d.found = nil, false }

// containsAny reports whether p holds any of m's values whole.
func (m Masker) containsAny(p []byte) bool {
	var present [256]bool
	for _, b := range p {
		present[b] = true
	}
	for _, s := range m.secrets {
		if present[s[0]] && bytes.Contains(p, s) {
			return true
		}
	}
	return false
}
