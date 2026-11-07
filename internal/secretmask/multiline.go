// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import "bytes"

// MultilineRenderings returns the forms of a value with a line break inside it
// that output can carry and the stored bytes do not match: the value with
// every line break as a bare LF and as CR LF (what a terminal's line discipline
// prints for each LF), and each line of at least MinLen bytes on its own (a
// key shown in part: scrolled, cut to its head or tail, read in pieces). All
// are without the trailing line break. JSONEscapedVariants derives the form
// inside a recording from each. Nil for a value with no line break inside it.
func MultilineRenderings(val []byte) [][]byte {
	body := bytes.TrimRight(val, "\r\n")
	if !bytes.Contains(body, []byte{'\n'}) {
		return nil
	}
	lf := bytes.ReplaceAll(body, []byte("\r\n"), []byte{'\n'})
	out := [][]byte{lf, bytes.ReplaceAll(lf, []byte{'\n'}, []byte("\r\n"))}
	for _, line := range bytes.Split(lf, []byte{'\n'}) {
		if line = bytes.TrimRight(line, "\r"); len(line) >= MinLen {
			out = append(out, line)
		}
	}
	return out
}
