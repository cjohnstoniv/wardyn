// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// mcpCallTextFixture is a call whose numbers a JavaScript Number cannot hold: two adjacent integers
// past 2^53 and a decimal with more digits than a float64 keeps.
const (
	mcpCallTextArgs  = `{"ids":[9007199254740993,9007199254740995],"price":1.0000000000000001,"note":"a\nb \"q\""}`
	mcpCallTextExtra = `{"task":{"ttl":18446744073709551617}}`
)

func mcpCallTextScope(t *testing.T) json.RawMessage {
	t.Helper()
	s := MCPCallScope{Tool: "mcp__jira__delete_issue", MCPServer: "jira", Arguments: json.RawMessage(mcpCallTextArgs), Extra: json.RawMessage(mcpCallTextExtra)}
	key, err := s.ComputeKey()
	if err != nil {
		t.Fatal(err)
	}
	s.CallSHA256 = key
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The text a person reads and copies is the canonical arguments and extra params with only indentation
// added: every digit of every number and every string escape survives, because nothing parses a number.
func TestMCPCallTextIsLossless(t *testing.T) {
	got := MCPCallTextOf(mcpCallTextScope(t))
	if got == nil {
		t.Fatal("MCPCallTextOf = nil for an MCP call scope")
	}
	for _, digits := range []string{"9007199254740993", "9007199254740995", "1.0000000000000001"} {
		if !strings.Contains(got.Arguments, digits) {
			t.Errorf("arguments text lost %s:\n%s", digits, got.Arguments)
		}
	}
	if !strings.Contains(got.Extra, "18446744073709551617") {
		t.Errorf("extra text lost 18446744073709551617:\n%s", got.Extra)
	}
	if !strings.Contains(got.Arguments, `"a\nb \"q\""`) {
		t.Errorf("arguments text changed a string's escapes:\n%s", got.Arguments)
	}
	for name, c := range map[string]struct{ text, raw string }{"arguments": {got.Arguments, mcpCallTextArgs}, "extra": {got.Extra, mcpCallTextExtra}} {
		want, err := CanonicalJSON([]byte(c.raw))
		if err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(c.text)); err != nil {
			t.Fatalf("%s text is not JSON: %v", name, err)
		}
		if compact.String() != string(want) {
			t.Errorf("%s text compacts to %s, want the canonical %s", name, compact.String(), want)
		}
		if !strings.Contains(c.text, "\n") {
			t.Errorf("%s text is one line; want it indented so a person can read it", name)
		}
	}
}

// No extra params, no extra text; arguments absent read as {}; a scope that is not an MCP call has none.
func TestMCPCallTextEdges(t *testing.T) {
	got := MCPCallTextOf(json.RawMessage(`{"tool":"mcp__jira__ping","mcp_server":"jira","call_sha256":"k"}`))
	if got == nil || got.Arguments != "{}" || got.Extra != "" {
		t.Errorf("MCPCallTextOf(no arguments) = %+v, want arguments {} and no extra", got)
	}
	for _, scope := range []string{`{"host":"a.example"}`, `{"cmd":"ls"}`, `not json`, ``} {
		if MCPCallTextOf(json.RawMessage(scope)) != nil {
			t.Errorf("MCPCallTextOf(%q) is set, want nil", scope)
		}
	}
	// A call the key refuses (a repeated key) has no exact text to show, so it has none.
	bad, _ := json.Marshal(MCPCallScope{Tool: "mcp__jira__x", MCPServer: "jira", Arguments: json.RawMessage(`{"a":1,"a":2}`), CallSHA256: "k"})
	if MCPCallTextOf(bad) != nil {
		t.Error("MCPCallTextOf(repeated key) is set, want nil")
	}
}
