// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signInHelpTextMax is owner decision Q457-8 (docs/design/admin-access-canon.md),
// counted in characters (runes), never bytes.
const signInHelpTextMax = 1000

// signInHelpURLMax bounds the link the way shellSafeSiteString bounds every
// other site-config URL.
const signInHelpURLMax = 2048

var (
	errSignInHelpTextTooLong  = errors.New("sign_in_help_text: longer than 1,000 characters — it renders under a refusal on the sign-in page")
	errSignInHelpTextControl  = errors.New("sign_in_help_text: contains a line break, control character or invisible formatting character — it renders as one plain paragraph on the sign-in page")
	errSignInHelpURLScheme    = errors.New("sign_in_help_url: must be an https:// address — it is shown to people who have not signed in")
	errSignInHelpURLMalformed = errors.New("sign_in_help_url: must be a plain web address with a real host name — no spaces, sign-in details or hidden characters — it is shown to people who have not signed in")
)

// unsafeHelpRune is what neither field may carry: C0/C1 controls (line breaks
// included), and the invisible Unicode that can make text read differently
// from what it is — format characters (bidi overrides, zero-width spaces) and
// the line/paragraph separators.
func unsafeHelpRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

// validateSignInHelp is the one check both doors run: the write (via
// validateSiteConfig) and the anonymous read (signInHelpPublic). Not
// shellSafeSiteString, for either field: neither is ever interpolated into a
// config file, an apostrophe is ordinary prose, and a helpdesk link's query
// string needs '&' (which that gate bans).
func validateSignInHelp(text, link string) error {
	if utf8.RuneCountInString(text) > signInHelpTextMax {
		return errSignInHelpTextTooLong
	}
	if strings.ContainsFunc(text, unsafeHelpRune) {
		return errSignInHelpTextControl
	}
	if link == "" {
		return nil
	}
	if len(link) > signInHelpURLMax || strings.ContainsFunc(link, func(r rune) bool { return unsafeHelpRune(r) || unicode.IsSpace(r) }) {
		return errSignInHelpURLMalformed
	}
	u, err := url.Parse(link)
	if err != nil {
		return errSignInHelpURLMalformed
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return errSignInHelpURLScheme
	}
	if u.User != nil || !hostrules.ValidApprovedHost(strings.ToLower(u.Hostname())) {
		return errSignInHelpURLMalformed
	}
	return nil
}

// signInHelpURLHTTPS is the write's https-only rule (#489), checked against the
// stored link because validateSignInHelp's shape check still admits http://:
// a link stored before the rule keeps publishing, and a save that echoes it back
// unchanged (a console save of the whole document, an MDM re-apply) is not
// refused for a field it did not change. setup/status warns about that link
// (signInHelpHTTPCheck) until an admin replaces it.
func signInHelpURLHTTPS(link, stored string) error {
	if link != stored && signInHelpIsHTTP(link) {
		return errSignInHelpURLScheme
	}
	return nil
}

// signInHelpIsHTTP reports whether link is an http:// address.
func signInHelpIsHTTP(link string) bool {
	u, err := url.Parse(link)
	return err == nil && strings.EqualFold(u.Scheme, "http")
}

// signInHelpPublic is what /healthz may publish: each stored value re-checked
// on the way out and dropped — falling back to Wardyn's own sentence alone —
// when it no longer passes (a document written before the check existed, or
// by a hand-edited store row).
func signInHelpPublic(sc types.SiteConfig) (text, link string) {
	if validateSignInHelp(sc.SignInHelpText, "") == nil {
		text = sc.SignInHelpText
	}
	if validateSignInHelp("", sc.SignInHelpURL) == nil {
		link = sc.SignInHelpURL
	}
	return text, link
}
