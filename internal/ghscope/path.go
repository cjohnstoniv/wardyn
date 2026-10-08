// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

// How a request path is read. Every rule here refuses rather than resolves:
// a spelling GitHub might route differently from the way it reads is not
// classified at all.

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// segment is one path segment: its decoded, lowercased text, and whether the
// request spelled any of it with a percent-escape.
type segment struct {
	text    string
	escaped bool
}

// splitPath decodes raw into its segments. A single trailing slash is
// tolerated; an empty segment anywhere else (a doubled slash) is refused,
// because whether GitHub collapses it is not knowable from here. The root
// path yields no segments.
func splitPath(raw string) ([]segment, error) {
	if !strings.HasPrefix(raw, "/") {
		return nil, fmt.Errorf("ghscope: path %q does not start with a slash", raw)
	}
	if strings.ContainsAny(raw, "?#") {
		return nil, fmt.Errorf("ghscope: path %q carries a query or fragment — pass the path alone", raw)
	}
	rest := raw[1:]
	if rest == "" {
		return nil, nil
	}
	var out []segment
	for _, part := range strings.Split(strings.TrimSuffix(rest, "/"), "/") {
		if part == "" {
			return nil, fmt.Errorf("ghscope: path %q has an empty segment", raw)
		}
		text, err := decodeSegment(part)
		if err != nil {
			return nil, err
		}
		out = append(out, segment{text: strings.ToLower(text), escaped: strings.Contains(part, "%")})
	}
	return out, nil
}

// decodeSegment percent-decodes one raw segment, refusing every spelling that
// could be routed as something other than what it reads as: a malformed
// escape, an escape that decodes to a separator, a backslash, a dot segment,
// a control character, invalid UTF-8, and a second layer of encoding around
// any of those. Decoded by hand rather than url.PathUnescape, which leaves an
// encoded "/" as a literal slash needing re-detection.
func decodeSegment(raw string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' {
			b.WriteByte(raw[i])
			continue
		}
		if i+2 >= len(raw) {
			return "", fmt.Errorf("ghscope: path segment %q is not decodable", raw)
		}
		hi, lo := unhex(raw[i+1]), unhex(raw[i+2])
		if hi < 0 || lo < 0 {
			return "", fmt.Errorf("ghscope: path segment %q is not decodable", raw)
		}
		b.WriteByte(byte(hi<<4 | lo))
		i += 2
	}
	seg := b.String()
	if !utf8.ValidString(seg) {
		return "", fmt.Errorf("ghscope: path segment %q decodes to invalid UTF-8", raw)
	}
	if hazard := segmentHazard(seg); hazard != "" {
		return "", fmt.Errorf("ghscope: path segment %q %s", raw, hazard)
	}
	if hidesStructure(seg) {
		return "", fmt.Errorf("ghscope: path segment %q is encoded more than once around structure", raw)
	}
	return seg, nil
}

// segmentHazard names why a decoded segment would not be routed the way it
// reads, or "" when it would.
func segmentHazard(seg string) string {
	switch {
	case seg == "." || seg == "..":
		return "is a dot segment"
	case strings.ContainsAny(seg, `/\`):
		return "holds a separator"
	case strings.ContainsFunc(seg, unicode.IsControl):
		return "holds a control character"
	}
	return ""
}

// maxDecodeDepth bounds hidesStructure. Nothing legitimate on this API is
// percent-encoded more than once, so a segment still changing after this many
// further rounds is refused rather than followed.
const maxDecodeDepth = 4

// hidesStructure reports whether decoding seg AGAIN yields a segmentHazard at
// any depth, as a layer that decodes once more would see: "%252F" decodes once
// to a literal "%2F", harmless to a single-decode server and a separator to a
// double-decoding one. The re-decode is lenient (malformed escapes kept as
// text), since a lenient decoder is the most dangerous one a request could meet.
func hidesStructure(seg string) bool {
	for range maxDecodeDepth {
		next := percentDecodeLenient(seg)
		if next == seg {
			return false
		}
		if segmentHazard(next) != "" {
			return true
		}
		seg = next
	}
	return true
}

// percentDecodeLenient decodes every valid %XY in s once and keeps every
// malformed one as literal text.
func percentDecodeLenient(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, lo := unhex(s[i+1]), unhex(s[i+2]); hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unhex is one hex digit's value, or -1.
func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// ownerName is s as an account or organisation name — letters, digits, "-"
// and "_" (the last only in managed-user names) — lowercased, since GitHub
// resolves names case-insensitively. ok=false for anything else: no client
// needs to escape a name, so one that arrives escaped is refused, not decoded.
func ownerName(s segment) (string, bool) {
	if s.escaped || s.text == "" || !isNameText(s.text, "-_") {
		return "", false
	}
	return s.text, true
}

// repoName is s as a repository name — letters, digits, "-", "_" and "." —
// lowercased, with ONE trailing ".git" removed: GitHub serves a repository
// under both spellings and no repository's own name ends in it, so a name
// still ending in ".git" afterwards is refused.
func repoName(s segment) (string, bool) {
	name := strings.TrimSuffix(s.text, ".git")
	if s.escaped || name == "" || name == "." || name == ".." || strings.HasSuffix(name, ".git") || !isNameText(name, "-_.") {
		return "", false
	}
	return name, true
}

// isNameText reports whether s is ASCII letters and digits plus the bytes of extra.
func isNameText(s, extra string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte(extra, c) >= 0 {
			continue
		}
		return false
	}
	return true
}

// RepoKey is "owner/name" in the one spelling this package compares
// repositories in: lowercased, with a trailing ".git" removed. ok=false for
// anything that is not exactly an owner and a repository name.
func RepoKey(s string) (string, bool) {
	o, n, found := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "/")
	if !found {
		return "", false
	}
	owner, ok := ownerName(segment{text: o})
	if !ok {
		return "", false
	}
	name, ok := repoName(segment{text: n})
	if !ok {
		return "", false
	}
	return owner + "/" + name, true
}
