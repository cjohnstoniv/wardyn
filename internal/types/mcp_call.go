// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// The call key and the canonical form of an MCP call's arguments.
//
// An approve_each rule makes a person say yes to one call: one tool of one
// server with its exact arguments. The key below is the identity of that call.
// The proxy that raises the approval, the control plane that checks it and the
// broker that spends it all compute it here, so they cannot disagree.

// MaxMCPCallBytes caps the canonical arguments plus extra params of one call.
// A larger call is refused, never truncated: a person must see all of it.
const MaxMCPCallBytes = 32 << 10

// maxCanonicalDepth caps how deeply arrays and objects may nest.
const maxCanonicalDepth = 64

// maxJSONBDepth is encoding/json's own nesting limit, so any value json.Marshal produced
// canonicalizes under JSONB's rules.
const maxJSONBDepth = 10000

// canonMode is one set of canonicalization rules.
type canonMode struct {
	lastKeyWins bool // a repeated key keeps its last value, as JSONB does, instead of being refused
	maxDepth    int
}

var (
	canonStrict = canonMode{maxDepth: maxCanonicalDepth}
	canonJSONB  = canonMode{lastKeyWins: true, maxDepth: maxJSONBDepth}
)

var (
	// ErrCanonicalRepeatedKey means an object repeats a key. Two parsers can read the
	// two values differently, so such a call cannot be shown exactly and is refused.
	ErrCanonicalRepeatedKey = errors.New("a JSON object repeats a key")
	// ErrCanonicalDepth means the value nests deeper than 64 levels.
	ErrCanonicalDepth = errors.New("JSON nests deeper than 64 levels")
	// ErrMCPCallTooLarge means the canonical call is over MaxMCPCallBytes.
	ErrMCPCallTooLarge = errors.New("the call's arguments are larger than 32 KiB")
)

// CanonicalJSON is raw in one spelling per JSON value: object keys sorted, no
// insignificant space, numbers through CanonicalNumber, strings re-encoded by
// encoding/json. It refuses a repeated key at any depth, nesting over 64 levels,
// trailing data and anything that is not exactly one JSON value.
func CanonicalJSON(raw []byte) ([]byte, error) {
	return canonical(raw, canonStrict)
}

// CanonicalJSONB is the canonical form under JSONB's rules: a repeated key keeps its last value;
// used for evidence digests that must be recomputable from a stored row. It nests to 10000 levels.
func CanonicalJSONB(raw []byte) ([]byte, error) {
	return canonical(raw, canonJSONB)
}

func canonical(raw []byte, m canonMode) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out bytes.Buffer
	if err := canonValue(dec, &out, 0, m); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("JSON has data after its first value")
	}
	return out.Bytes(), nil
}

func canonValue(dec *json.Decoder, out *bytes.Buffer, depth int, m canonMode) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("read JSON: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth >= m.maxDepth {
			if m.maxDepth == maxCanonicalDepth {
				return ErrCanonicalDepth
			}
			return fmt.Errorf("JSON nests deeper than %d levels", m.maxDepth)
		}
		if t == '[' {
			return canonArray(dec, out, depth+1, m)
		}
		return canonObject(dec, out, depth+1, m)
	case json.Number:
		out.WriteString(CanonicalNumber(string(t)))
	default: // string, bool, nil
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("write JSON: %w", err)
		}
		out.Write(b)
	}
	return nil
}

func canonArray(dec *json.Decoder, out *bytes.Buffer, depth int, m canonMode) error {
	out.WriteByte('[')
	for first := true; dec.More(); first = false {
		if !first {
			out.WriteByte(',')
		}
		if err := canonValue(dec, out, depth, m); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing ]
		return fmt.Errorf("read JSON: %w", err)
	}
	out.WriteByte(']')
	return nil
}

func canonObject(dec *json.Decoder, out *bytes.Buffer, depth int, m canonMode) error {
	members := map[string][]byte{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return fmt.Errorf("read JSON: %w", err)
		}
		key, _ := kt.(string)
		if _, dup := members[key]; dup && !m.lastKeyWins {
			return ErrCanonicalRepeatedKey
		}
		var v bytes.Buffer
		if err := canonValue(dec, &v, depth, m); err != nil {
			return err
		}
		members[key] = v.Bytes()
	}
	if _, err := dec.Token(); err != nil { // the closing }
		return fmt.Errorf("read JSON: %w", err)
	}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		out.Write(kb)
		out.WriteByte(':')
		out.Write(members[k])
	}
	out.WriteByte('}')
	return nil
}

// CanonicalNumber is one spelling per value of a JSON number, made from its decimal digits and never
// through float64: no exponent, no trailing fractional zeros, and no minus on zero, as Postgres
// prints a numeric with no excess scale; past 20 zeros of padding, the significant digits with an
// exponent, which Postgres reads to the same numeric. An exponent too large to compute with is
// returned as written (Postgres refuses such a number).
func CanonicalNumber(n string) string {
	neg := strings.HasPrefix(n, "-")
	mant, expPart, hasExp := strings.Cut(strings.TrimPrefix(n, "-"), "e")
	if !hasExp {
		mant, expPart, hasExp = strings.Cut(mant, "E")
	}
	exp := 0
	if hasExp {
		e, err := strconv.Atoi(expPart)
		if err != nil || e > 1<<40 || e < -(1<<40) {
			return n
		}
		exp = e
	}
	whole, frac, _ := strings.Cut(mant, ".")
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return "0"
	}
	q := exp - len(frac) // the value is digits × 10^q
	for strings.HasSuffix(digits, "0") {
		digits, q = digits[:len(digits)-1], q+1
	}
	var out string
	switch point := len(digits) + q; {
	case q >= 0 && q <= 20:
		out = digits + strings.Repeat("0", q)
	case q < 0 && point > 0:
		out = digits[:point] + "." + digits[point:]
	case q < 0 && point > -20:
		out = "0." + strings.Repeat("0", -point) + digits
	default:
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		out += "e" + strconv.Itoa(point-1)
	}
	if neg {
		return "-" + out
	}
	return out
}

// MCPCallKey is the identity of one MCP call: the hex SHA-256 of the server's key, the tool's RAW
// name as the server knows it, the canonical arguments and the canonical extra params. arguments
// absent reads as {} and extra absent as nothing. The raw name is the key's, not a display form:
// Claude Code shows every rune outside [A-Za-z0-9_-] as "_", so delete.issue and delete_issue
// share one display name but are two tools, and an approval for one is never spendable on the
// other. The harness is not in the key: any process in the sandbox can use any harness's broker
// address, and the person approved that exact call on that server.
func MCPCallKey(serverKey, rawTool string, arguments, extra json.RawMessage) (string, error) {
	args, ext, err := canonCall(arguments, extra)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte("wardyn.mcp-call.v1"), []byte(serverKey), []byte(rawTool), args, ext} {
		fmt.Fprintf(h, "%d:", len(part)) // length-prefixed, so no field can run into the next
		h.Write(part)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonCall is a call's canonical arguments and extra params, refused over MaxMCPCallBytes.
func canonCall(arguments, extra json.RawMessage) (args, ext []byte, err error) {
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if args, err = CanonicalJSON(arguments); err != nil {
		return nil, nil, fmt.Errorf("arguments: %w", err)
	}
	if len(bytes.TrimSpace(extra)) > 0 {
		if ext, err = CanonicalJSON(extra); err != nil {
			return nil, nil, fmt.Errorf("extra params: %w", err)
		}
		if string(ext) == "{}" {
			ext = nil // no other params, however it was spelled
		}
	}
	if len(args)+len(ext) > MaxMCPCallBytes {
		return nil, nil, ErrMCPCallTooLarge
	}
	return args, ext, nil
}

// MCPCallScope is the requested_scope of a tool_call approval for one MCP call. It is known
// structurally by call_sha256 (with no grant), and only the proxy writes it.
type MCPCallScope struct {
	// Tool is the display name, mcp__<server>__<tool>, the name the gated lane's toolgate sees.
	Tool      string `json:"tool"`
	MCPServer string `json:"mcp_server"`
	MCPHost   string `json:"mcp_host,omitempty"`
	// MCPTool is the raw tool name, set only when it differs from the display name.
	MCPTool string `json:"mcp_tool,omitempty"`
	// Arguments and Extra are canonical.
	Arguments  json.RawMessage `json:"arguments"`
	Extra      json.RawMessage `json:"extra,omitempty"`
	CallSHA256 string          `json:"call_sha256"`
	// Harness is set only on a run whose harnesses each hold a credential.
	Harness string `json:"harness,omitempty"`
	// HoldSeconds is set when the proxy holds the open request for the answer; absent when the
	// gate raised the row and the toolgate waits.
	HoldSeconds int `json:"hold_seconds,omitempty"`
}

// ComputeKey recomputes the key from the scope's own fields, so what a person sees is what was
// hashed. The raw tool name is MCPTool, else the display name without its mcp__<server>__ prefix.
func (s MCPCallScope) ComputeKey() (string, error) {
	raw := s.MCPTool
	if raw == "" {
		raw = strings.TrimPrefix(s.Tool, "mcp__"+s.MCPServer+"__")
	}
	return MCPCallKey(s.MCPServer, raw, s.Arguments, s.Extra)
}

// IsMCPCallScope reports whether raw is an MCP call scope: an object with a non-empty call_sha256
// that is not a kubectl call scope (IsKubeCallScope) or a host call scope (IsHostCallScope).
func IsMCPCallScope(raw json.RawMessage) bool {
	var s struct {
		CallSHA256 string `json:"call_sha256"`
	}
	return json.Unmarshal(raw, &s) == nil && s.CallSHA256 != "" && !IsKubeCallScope(raw) && !IsHostCallScope(raw)
}

// MCPCallText is an MCP call's canonical arguments and extra params as text, for a person to read and
// copy. It is the canonical bytes with indentation added and nothing else: no number is parsed, so
// every digit the approval binds is shown, where a JavaScript client reading the scope as JSON would
// round 9007199254740993 to ...992. A client shows and copies these strings as they are.
type MCPCallText struct {
	Arguments string `json:"arguments"`
	Extra     string `json:"extra,omitempty"`
}

// MCPCallTextOf is the text of the MCP call scope raw, or nil when raw is not one or its arguments
// cannot be put in canonical form (a call the key refuses has no exact text to show).
func MCPCallTextOf(raw json.RawMessage) *MCPCallText {
	if !IsMCPCallScope(raw) {
		return nil
	}
	var s MCPCallScope
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	args, ext, err := canonCall(s.Arguments, s.Extra)
	if err != nil {
		return nil
	}
	out := &MCPCallText{Arguments: indentCanonical(args)}
	if len(ext) > 0 {
		out.Extra = indentCanonical(ext)
	}
	return out
}

// indentCanonical adds two-space indentation to canonical JSON. json.Indent works on the bytes, so
// number and string spellings pass through untouched.
func indentCanonical(canon []byte) string {
	var b bytes.Buffer
	if json.Indent(&b, canon, "", "  ") != nil {
		return string(canon)
	}
	return b.String()
}
