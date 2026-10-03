// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package scim

import (
	"encoding/json"
	"strings"
)

// Filter is the one filter form Wardyn serves: `attr eq "value"`.
type Filter struct {
	// Attr is spelled as in the allowed list passed to ParseFilter, whatever case the client used.
	Attr  string
	Value string
}

// ParseFilter parses `attr eq "value"` where attr is one of allowed (compared case-insensitively, as SCIM
// attribute names are) and value is a JSON string literal. Anything else, including `and`/`or`, other
// operators, unquoted values and unlisted attributes, is invalidFilter. A client that sends an unquoted
// value (a Microsoft sample shows one) is refused too: RFC 7644 requires the quotes.
func ParseFilter(s string, allowed ...string) (Filter, *Error) {
	rest := strings.TrimLeft(s, " \t")
	attr, rest := cutToken(rest)
	canon := ""
	for _, a := range allowed {
		if strings.EqualFold(a, attr) {
			canon = a
			break
		}
	}
	if canon == "" {
		return Filter{}, badRequest(TypeInvalidFilter, "filter attribute is not supported; use one of: "+strings.Join(allowed, ", "))
	}
	op, rest := cutToken(strings.TrimLeft(rest, " \t"))
	if !strings.EqualFold(op, "eq") {
		return Filter{}, badRequest(TypeInvalidFilter, `only the "eq" operator is supported`)
	}
	rest = strings.TrimLeft(rest, " \t")
	end := quotedEnd(rest)
	if end < 0 {
		return Filter{}, badRequest(TypeInvalidFilter, `filter value must be a quoted string, as in attr eq "value"`)
	}
	var value string
	if err := json.Unmarshal([]byte(rest[:end]), &value); err != nil {
		return Filter{}, badRequest(TypeInvalidFilter, "filter value is not a valid string literal")
	}
	if strings.Trim(rest[end:], " \t") != "" {
		return Filter{}, badRequest(TypeInvalidFilter, `only a single "attr eq value" comparison is supported`)
	}
	return Filter{Attr: canon, Value: value}, nil
}

// cutToken splits off the leading run of non-blank bytes, requiring blank (or the end) after it.
func cutToken(s string) (tok, rest string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// quotedEnd returns the index just past the closing quote of the string literal that starts s, or -1.
func quotedEnd(s string) int {
	if s == "" || s[0] != '"' {
		return -1
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return -1
}
