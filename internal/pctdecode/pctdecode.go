// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package pctdecode is the percent-decoding step the path scope decoders
// share. Both the GitHub and the Azure DevOps decoders re-decode a segment to
// find structure hidden behind a second layer of encoding, and both must read
// escapes identically: a divergence is a bypass in whichever copy is laxer.
package pctdecode

import "strings"

// Lenient decodes every valid %XY in s once and keeps every malformed one as
// literal text. Lenient on purpose: a lenient decoder is the most dangerous
// one a request could meet, so a caller hunting hidden structure decodes the
// way it would.
func Lenient(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, lo := Unhex(s[i+1]), Unhex(s[i+2]); hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Unhex is one hex digit's value, or -1.
func Unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
