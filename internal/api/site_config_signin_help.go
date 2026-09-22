// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signInHelpTextMax is owner decision Q457-8 (docs/design/admin-access-canon.md),
// counted in characters (runes), never bytes.
const signInHelpTextMax = 1000

var (
	errSignInHelpTextTooLong = errors.New("sign_in_help_text: longer than 1,000 characters — it renders under a refusal on the sign-in page")
	errSignInHelpTextControl = errors.New("sign_in_help_text: contains a line break or control character — it renders as one plain paragraph on the sign-in page")
	errSignInHelpURLScheme   = errors.New("sign_in_help_url: must be an http:// or https:// address — it is shown to people who have not signed in")
)

// validateSignInHelp is the one check both doors run: the write (via
// validateSiteConfig) and the anonymous read (signInHelpPublic). Not
// shellSafeSiteString: this text is never interpolated into a config file, and
// an apostrophe is ordinary prose here. The URL is, though — it reuses
// validSiteURL so it can never be looser than every other site-config URL.
func validateSignInHelp(text, link string) error {
	if utf8.RuneCountInString(text) > signInHelpTextMax {
		return errSignInHelpTextTooLong
	}
	for _, r := range text {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return errSignInHelpTextControl
		}
	}
	if link != "" && !validSiteURL(link) {
		return errSignInHelpURLScheme
	}
	return nil
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
