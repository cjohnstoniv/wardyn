// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// CanonicalJSON rewrites one JSON value into a single spelling, so two documents that mean the same thing
// give the same bytes whatever wrote them or whichever store has since read them back:
//   - object members are sorted by key (a repeated key keeps its last value, as JSONB does);
//   - strings escape only `"`, `\` and the control characters below U+0020 (as \u00xx); everything else,
//     including `<`, `>`, `&` and U+2028, is written raw;
//   - numbers are written in plain decimal with no exponent, no trailing zeros and no "-0"
//     (1e2, 100 and 100.0 are all 100); one with an exponent beyond ±1000 is kept as written;
//   - there is no whitespace.
//
// A document that is not exactly one JSON value is an error.
func CanonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("types: canonical json: trailing data after the value")
	}
	var b bytes.Buffer
	writeCanonical(&b, v)
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		writeCanonicalString(b, t)
	case json.Number:
		b.WriteString(canonicalNumber(t.String()))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			writeCanonical(b, t[k])
		}
		b.WriteByte('}')
	}
}

func writeCanonicalString(b *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[r>>4])
			b.WriteByte(hexDigits[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// canonicalNumber is n in plain decimal form: an integer value as digits, anything else with its
// fraction to at most 40 places and no trailing zeros.
func canonicalNumber(n string) string {
	if i := strings.IndexAny(n, "eE"); i >= 0 {
		if exp, err := strconv.Atoi(n[i+1:]); err != nil || exp > 1000 || exp < -1000 {
			return n // too large to expand; kept as written
		}
	}
	r, ok := new(big.Rat).SetString(n)
	if !ok {
		return n
	}
	if r.IsInt() {
		return r.Num().String()
	}
	s := r.FloatString(40)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
