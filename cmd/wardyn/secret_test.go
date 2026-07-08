// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestReadSecretValue covers the multi-line secret truncation finding: reading a
// secret value from stdin must preserve the ENTIRE input (e.g. a PEM private
// key or a JSON blob spanning many lines), not just the first line. The old
// implementation used bufio.ReadString('\n') and silently truncated everything
// after the first newline.
func TestReadSecretValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single line trailing newline stripped",
			in:   "hunter2\n",
			want: "hunter2",
		},
		{
			name: "single line crlf stripped",
			in:   "hunter2\r\n",
			want: "hunter2",
		},
		{
			name: "single line no trailing newline",
			in:   "hunter2",
			want: "hunter2",
		},
		{
			name: "multi-line preserves internal newlines",
			in:   "-----BEGIN KEY-----\nline2\nline3\n-----END KEY-----\n",
			want: "-----BEGIN KEY-----\nline2\nline3\n-----END KEY-----",
		},
		{
			name: "multi-line no trailing newline",
			in:   "line1\nline2\nline3",
			want: "line1\nline2\nline3",
		},
		{
			name: "only a single trailing newline removed, blank lines kept",
			in:   "line1\n\nline3\n\n",
			want: "line1\n\nline3\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readSecretValue(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("readSecretValue(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("readSecretValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
