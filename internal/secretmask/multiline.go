// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import "bytes"

// MultilineRenderings returns the forms of a value that output can carry and
// the stored bytes do not match: the value without its trailing line break
// (a terminal prints that break as CR LF, or a shell reads it off); and, for a
// value with a line break inside it, that value with every line break as a bare
// LF and as CR LF, and each line of at least MinLen bytes on its own (a key
// shown in part: scrolled, cut to its head or tail, read in pieces).
// JSONEscapedVariants derives the form inside a recording from each. Nil for a
// value with no line break in it.
func MultilineRenderings(val []byte) [][]byte {
	body := bytes.TrimRight(val, "\r\n")
	var out [][]byte
	if len(body) > 0 && len(body) != len(val) {
		out = append(out, body)
	}
	if !bytes.Contains(body, []byte{'\n'}) {
		return out
	}
	lf := bytes.ReplaceAll(body, []byte("\r\n"), []byte{'\n'})
	out = append(out, lf, bytes.ReplaceAll(lf, []byte{'\n'}, []byte("\r\n")))
	for _, line := range bytes.Split(lf, []byte{'\n'}) {
		if line = bytes.TrimRight(line, "\r"); len(line) >= MinLen {
			out = append(out, line)
		}
	}
	return out
}

// TrimmedTooShort reports a value the masker cannot hide once its trailing line
// break is gone: at least MinLen bytes as stored, fewer without the break. A
// terminal or a shell's command substitution prints that shorter form, so a
// sink must not hand such a value out.
func TrimmedTooShort(val []byte) bool {
	body := bytes.TrimRight(val, "\r\n")
	return len(body) < len(val) && len(body) < MinLen
}
