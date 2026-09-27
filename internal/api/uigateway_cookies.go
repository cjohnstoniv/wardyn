// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"slices"
	"strings"
)

// UICookiePolicy is WARDYN_UI_SANDBOX_STRIP_COOKIES parsed: which inbound
// cookies, beyond the wardyn_* namespace that is stripped unconditionally, the
// relay forwards to a sandbox app. The zero value is the default and forwards
// every other cookie.
//
// It exists for a relay host under a registrable domain shared with other
// applications: a browser attaches a sibling host's Domain= cookies to the
// relay's requests, and the Cookie header carries only name=value, so a
// cookie's origin cannot be recovered — the operator names the cookies instead.
type UICookiePolicy struct {
	// Allow true forwards ONLY the cookies Names matches; false strips them.
	Allow bool
	// Names are exact cookie names, or prefixes when they end in "*".
	// Matching is case-sensitive, as browsers treat cookie names.
	Names []string
}

// ParseUICookiePolicy parses "allow:<names>" or "deny:<names>", a
// comma-separated list of cookie names or "prefix*" entries. Empty is the
// default policy. Boot refuses anything else (cmd/wardynd,
// validateUISandboxConfig), including an allow entry inside the wardyn_*
// namespace: those cookies are never forwarded, and a policy that reads as
// if it forwarded one is a misconfiguration to name, not to ignore.
func ParseUICookiePolicy(raw string) (UICookiePolicy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return UICookiePolicy{}, nil
	}
	mode, list, _ := strings.Cut(raw, ":")
	var p UICookiePolicy
	switch mode {
	case "allow":
		p.Allow = true
	case "deny":
	default:
		return UICookiePolicy{}, fmt.Errorf("WARDYN_UI_SANDBOX_STRIP_COOKIES %q: want \"allow:<names>\" or \"deny:<names>\"", raw)
	}
	for _, n := range strings.Split(list, ",") {
		n = strings.TrimSpace(n)
		body := strings.TrimSuffix(n, "*")
		if n == "" || (body == "" && n != "*") || strings.ContainsFunc(body, uiNotCookieNameRune) {
			return UICookiePolicy{}, fmt.Errorf("WARDYN_UI_SANDBOX_STRIP_COOKIES %q: entry %q is not a cookie name or \"prefix*\"", raw, n)
		}
		if p.Allow && uiIsWardynCookie(body) {
			return UICookiePolicy{}, fmt.Errorf("WARDYN_UI_SANDBOX_STRIP_COOKIES %q: %q names a Wardyn cookie, which is never forwarded to a sandbox app", raw, n)
		}
		p.Names = append(p.Names, n)
	}
	return p, nil
}

// uiNotCookieNameRune is what a cookie name, as a browser splits the Cookie
// header, cannot contain — and '*', which in a policy entry means "prefix" only
// at the end.
func uiNotCookieNameRune(r rune) bool {
	return r <= ' ' || r == 0x7f || r == ';' || r == '=' || r == ',' || r == '*'
}

// forwards reports whether an inbound cookie named name reaches the app.
func (p UICookiePolicy) forwards(name string) bool {
	if uiIsWardynCookie(name) {
		return false
	}
	matched := slices.ContainsFunc(p.Names, func(n string) bool {
		if prefix, ok := strings.CutSuffix(n, "*"); ok {
			return strings.HasPrefix(name, prefix)
		}
		return n == name
	})
	return matched == p.Allow
}

// uiSetCookieAllowed decides one upstream Set-Cookie. It splits the way a
// browser does (RFC 6265 §5.2: on every ';', quoted or not), so an attribute
// hidden inside a quoted value is seen exactly where the browser would see it,
// and a "Domain=" that is only value text is not.
//
// Dropped:
//   - a wardyn_* name (cookie tossing onto the relay's own credential);
//   - a pair with no '=' or an empty name. RFC 6265bis stores it as a NAMELESS
//     cookie whose value is sent back verbatim, so "=wardyn_ui_sess=x" would
//     come back as a wardyn_ui_sess cookie the name check above never saw;
//   - any Domain attribute, whatever its case or value. An app confined to its
//     own origin never needs one, and with one a sandbox could set a cookie
//     every sibling host under the parent domain receives.
func uiSetCookieAllowed(sc string) bool {
	pair, attrs, _ := strings.Cut(sc, ";")
	name, _, ok := strings.Cut(pair, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" || uiIsWardynCookie(name) {
		return false
	}
	for _, a := range strings.Split(attrs, ";") {
		attr, _, _ := strings.Cut(a, "=")
		if strings.EqualFold(strings.TrimSpace(attr), "domain") {
			return false
		}
	}
	return true
}
