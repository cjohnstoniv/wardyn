// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package scim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// OpKind is a PatchOp operation, normalised to lower case.
type OpKind string

// The three RFC 7644 operations. Entra spells them `Add`, `Replace` and `Remove`; the match is case-insensitive.
const (
	OpAdd     OpKind = "add"
	OpReplace OpKind = "replace"
	OpRemove  OpKind = "remove"
)

// Canonical attribute names of the attributes Wardyn acts on. Op.Attr is one of these, or, for any other
// attribute, the path exactly as the client sent it.
const (
	AttrActive      = "active"
	AttrUserName    = "userName"
	AttrExternalID  = "externalId"
	AttrDisplayName = "displayName"
	AttrEmail       = "emails.value"
	AttrMembers     = "members"
)

// Op is one resolved PatchOp change. A path-less replace carrying a value object becomes one Op per key,
// all with the same Index.
//
// Value by Attr: AttrActive is a bool (Entra's string "False" and "True" are accepted); AttrUserName,
// AttrExternalID, AttrDisplayName and AttrEmail are a non-empty string; AttrMembers is a []Member (a
// filter-path remove becomes one member; nil on a remove means every member); any other attribute is its
// json.RawMessage, unvalidated, for the caller to ignore. A remove carries a nil Value except for members.
// The `emails[type eq "work"]` selector is dropped: Wardyn keeps one address per person.
type Op struct {
	Index int
	Kind  OpKind
	Attr  string
	Value any
}

// OpError reports one invalid operation of a PATCH by its position in Operations.
type OpError struct {
	Index int
	*Error
}

// PatchError is the refusal of a whole PATCH. Invalid is empty when the request envelope itself was bad
// (not JSON, wrong schema, no Operations).
type PatchError struct {
	*Error
	Invalid []OpError
}

type rawOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// ParsePatch is the pre-validation pass: it resolves every operation of the PATCH before the caller applies
// any, so a request with one invalid operation yields a *PatchError listing each bad operation by index and
// no ops at all. It never returns a partial result. Whether a well-formed operation is allowed (an
// externalId change on a bound identity, say) is the caller's decision, per Op.
func ParsePatch(body []byte) ([]Op, *PatchError) {
	var req struct {
		Schemas    []string `json:"schemas"`
		Operations []rawOp  `json:"Operations"`
	}
	if err := decodeOne(body, &req); err != nil {
		return nil, &PatchError{Error: err}
	}
	if !slices.ContainsFunc(req.Schemas, func(s string) bool { return strings.EqualFold(s, SchemaPatchOp) }) {
		return nil, &PatchError{Error: badRequest(TypeInvalidSyntax, "schemas must include "+SchemaPatchOp)}
	}
	if len(req.Operations) == 0 {
		return nil, &PatchError{Error: badRequest(TypeInvalidSyntax, "Operations must hold at least one operation")}
	}
	var ops []Op
	var bad []OpError
	for i, r := range req.Operations {
		resolved, err := resolveOp(i, r)
		if err != nil {
			bad = append(bad, OpError{Index: i, Error: err})
			continue
		}
		ops = append(ops, resolved...)
	}
	if len(bad) == 0 {
		return ops, nil
	}
	idx := make([]int, len(bad))
	for i, b := range bad {
		idx[i] = b.Index
	}
	first := bad[0].Error
	return nil, &PatchError{
		Error:   badRequest(first.ScimType, fmt.Sprintf("operations %v are invalid; first: %s", idx, first.Detail)),
		Invalid: bad,
	}
}

func resolveOp(index int, r rawOp) ([]Op, *Error) {
	kind := OpKind(strings.ToLower(r.Op))
	if kind != OpAdd && kind != OpReplace && kind != OpRemove {
		return nil, badRequest(TypeInvalidSyntax, `op must be "add", "replace" or "remove"`)
	}
	if r.Path != "" {
		o, err := resolveTarget(index, kind, r.Path, r.Value)
		if err != nil {
			return nil, err
		}
		return []Op{o}, nil
	}
	if kind == OpRemove {
		return nil, badRequest(TypeNoTarget, "remove needs a path")
	}
	// Entra's path-less form: the value is an object of attribute paths to values.
	var attrs map[string]json.RawMessage
	if json.Unmarshal(r.Value, &attrs) != nil || len(attrs) == 0 {
		return nil, badRequest(TypeInvalidValue, "an operation without a path needs a value object of attributes")
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	ops := make([]Op, 0, len(keys))
	for _, k := range keys {
		o, err := resolveTarget(index, kind, k, attrs[k])
		if err != nil {
			return nil, err
		}
		ops = append(ops, o)
	}
	return ops, nil
}

func resolveTarget(index int, kind OpKind, path string, raw json.RawMessage) (Op, *Error) {
	attr, sel, err := parsePath(path)
	if err != nil {
		return Op{}, err
	}
	op := Op{Index: index, Kind: kind, Attr: attr}
	present := len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	if attr == AttrMembers {
		op.Value, err = memberValue(kind, sel, raw, present)
		return op, err
	}
	if kind == OpRemove {
		return op, nil
	}
	if !present {
		return Op{}, badRequest(TypeInvalidValue, attr+" needs a value")
	}
	op.Value, err = scalarValue(attr, raw)
	return op, err
}

func scalarValue(attr string, raw json.RawMessage) (any, *Error) {
	switch attr {
	case AttrActive:
		var b flexBool
		if json.Unmarshal(raw, &b) != nil || !b.set {
			return nil, badRequest(TypeInvalidValue, `active must be true or false (the strings "True" and "False" are accepted)`)
		}
		return b.v, nil
	case AttrUserName, AttrExternalID, AttrDisplayName, AttrEmail:
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return nil, badRequest(TypeInvalidValue, attr+" must be a non-empty string")
		}
		return s, nil
	}
	return slices.Clone(raw), nil
}

func memberValue(kind OpKind, sel *Filter, raw json.RawMessage, present bool) (any, *Error) {
	switch {
	case kind == OpReplace:
		return nil, badRequest(TypeInvalidValue, "replacing members is not supported; use add and remove")
	case sel != nil && (kind == OpAdd || present):
		return nil, badRequest(TypeInvalidPath, "a members filter path is only valid on remove, without a value")
	case sel != nil:
		return []Member{{Value: sel.Value}}, nil
	case !present && kind == OpRemove:
		return nil, nil
	case !present:
		return nil, badRequest(TypeInvalidValue, "adding members needs a value array of member refs")
	}
	var ms []Member
	if json.Unmarshal(raw, &ms) != nil || len(ms) == 0 || slices.ContainsFunc(ms, func(m Member) bool { return m.Value == "" }) {
		return nil, badRequest(TypeInvalidValue, `members value must be a non-empty array of {"value": "<id>"} refs`)
	}
	return ms, nil
}

// parsePath resolves a PatchOp path to the canonical attribute Wardyn acts on, plus the members filter
// selector when the path has one. Paths of attributes Wardyn does not act on come back verbatim.
func parsePath(path string) (attr string, sel *Filter, err *Error) {
	p := path
	for _, schema := range []string{SchemaUser, SchemaGroup} {
		if len(p) > len(schema) && strings.EqualFold(p[:len(schema)+1], schema+":") {
			p = p[len(schema)+1:]
		}
	}
	open := strings.IndexByte(p, '[')
	if open < 0 {
		return canonicalAttr(p, path), nil, nil
	}
	head := strings.ToLower(p[:open])
	if head != "members" && head != "emails" {
		return path, nil, nil
	}
	end := quotedAwareClose(p, open)
	if end < 0 {
		return "", nil, badRequest(TypeInvalidPath, "path has an unclosed filter bracket")
	}
	allowed := []string{"value"}
	if head == "emails" {
		allowed = []string{"type", "value"}
	}
	f, ferr := ParseFilter(p[open+1:end], allowed...)
	if ferr != nil {
		return "", nil, ferr
	}
	sub := p[end+1:]
	switch {
	case head == "members" && sub == "":
		return AttrMembers, &f, nil
	case head == "emails" && strings.EqualFold(sub, ".value"):
		return AttrEmail, nil, nil
	case head == "members":
		return "", nil, badRequest(TypeInvalidPath, "members has no sub-attribute "+sub)
	}
	return path, nil, nil
}

func canonicalAttr(p, original string) string {
	for _, a := range []string{AttrActive, AttrUserName, AttrExternalID, AttrDisplayName, AttrEmail, AttrMembers} {
		if strings.EqualFold(a, p) {
			return a
		}
	}
	return original
}

// quotedAwareClose returns the index of the ']' that closes the '[' at open, skipping string literals.
func quotedAwareClose(s string, open int) int {
	inQuote := false
	for i := open + 1; i < len(s); i++ {
		switch {
		case inQuote && s[i] == '\\':
			i++
		case s[i] == '"':
			inQuote = !inQuote
		case s[i] == ']' && !inQuote:
			return i
		}
	}
	return -1
}

// flexBool decodes a JSON boolean or Entra's strings "True" and "False" (any case); set is false for
// null, and any other value is an error.
type flexBool struct{ set, v bool }

func (b *flexBool) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case nil:
	case bool:
		*b = flexBool{true, t}
	case string:
		switch strings.ToLower(t) {
		case "true":
			*b = flexBool{true, true}
		case "false":
			*b = flexBool{true, false}
		default:
			return fmt.Errorf("not a boolean: %q", t)
		}
	default:
		return fmt.Errorf("not a boolean: %s", data)
	}
	return nil
}
