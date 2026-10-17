// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package armscope is the Azure Resource Manager gate, the sibling of
// internal/ghscope and internal/adoscope: it says whether one request to
// management.azure.com stays inside the subscriptions a run was given, and
// whether a run that may only read is trying to change something.
//
// A person's Resource Manager token carries that person's whole Azure role
// assignment in every subscription and tenant-level scope they can reach, so
// this check on the request line is the only thing that holds a run to its
// row. It reads the method and the path alone: a body that names a resource in
// another subscription (a move, a nested deployment, a cross-subscription
// link) is Azure's own role check to refuse.
//
// The package is PURE: the standard library and adoscope's segment decoder.
//
// FAIL-CLOSED IS THE CONTRACT: Admit refuses a request it cannot read as one
// method on one path, and every path it does not name.
package armscope

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// Host is the one Resource Manager host this gate judges. The sovereign-cloud
// and data-plane hosts (Storage, Key Vault, Graph) are other audiences and are
// absent on purpose.
const Host = "management.azure.com"

// The three refusals. A caller tells them apart with errors.Is; each error's
// text is the clause a refusal sentence carries.
var (
	// ErrUnreadable: the method or the path cannot be read as one operation on
	// one resource, so what Azure would act on is not knowable.
	ErrUnreadable = errors.New("it could not be read as one Azure Resource Manager operation")
	// ErrOutside: the path is not under one of the scope's subscriptions and is
	// not one of the tenant-level reads.
	ErrOutside = errors.New("it reaches outside this run's Azure subscriptions")
	// ErrReadOnly: the scope may only read, and the method changes something.
	ErrReadOnly = errors.New("this run's Azure CLI may only read, and this request is not a GET or HEAD")
)

// Scope is what one run may do on Resource Manager.
type Scope struct {
	// Subscriptions are the subscription ids the run is pinned to, each as
	// Subscription spells it.
	Subscriptions []string
	// Write lets the run use POST, PUT, PATCH and DELETE under them. Without
	// it the run may only GET and HEAD, so a POST that returns secrets
	// (…/listKeys) is refused with every other POST.
	Write bool
}

// tenantReads are the paths outside any subscription a run may read: what the
// CLI lists to describe the account it is signed in to. Exact paths, never
// prefixes: /providers/Microsoft.Management/… and the other tenant-level
// resource providers stay refused.
var tenantReads = []string{"subscriptions", "providers", "tenants"}

// methodOverride reports whether a header name is a spelling of "act as this
// method instead": X-HTTP-Method-Override, X-HTTP-Method, X-Method-Override,
// X-Ms-Http-Method, X-Method, in any case and with hyphens or underscores. Whether
// Resource Manager honours one is not documented, so a request that carries
// one is refused: the method Azure would act on is not knowable.
func methodOverride(header string) bool {
	return strings.Contains(normalizedHeader(header), "method")
}

// pathOverride reports whether a header name asks a front end to act on another
// path than the request line's: X-Original-URL, X-Original-URI, X-Rewrite-URL,
// X-Forwarded-Prefix, in any case and with hyphens or underscores. Refused for the
// same reason as methodOverride: the path Azure would act on is not knowable.
func pathOverride(header string) bool {
	name := normalizedHeader(header)
	for _, part := range []string{"originalurl", "originaluri", "rewriteurl", "forwardedprefix"} {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

func normalizedHeader(header string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(header))
}

// Subscription is id as this package compares it: a GUID in lowercase. ok is
// false for anything else.
func Subscription(id string) (string, bool) {
	if len(id) != 36 {
		return "", false
	}
	for i := range len(id) {
		c := id[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return "", false
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return "", false
		}
	}
	return strings.ToLower(id), true
}

// Admit reports whether the scope admits one request: nil admits it, and any
// error is one of the three refusals above. method is the request line's
// method, rawPath its path as it arrived (still percent-encoded, no query),
// and h its headers (nil is none).
func (s Scope) Admit(method, rawPath string, h http.Header) error {
	read, err := readMethod(method, h)
	if err != nil {
		return err
	}
	segs, err := segments(rawPath)
	if err != nil {
		return err
	}
	if len(segs) == 1 && slices.Contains(tenantReads, segs[0]) {
		if !read {
			return ErrOutside
		}
		return nil
	}
	if len(segs) < 2 || segs[0] != "subscriptions" || !slices.Contains(s.Subscriptions, segs[1]) {
		return ErrOutside
	}
	if !read && !s.Write {
		return ErrReadOnly
	}
	return nil
}

// readMethod classifies the method: GET and HEAD read, POST, PUT, PATCH and
// DELETE write, and anything else is refused.
func readMethod(method string, h http.Header) (bool, error) {
	for k := range h {
		if methodOverride(k) || pathOverride(k) {
			return false, fmt.Errorf("%w: it carries %s", ErrUnreadable, k)
		}
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return true, nil
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return false, nil
	}
	return false, fmt.Errorf("%w: %q is not a method this gate classifies", ErrUnreadable, method)
}

// segmentMarks are the characters no segment may hold, written raw or
// percent-encoded: a question mark and a hash end the path, and a percent sign
// left after one decode is a second layer of encoding a server that decodes
// again would read differently. A semicolon is judged apart (pathParameter).
const segmentMarks = "?#%"

// pathParameter reports whether seg, the decoded segment at index i, uses a
// semicolon in a way that could change which path is routed. A semicolon starts
// a path parameter some servers drop before routing, so "..;" can route as "..",
// and one in the first two segments could change which subscription is reached.
// Resource Manager's own names use it later in a path ("VM;iaasvmcontainerv2;rg;vm"
// is a backup item), so there it is refused only when nothing precedes it or what
// precedes it is a dot or a lookalike for one.
func pathParameter(i int, seg string) bool {
	head, _, found := strings.Cut(seg, ";")
	if !found {
		return false
	}
	return i < 2 || head == "" || dotLookalike(head)
}

// segments decodes rawPath into its lowercased segments. Resource Manager
// compares paths without regard to case, so the comparison here does too. One
// trailing slash is tolerated. Refused rather than resolved: an empty segment
// anywhere else, a backslash, a segment adoscope.UnescapeName would not route
// as it reads (a dot segment, an encoded separator, a control character), and
// any of segmentMarks in the path as it arrived or in a segment once decoded, and
// a semicolon where pathParameter refuses it.
func segments(rawPath string) ([]string, error) {
	if !strings.HasPrefix(rawPath, "/") || strings.ContainsAny(rawPath, `\?#`) {
		return nil, fmt.Errorf("%w: its path is not a plain absolute path", ErrUnreadable)
	}
	rest := strings.TrimSuffix(rawPath[1:], "/")
	if rest == "" {
		return nil, nil
	}
	parts := strings.Split(rest, "/")
	for i, raw := range parts {
		if raw == "" {
			return nil, fmt.Errorf("%w: its path has an empty segment", ErrUnreadable)
		}
		seg, err := adoscope.UnescapeName(raw)
		if err != nil || strings.ContainsAny(seg, segmentMarks) || pathParameter(i, seg) {
			return nil, fmt.Errorf("%w: a path segment is spelled so that it could be routed as another", ErrUnreadable)
		}
		if dotLookalike(seg) {
			return nil, fmt.Errorf("%w: a path segment is spelled so that it could be routed as another", ErrUnreadable)
		}
		parts[i] = strings.ToLower(seg)
	}
	return parts, nil
}

// dotLookalike reports whether seg has no letter or digit and holds a dot or a
// non-ASCII character: a fullwidth dot, ".." with a zero-width character or
// "..+" is a dot segment to a layer that folds or strips it. The invisible
// fillers Unicode files under letters (the Hangul fillers, among others) are
// not letters here: they draw nothing, and a layer that strips them leaves a dot.
func dotLookalike(seg string) bool {
	if strings.IndexFunc(seg, func(r rune) bool {
		return (unicode.IsLetter(r) || unicode.IsDigit(r)) && !unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r)
	}) >= 0 {
		return false
	}
	return strings.IndexFunc(seg, func(r rune) bool { return r == '.' || r > unicode.MaxASCII }) >= 0
}
