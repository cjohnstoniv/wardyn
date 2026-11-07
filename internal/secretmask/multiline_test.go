// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"bytes"
	"encoding/json"
	"testing"
)

const multilineKey = "-----BEGIN KEY-----\nabcdefghijklmnop\nqrstuvwxyz012345\n-----END KEY-----\n"

// maskerFor is the masker a run holding val as a delivered secret has: the
// stored bytes and renderings, with their JSON-escaped forms.
func maskerFor(val string, extra [][]byte) Masker {
	return NewMasker(JSONEscapedVariants(append([][]byte{[]byte(val)}, extra...)))
}

func TestMultilineRenderingsMaskTerminalOutput(t *testing.T) {
	m := maskerFor(multilineKey, MultilineRenderings([]byte(multilineKey)))
	crlf := bytes.ReplaceAll([]byte(multilineKey), []byte("\n"), []byte("\r\n"))
	cast, err := json.Marshal(string(crlf))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, in string }{
		{"LF", multilineKey},
		{"CRLF", string(crlf)},
		{"CRLF in an asciicast event", `[0.5,"o",` + string(cast) + `]`},
		{"one line of it", "head: abcdefghijklmnop\r\n"},
		{"the last line alone", "$ tail -1 key\r\nqrstuvwxyz012345\r\n"},
	} {
		got := string(m.Mask([]byte(c.in)))
		for _, leak := range []string{"abcdefghijklmnop", "qrstuvwxyz012345", "-----BEGIN KEY-----", "-----END KEY-----"} {
			if bytes.Contains([]byte(got), []byte(leak)) {
				t.Errorf("%s: %q leaks %q", c.name, got, leak)
			}
		}
	}
}

func TestMultilineRenderingsSingleLineControl(t *testing.T) {
	if got := MultilineRenderings([]byte("sk-single-line-secret\n")); got != nil {
		t.Fatalf("a single-line value got renderings %q", got)
	}
	m := maskerFor("sk-single-line-secret", nil)
	if got := string(m.Mask([]byte("key=sk-single-line-secret\r\n"))); got != "key=<secret-hidden>\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestMultilineRenderingsLines(t *testing.T) {
	got := MultilineRenderings([]byte("ab\r\nlongenough-line\r\ncd\r\n"))
	var lines []string
	for _, g := range got {
		lines = append(lines, string(g))
	}
	want := []string{"ab\nlongenough-line\ncd", "ab\r\nlongenough-line\r\ncd", "longenough-line"}
	if len(lines) != len(want) {
		t.Fatalf("got %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("got %q, want %q", lines, want)
		}
	}
}
