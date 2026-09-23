// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A provider row's base URLs: the one stored form they are normalised to, and
// the write-boundary shape each is held to. Split from workspace_providers.go
// by seam (file-size gate).

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// normalizeProviderBaseURL puts a base URL in the ONE form the match rule
// compares against: lowercase scheme and authority, the path rebuilt from its
// non-empty segments, and — on github.com only — the path folded to lowercase.
//
// It is STRING SURGERY rather than a url.Parse/String round trip on purpose:
// re-serializing percent-ENCODES the characters shellSafeSiteString exists to
// refuse, so a base URL carrying a backtick normalized into one that passed the
// injection gate. Normalization must never launder a string past the validator
// that runs after it — and the path is therefore never DECODED here either; a
// percent-escape in it is refused by validateProviderBaseURLs instead. (An
// azure_devops row's path reaches here already in adoscope's canonical
// spelling — normalizeWorkspaceProviders — which re-escapes only whitespace,
// "%" and structure, so every character the validator must see is literal.)
//
// Rebuilding the path from segments is what keeps "stored" and "canonical" the
// same string: "https://github.com//acme" counted as one segment at the write
// boundary and was then matched RAW, so it claimed github.com kind-wide while
// admitting nothing anyone would ever clone.
//
// The github.com fold is the one case-INSENSITIVE forge the tree knows about:
// GitHub treats /Acme and /acme as one org, so a base URL written /Acme that
// refused every /acme clone URL would read as a working policy and silently
// stop every repo. Azure DevOps project paths ARE case-sensitive and are left
// alone, as is any self-hosted host (whose rule nothing here can know).
func normalizeProviderBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	i := strings.Index(s, "://")
	if i < 0 {
		return strings.TrimSuffix(s, "/")
	}
	end := len(s)
	if j := strings.IndexByte(s[i+3:], '/'); j >= 0 {
		end = i + 3 + j
	}
	authority := strings.ToLower(s[:end])
	segs := baseURLPathSegments(s[end:])
	if len(segs) == 0 {
		return authority
	}
	path := "/" + strings.Join(segs, "/")
	if foldsPathCase(hostrules.HostOf(authority)) {
		path = strings.ToLower(path)
	}
	return authority + path
}

// foldsPathCase reports whether a host's repository paths are case-INSENSITIVE,
// which decides whether the match rule (and normalization) folds the path.
// github.com is the only one the tree can claim this about; everything else —
// Azure DevOps, GHES, ADO Server, any self-hosted forge — stays case-sensitive,
// the safe direction (refuse a case the admin did not write, never admit one
// they did not).
func foldsPathCase(host string) bool {
	return canonicalProviderHost(host) == "github.com"
}

// validateProviderBaseURLs holds every base URL of one row to the shape a
// persisted site-config URL is already held to (validSiteURL ->
// shellSafeSiteString + hostrules.ValidApprovedHost), plus the four narrowings a
// provider address needs beyond that gate — https only, no userinfo, no query or
// fragment, a bounded path — and then the kind's own host rule.
func validateProviderBaseURLs(i int, row types.GitProvider) error {
	for j, raw := range row.BaseURLs {
		u, err := url.Parse(raw)
		switch {
		case err != nil, !validSiteURL(raw),
			// https ONLY. validSiteURL accepts both schemes for its other
			// callers; a base URL is matched against a clone URL scheme-for-
			// scheme, and an http:// provider address would admit a cleartext
			// clone of the org's own source.
			u.Scheme != "https",
			// No userinfo — verbatim the upstream_proxy_url rule: a credential
			// in a URL is a credential in every log and config dump of it.
			u.User != nil,
			// No query or fragment: neither participates in the match, so one
			// here is a base URL the admin thinks is narrower than it is.
			u.RawQuery != "", u.ForceQuery, u.Fragment != "",
			// No port. HostOf (and so the egress allowlist) discards it, so a
			// port here scopes nothing while reading as though it did.
			u.Port() != "",
			// No percent-encoding in the path. It is the same laundering
			// normalizeProviderBaseURL refuses to do: "%60id%60" DECODES to a
			// backtick that shellSafeSiteString would have refused on sight, and
			// "acme%2Fevil" decodes to a second path segment the segment count
			// above cannot see in the stored string. One escape hatch closed at
			// the boundary beats two readers disagreeing about the same bytes —
			// see baseURLPathPlain for the one spelling an azure_devops row may
			// carry.
			!baseURLPathPlain(row.Kind, raw, u):
			return fmt.Errorf(providers400BaseURL, j)
		}
		segs := baseURLPathSegments(u.Path)
		if len(segs) > maxProviderBaseURLPathSegments {
			return fmt.Errorf(providers400BaseURL, j)
		}
		if err := validateProviderHostForKind(j, row.Kind, strings.ToLower(u.Hostname()), len(segs)); err != nil {
			return err
		}
	}
	return nil
}

// baseURLPathPlain reports whether a base URL's path carries no escape the
// match rule would read differently from the stored string. For an
// azure_devops row, whose project names may hold a space, an escaped path is
// plain when it is ALREADY adoscope's canonical spelling (normalizeWorkspace-
// Providers wrote it): that rule refused every escape decoding to a separator,
// a dot segment or a control character, and it leaves every other character —
// the backtick included — literal, where shellSafeSiteString still sees it.
func baseURLPathPlain(kind types.GitProviderKind, raw string, u *url.URL) bool {
	if u.Path == u.EscapedPath() {
		return true
	}
	if kind != types.GitProviderAzureDevOps {
		return false
	}
	c, ok := adoscope.CanonicalURL(raw)
	return ok && c == raw
}

// baseURLPathSegments splits a base URL path into its non-empty segments.
func baseURLPathSegments(path string) []string {
	var out []string
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// validateProviderHostForKind is the kind x host table. The two WELL-KNOWN
// hosts belong to exactly one kind each, so a row claiming the other kind's host
// is refused — the claim rule admission uses is kind-wide on those hosts, and a
// mislabelled row would claim a host whose repos it can never admit. Any OTHER
// valid host is accepted for either kind: it is a GHES, an ADO Server or a
// GitLab host indistinguishably (nothing on the tree can tell them apart by
// name), so the admin's own label is the only fact available.
func validateProviderHostForKind(j int, kind types.GitProviderKind, host string, pathSegments int) error {
	// adoEgressDomains is the tree's own ADO-host classifier: dev.azure.com or
	// any *.visualstudio.com.
	switch kind {
	case types.GitProviderGitHub:
		if adoEgressDomains(host) != nil {
			return fmt.Errorf(providers400HostKind, j, host, string(kind))
		}
		// github.com takes an optional single /<org>; a deeper path there names
		// a repository, not an org.
		if host == "github.com" && pathSegments > 1 {
			return fmt.Errorf(providers400HostKind, j, host, string(kind))
		}
	case types.GitProviderAzureDevOps:
		if host == "github.com" {
			return fmt.Errorf(providers400HostKind, j, host, string(kind))
		}
		// dev.azure.com is shared by every org on the planet, so the
		// organization segment is REQUIRED there — "https://dev.azure.com" as a
		// base URL is not a policy.
		if host == "dev.azure.com" && pathSegments != 1 {
			return fmt.Errorf(providers400HostKind, j, host, string(kind))
		}
	}
	return nil
}
