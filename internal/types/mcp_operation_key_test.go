// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestMCPCallSendWindow: a send answers for its key for 24 hours and not a microsecond longer; at exactly
// 24 hours it no longer does, and a clock behind the send still answers.
func TestMCPCallSendWindow(t *testing.T) {
	if MCPCallSendWindow != 24*time.Hour {
		t.Fatalf("MCPCallSendWindow = %s, want the 24 hours the docs state", MCPCallSendWindow)
	}
	sent := time.Unix(1_000_000_000, 6000).UTC()
	for name, c := range map[string]struct {
		age  time.Duration
		live bool
	}{
		"just sent":                  {0, true},
		"a microsecond inside":       {24*time.Hour - time.Microsecond, true},
		"exactly 24 hours":           {24 * time.Hour, false},
		"a microsecond past":         {24*time.Hour + time.Microsecond, false},
		"a clock a minute behind it": {-time.Minute, true},
	} {
		if got := MCPCallSendLive(sent, sent.Add(c.age)); got != c.live {
			t.Errorf("%s: MCPCallSendLive = %v, want %v", name, got, c.live)
		}
	}
}

// TestValidOperationKey: a key is 1 to 128 of the correlation ID's characters, and nothing else is.
func TestValidOperationKey(t *testing.T) {
	for key, want := range map[string]bool{
		"step-1":                 true,
		"corr.42:build/test_2-A": true,
		strings.Repeat("a", 128): true,
		"":                       false,
		strings.Repeat("a", 129): false,
		"has space":              false,
		"line\nbreak":            false,
		"caf\u00e9":              false,
		"a+b":                    false,
		"nul\x00":                false,
	} {
		if got := ValidOperationKey(key); got != want {
			t.Errorf("ValidOperationKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestOperationKeyIsOutsideTheCallHash: the key is not hashed and not part of the call's text, a keyless
// scope marshals exactly as it did before the key existed, and a keyed one carries it.
func TestOperationKeyIsOutsideTheCallHash(t *testing.T) {
	plain := MCPCallScope{Tool: "mcp__jira__delete_issue", MCPServer: "jira", Arguments: json.RawMessage(`{"id":1}`)}
	keyed := plain
	keyed.OperationKey = "step-1"
	key, err := plain.ComputeKey()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := keyed.ComputeKey(); err != nil || got != key {
		t.Fatalf("keyed ComputeKey = %s, %v; want the keyless key %s", got, err, key)
	}
	plain.CallSHA256, keyed.CallSHA256 = key, key
	raw, err := json.Marshal(plain)
	if want := `{"tool":"mcp__jira__delete_issue","mcp_server":"jira","arguments":{"id":1},"call_sha256":"` + key + `"}`; err != nil || string(raw) != want {
		t.Fatalf("keyless scope\n got %s\nwant %s", raw, want)
	}
	withKey, err := json.Marshal(keyed)
	var back MCPCallScope
	if err != nil || json.Unmarshal(withKey, &back) != nil || back.OperationKey != "step-1" ||
		!strings.HasSuffix(string(withKey), `,"operation_key":"step-1"}`) {
		t.Fatalf("keyed scope %s round-trips to %+v", withKey, back)
	}
	if p, k := MCPCallTextOf(raw), MCPCallTextOf(withKey); p == nil || k == nil || *p != *k {
		t.Fatalf("call text keyless %+v, keyed %+v; want the same", p, k)
	}
}
