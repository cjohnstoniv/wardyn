// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCanonicalJSON(t *testing.T) {
	ok := []struct{ name, in, want string }{
		{"key order and space", ` { "b" : 1 , "a" : [ true , null , "x" ] } `, `{"a":[true,null,"x"],"b":1}`},
		{"nested key order", `{"z":{"y":1,"x":2},"a":{}}`, `{"a":{},"z":{"x":2,"y":1}}`},
		{"number 1", `1`, `1`},
		{"number 1.0", `1.0`, `1`},
		{"number 1e0", `1e0`, `1`},
		{"number 10e-1", `10e-1`, `1`},
		{"negative zero", `-0.0`, `0`},
		{"fraction", `[0.50]`, `[0.5]`},
		{"escapes re-encoded", `"Aé\n"`, `"Aé\n"`},
		{"html escaped by encoding/json", `"<a&b>"`, "\"\\u003ca\\u0026b\\u003e\""},
		{"empty containers", `{"a":[],"b":{}}`, `{"a":[],"b":{}}`},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			got, err := CanonicalJSON([]byte(c.in))
			if err != nil {
				t.Fatalf("CanonicalJSON(%s): %v", c.in, err)
			}
			if string(got) != c.want {
				t.Fatalf("CanonicalJSON(%s) = %s, want %s", c.in, got, c.want)
			}
		})
	}
	// Equal values spell the same, so one approval covers every spelling.
	a, _ := CanonicalJSON([]byte(`{"n":1,"m":1.0}`))
	b, _ := CanonicalJSON([]byte(`{"m":1e0,"n":1.00}`))
	if string(a) != string(b) {
		t.Errorf("value-equal documents spell differently: %s vs %s", a, b)
	}

	t.Run("a repeated key is refused at any depth", func(t *testing.T) {
		for _, in := range []string{`{"a":1,"a":2}`, `{"x":[{"a":1,"a":1}]}`, `{"x":{"y":{"k":1,"k":2}}}`} {
			if _, err := CanonicalJSON([]byte(in)); !errors.Is(err, ErrCanonicalRepeatedKey) {
				t.Errorf("CanonicalJSON(%s) err = %v, want ErrCanonicalRepeatedKey", in, err)
			}
		}
	})
	t.Run("depth is capped at 64", func(t *testing.T) {
		deep := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
		if _, err := CanonicalJSON([]byte(deep(64))); err != nil {
			t.Errorf("depth 64 must be accepted: %v", err)
		}
		if _, err := CanonicalJSON([]byte(deep(65))); !errors.Is(err, ErrCanonicalDepth) {
			t.Errorf("depth 65 err = %v, want ErrCanonicalDepth", err)
		}
	})
	t.Run("malformed input is refused", func(t *testing.T) {
		for _, in := range []string{``, `{`, `{"a":1} {"b":2}`, `[1,]`, `nul`, `{"a":1}x`} {
			if _, err := CanonicalJSON([]byte(in)); err == nil {
				t.Errorf("CanonicalJSON(%q) must fail", in)
			}
		}
	})
	t.Run("canonical output is a fixed point", func(t *testing.T) {
		once, _ := CanonicalJSON([]byte(`{"b":[1.50,{"d":1e2,"c":"é"}],"a":null}`))
		twice, err := CanonicalJSON(once)
		if err != nil || string(once) != string(twice) {
			t.Errorf("not a fixed point: %s -> %s (%v)", once, twice, err)
		}
	})
}

func TestCanonicalNumber(t *testing.T) {
	for in, want := range map[string]string{
		"1": "1", "1.0": "1", "1e0": "1", "1E0": "1", "100e-2": "1", "0.0": "0", "-0": "0",
		"-1.50": "-1.5", "0.001": "0.001", "12e2": "1200", "1.5e1": "15",
	} {
		if got := CanonicalNumber(in); got != want {
			t.Errorf("CanonicalNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMCPCallKeyGateAndBrokerAgree: the gate sees the display name and the
// broker the raw one; where they are the same string the two sides must
// compute one key from differently spelled arguments.
func TestMCPCallKeyGateAndBrokerAgree(t *testing.T) {
	gate, err := MCPCallKey("jira", "delete_issue", json.RawMessage(`{ "id": 7, "force": false }`), nil)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := MCPCallKey("jira", "delete_issue", json.RawMessage(`{"force":false,"id":7.0}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if gate != broker {
		t.Fatalf("gate key %s != broker key %s for the same call", gate, broker)
	}
	if len(gate) != 64 {
		t.Errorf("key %q is not a hex SHA-256", gate)
	}
	absent, _ := MCPCallKey("jira", "t", nil, nil)
	empty, _ := MCPCallKey("jira", "t", json.RawMessage(`{}`), nil)
	if absent != empty {
		t.Error("absent arguments read as {}")
	}
}

// TestMCPCallKeyBindsRawName: two raw names that one display form maps to
// (Claude Code turns every rune outside [A-Za-z0-9_-] into "_") are two calls
// with two keys, so an approval for one is never spent on the other.
func TestMCPCallKeyBindsRawName(t *testing.T) {
	args := json.RawMessage(`{"id":1}`)
	dotted, err := MCPCallKey("jira", "delete.issue", args, nil)
	if err != nil {
		t.Fatal(err)
	}
	under, _ := MCPCallKey("jira", "delete_issue", args, nil)
	if dotted == under {
		t.Fatal("delete.issue and delete_issue share a key: an approval for one is spendable on the other")
	}
	for name, other := range map[string][3]string{
		"server":    {"gitlab", "delete_issue", `{"id":1}`},
		"arguments": {"jira", "delete_issue", `{"id":2}`},
	} {
		k, _ := MCPCallKey(other[0], other[1], json.RawMessage(other[2]), nil)
		if k == under {
			t.Errorf("a different %s keeps the same key", name)
		}
	}
	withExtra, _ := MCPCallKey("jira", "delete_issue", args, json.RawMessage(`{"task":{"ttl":1}}`))
	if withExtra == under {
		t.Error("extra params are part of the call and must change the key")
	}
	// Separator confusion: a boundary moved between fields is a different call.
	a, _ := MCPCallKey("a", "bc", nil, nil)
	b, _ := MCPCallKey("ab", "c", nil, nil)
	if a == b {
		t.Error("server and tool names run together in the key")
	}
}

func TestMCPCallKeyRefusals(t *testing.T) {
	if _, err := MCPCallKey("s", "t", json.RawMessage(`{"a":1,"a":2}`), nil); !errors.Is(err, ErrCanonicalRepeatedKey) {
		t.Errorf("repeated key err = %v", err)
	}
	big := json.RawMessage(`{"x":"` + strings.Repeat("a", MaxMCPCallBytes) + `"}`)
	if _, err := MCPCallKey("s", "t", big, nil); !errors.Is(err, ErrMCPCallTooLarge) {
		t.Errorf("oversize err = %v, want ErrMCPCallTooLarge (never truncated)", err)
	}
	if _, err := MCPCallKey("s", "t", json.RawMessage(`[1`), nil); err == nil {
		t.Error("malformed arguments must be refused")
	}
}

func TestMCPCallScopeKeyAndShape(t *testing.T) {
	args := json.RawMessage(`{"id":1}`)
	key, _ := MCPCallKey("jira", "delete.issue", args, nil)
	sc := MCPCallScope{
		Tool: "mcp__jira__delete_issue", MCPServer: "jira", MCPHost: "jira.example.com",
		MCPTool: "delete.issue", Arguments: args, CallSHA256: key,
	}
	got, err := sc.ComputeKey()
	if err != nil || got != key {
		t.Fatalf("ComputeKey = %q, %v; want %q (mcp_tool names the raw tool)", got, err, key)
	}
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !IsMCPCallScope(raw) {
		t.Errorf("%s is not recognised as an MCP call scope", raw)
	}
	// With no mcp_tool the raw name is the shown one.
	plain := MCPCallScope{Tool: "mcp__jira__delete_issue", MCPServer: "jira", Arguments: args}
	plain.CallSHA256, _ = MCPCallKey("jira", "delete_issue", args, nil)
	if got, _ := plain.ComputeKey(); got != plain.CallSHA256 {
		t.Errorf("shown==raw scope recomputes to %q, want %q", got, plain.CallSHA256)
	}
	// An edited argument no longer matches the hash the person approved.
	sc.Arguments = json.RawMessage(`{"id":2}`)
	if got, _ := sc.ComputeKey(); got == key {
		t.Error("a scope with edited arguments recomputed to the old key")
	}
	for _, in := range []string{``, `null`, `{}`, `{"tool":"Bash"}`, `{"call_sha256":""}`, `[1]`, `not json`} {
		if IsMCPCallScope(json.RawMessage(in)) {
			t.Errorf("IsMCPCallScope(%q) = true", in)
		}
	}
}
