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
	if got := MultilineRenderings([]byte("sk-single-line-secret")); got != nil {
		t.Fatalf("a value with no line break got renderings %q", got)
	}
	m := maskerFor("sk-single-line-secret", nil)
	if got := string(m.Mask([]byte("key=sk-single-line-secret\r\n"))); got != "key=<secret-hidden>\r\n" {
		t.Fatalf("got %q", got)
	}
}

// A one-line value stored with a trailing line break is printed without it
// followed by CR LF, so the value as stored never matches.
func TestMultilineRenderingsTrailingBreakOfOneLine(t *testing.T) {
	const val = "sk-throwaway-example-12345\n"
	got := MultilineRenderings([]byte(val))
	if len(got) != 1 || string(got[0]) != "sk-throwaway-example-12345" {
		t.Fatalf("got %q, want the value without its line break", got)
	}
	m := maskerFor(val, got)
	pty := "$ echo $TOKEN\r\nsk-throwaway-example-12345\r\n"
	cast, err := json.Marshal(pty)
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{"CRLF": pty, "JSON-escaped": `[0.5,"o",` + string(cast) + `]`} {
		if out := string(m.Mask([]byte(in))); bytes.Contains([]byte(out), []byte("sk-throwaway-example-12345")) {
			t.Errorf("%s output leaks the value: %q", name, out)
		}
	}
	if out := string(maskerFor(val, nil).Mask([]byte(pty))); !bytes.Contains([]byte(out), []byte("sk-throwaway-example-12345")) {
		t.Errorf("control: the stored value alone was expected to miss CRLF output, got %q", out)
	}
}

func TestMultilineRenderingsLines(t *testing.T) {
	got := MultilineRenderings([]byte("ab\r\nlongenough-line\r\ncd\r\n"))
	var lines []string
	for _, g := range got {
		lines = append(lines, string(g))
	}
	want := []string{"ab\r\nlongenough-line\r\ncd", "ab\nlongenough-line\ncd", "ab\r\nlongenough-line\r\ncd", "longenough-line"}
	if len(lines) != len(want) {
		t.Fatalf("got %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("got %q, want %q", lines, want)
		}
	}
}

// A value that reaches MinLen only by its trailing line break is printed without it, shorter than
// the masker hides, so the delivery sinks refuse it; a value long enough either way, and a short one
// with no break (refused by the length check itself), are not this case.
func TestTrimmedTooShort(t *testing.T) {
	for in, want := range map[string]bool{
		"AbC1234\n":               true,
		"AbC1234\r\n":             true,
		"AbC12345\n":              false,
		"AbC12345":                false,
		"AbC1234":                 false,
		"sk-single-line-secret\n": false,
	} {
		if got := TrimmedTooShort([]byte(in)); got != want {
			t.Errorf("TrimmedTooShort(%q) = %v, want %v", in, got, want)
		}
	}
}
