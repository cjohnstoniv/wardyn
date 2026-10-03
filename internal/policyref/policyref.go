// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package policyref holds the contact a governance profile (or the site's
// policy_help) publishes to the people it refuses, and the one validator for it.
//
// The fields are later written into HTTP headers a sandbox reads, so a value is
// checked twice: Validate on every write, and Project on every read, which drops
// field by field whatever no longer passes (a row written by an older binary or
// straight into the database, or a rule that tightened since). A stored value is
// never trusted. Leaf package: the standard library and internal/hostrules only.
package policyref

import (
	"errors"
	"net/mail"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
)

const (
	ownerMax       = 200  // characters
	emailMax       = 254  // bytes
	requestURLMax  = 2048 // bytes
	requestTextMax = 1000 // characters, the sign-in help cap

	// SourceProfile and SourceDeployment say what bound the person.
	SourceProfile    = "profile"
	SourceDeployment = "deployment"

	mailtoScheme = "mailto:"
)

// Contact is what an admin writes: who owns a policy and how to ask for a change.
// Every field is optional.
type Contact struct {
	Owner       string `json:"owner,omitempty"`
	Email       string `json:"email,omitempty"`
	RequestURL  string `json:"request_url,omitempty"`
	RequestText string `json:"request_text,omitempty"`
}

// IsZero reports whether every field is empty.
func (c Contact) IsZero() bool { return c == Contact{} }

// Fields lists, in a fixed order, the names of the fields that are set. The audit
// log records this instead of the values.
func (c Contact) Fields() []string {
	var out []string
	for _, f := range []struct{ name, v string }{
		{"owner", c.Owner}, {"email", c.Email}, {"request_url", c.RequestURL}, {"request_text", c.RequestText},
	} {
		if f.v != "" {
			out = append(out, f.name)
		}
	}
	return out
}

// Ref is the one shape every surface serves: the policy that bound the person
// plus its contact, and nothing else about the profile.
type Ref struct {
	Source      string `json:"source"`
	Name        string `json:"name,omitempty"`
	Owner       string `json:"owner,omitempty"`
	Email       string `json:"email,omitempty"`
	RequestURL  string `json:"request_url,omitempty"`
	RequestText string `json:"request_text,omitempty"`
}

// UnsafeRune is what no free-text or header-bound field may carry: C0/C1
// controls (line breaks included), and the invisible Unicode that can make text
// read differently from what it is, namely format characters (bidi overrides,
// zero-width spaces) and the line/paragraph separators.
func UnsafeRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

// Validate refuses the first field that does not pass. A zero Contact is valid.
func Validate(c Contact) error {
	if !validText(c.Owner, ownerMax) {
		return errors.New("contact.owner: at most 200 characters, with no line break, control character or invisible formatting character")
	}
	if !validEmail(c.Email) {
		return errors.New("contact.email: must be one plain address such as name@example.com, at most 254 characters, with no display name or query")
	}
	if !validRequestURL(c.RequestURL) {
		return errors.New("contact.request_url: must be an https:// address with a real host name and no sign-in details, or one mailto: address with no query, in printable ASCII with no spaces, at most 2048 characters")
	}
	if !validText(c.RequestText, requestTextMax) {
		return errors.New("contact.request_text: at most 1,000 characters, with no line break, control character or invisible formatting character")
	}
	return nil
}

// Project is the read-side re-validation: it keeps each field of c that still
// passes and drops the rest, so one bad field never costs the good ones. It
// returns nil for a source that is neither profile nor deployment. name is kept
// for a profile only, and a nil contact yields a Ref that names the policy alone.
func Project(source, name string, c *Contact) *Ref {
	if source != SourceProfile && source != SourceDeployment {
		return nil
	}
	ref := &Ref{Source: source}
	if source == SourceProfile {
		ref.Name = name
	}
	if c == nil {
		return ref
	}
	if validText(c.Owner, ownerMax) {
		ref.Owner = c.Owner
	}
	if validEmail(c.Email) {
		ref.Email = c.Email
	}
	if validRequestURL(c.RequestURL) {
		ref.RequestURL = c.RequestURL
	}
	if validText(c.RequestText, requestTextMax) {
		ref.RequestText = c.RequestText
	}
	return ref
}

// RequestRoute is the one place the request route is chosen: request_url, else
// "mailto:" + email, else "". Both candidates are checked again here, so a Ref
// assembled by hand never yields a route the write rules would have refused.
func (r Ref) RequestRoute() string {
	if r.RequestURL != "" && validRequestURL(r.RequestURL) {
		return r.RequestURL
	}
	if r.Email != "" && validRequestURL(mailtoScheme+r.Email) {
		return mailtoScheme + r.Email
	}
	return ""
}

// HeaderValues returns the ASCII values of X-Wardyn-Policy and
// X-Wardyn-Policy-Request; request is "" when there is no route and the header
// should be left off. A profile name may be any printable Unicode, so it travels
// percent-encoded.
func (r Ref) HeaderValues() (policy, request string) {
	policy = SourceDeployment
	if r.Source == SourceProfile {
		policy = SourceProfile
		if r.Name != "" {
			policy += `; name="` + url.PathEscape(r.Name) + `"`
		}
	}
	return policy, r.RequestRoute()
}

func validText(s string, maxRunes int) bool {
	return utf8.RuneCountInString(s) <= maxRunes && !strings.ContainsFunc(s, UnsafeRune)
}

func validEmail(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > emailMax {
		return false
	}
	// ParseAddress alone accepts "x?cc=evil@example.com" unchanged; the charset
	// below is what keeps "mailto:" + s free of a query, fragment or second address.
	if a, err := mail.ParseAddress(s); err != nil || a.Address != s {
		return false
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || strings.ContainsFunc(local, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._%+-", r))
	}) {
		return false
	}
	return hostrules.ValidApprovedHost(strings.ToLower(domain))
}

func validRequestURL(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > requestURLMax || strings.ContainsFunc(s, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return false
	}
	if len(s) > len(mailtoScheme) && strings.EqualFold(s[:len(mailtoScheme)], mailtoScheme) {
		return validEmail(s[len(mailtoScheme):])
	}
	u, err := url.Parse(s)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil {
		return false
	}
	return hostrules.ValidApprovedHost(strings.ToLower(u.Hostname()))
}
