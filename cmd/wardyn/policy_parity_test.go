// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPolicyToJSON_ReadsWhatTheConsoleReads pins the CLI's policy reader to the
// console's: one policy text must never mean two policies. The console's
// parser (ui/src/app/lib/policy-document, parity.test.ts) asserts each
// snippet's outcome in testdata/policy-reader-parity.json; for every snippet it
// accepts, policyToJSON must produce the same JSON. For a snippet it refuses,
// the file records what the CLI reads instead, the reason for the refusal.
func TestPolicyToJSON_ReadsWhatTheConsoleReads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "policy-reader-parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string `json:"name"`
		Source  string `json:"source"`
		Console struct {
			Value json.RawMessage `json:"value"`
			Error string          `json:"error"`
		} `json:"console"`
		CLI json.RawMessage `json:"cli"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			want := c.Console.Value
			if c.Console.Error != "" {
				want = c.CLI
			} else {
				accepted++
			}
			if (c.Console.Error == "") == (c.Console.Value == nil) || want == nil {
				t.Fatalf("case needs exactly one console outcome, and a cli reading when refused")
			}
			got, err := policyToJSON([]byte(c.Source))
			if err != nil {
				t.Fatalf("policyToJSON: %v", err)
			}
			if g, w := canonicalJSON(t, got), canonicalJSON(t, want); !bytes.Equal(g, w) {
				t.Fatalf("CLI reads %s\nwant         %s", g, w)
			}
		})
	}
	if accepted < 8 {
		t.Fatalf("only %d console-accepted snippets; the fixture lost its parity cases", accepted)
	}
}

// canonicalJSON re-encodes through any, so key order and number spelling
// (an int from yaml.v3, a float64 from JSON) compare equal.
func canonicalJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
