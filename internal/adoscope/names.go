// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

// Azure DevOps project and repository names in URLs — the ONE rule every site
// that builds, reads or compares one goes through.
//
// Microsoft's naming rules (learn.microsoft.com/azure/devops/organizations/
// settings/naming-restrictions) permit a space and most printable punctuation
// in both: a project forbids \ / : * ? " ' < > ; # $ { } , + = [ ] |, a
// repository the same set minus the apostrophe, and neither may hold a control
// character, start with "_", or start or end with ".". An organisation is
// letters, digits and "-" only, so it never needs any of this.
//
// A name therefore reaches Wardyn in more than one spelling — typed with its
// space, pasted from Azure DevOps as %20, or escaped wholesale by a client
// library — and every spelling decodes, by the same strict rule the REST gate
// already applied to its paths, to one name. "+" is a literal "+" here, never
// a space: this is PATH grammar, which is how Azure DevOps itself routes it.
// The one place a name travels in a query string, where "+" does mean a space,
// is spelled and read by net/url on both ends.

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// UnescapeName decodes one URL path segment into the name Azure DevOps routes
// it as, refusing every spelling the service would route differently from how
// it reads.
//
// It decodes by hand rather than through url.PathUnescape for one reason:
// PathUnescape leaves an encoded "/" as a literal slash in its output, which
// then has to be re-detected, and the two-step version of that rule is exactly
// where a laundering bug hides. Here the byte is refused where it is decoded.
func UnescapeName(raw string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' {
			b.WriteByte(raw[i])
			continue
		}
		if i+2 >= len(raw) {
			return "", fmt.Errorf("adoscope: path segment %q is not decodable", raw)
		}
		hi, lo := unhex(raw[i+1]), unhex(raw[i+2])
		if hi < 0 || lo < 0 {
			return "", fmt.Errorf("adoscope: path segment %q is not decodable", raw)
		}
		c := byte(hi<<4 | lo)
		if isPathSeparator(rune(c)) {
			return "", fmt.Errorf("adoscope: path segment %q decodes to a second segment", raw)
		}
		b.WriteByte(c)
		i += 2
	}
	seg := b.String()
	if hazard := segmentHazard(seg); hazard != "" {
		return "", fmt.Errorf("adoscope: path segment %q %s", raw, hazard)
	}
	if hidesStructure(seg) {
		return "", fmt.Errorf("adoscope: path segment %q is encoded more than once around structure — a layer that decodes again would route it differently", raw)
	}
	return seg, nil
}

// EscapeName is the ONE spelling of a name as a URL path segment:
// UnescapeName(EscapeName(n)) == n for every name the service permits.
//
// It escapes as LITTLE as it can — whitespace, control characters, "%", and
// the four characters that would end the segment or the path — and leaves every
// other character literal. That is deliberate: a repository URL with "(", "&",
// "'" or "é" in it was already accepted verbatim before this rule existed, so
// escaping those too would give a stored row a second spelling of itself. What
// it does escape is exactly what a stored field must not carry (a space breaks
// the tab-framed WARDYN_REPOS and every whitespace guard) and what a URL cannot
// hold unescaped.
func EscapeName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		if r == utf8.RuneError || unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`%#?/\`, r) {
			for _, c := range []byte(name[i : i+size]) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		} else {
			b.WriteString(name[i : i+size])
		}
		i += size
	}
	return b.String()
}

// NameKey is the comparison key of a path segment naming a project or
// repository: its decoded name, case-folded because Azure DevOps resolves
// names case-insensitively in its URLs. "" when the segment does not decode.
func NameKey(raw string) string {
	name, err := UnescapeName(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(name)
}

// CanonicalPath rewrites every non-empty segment of a URL path to
// EscapeName(UnescapeName(segment)), so each spelling of the same names is one
// string. ok=false when any segment does not decode — the caller keeps the
// original, and whatever refuses a percent-escape today still refuses it.
func CanonicalPath(path string) (string, bool) {
	segs := strings.Split(path, "/")
	for i, raw := range segs {
		if raw == "" {
			continue
		}
		name, err := UnescapeName(raw)
		if err != nil {
			return "", false
		}
		segs[i] = EscapeName(name)
	}
	return strings.Join(segs, "/"), true
}

// CanonicalRepoURL is CanonicalURL for an Azure DevOps repository address: one
// whose host is an Azure DevOps service host (dev.azure.com and its
// subdomains, or <org>.visualstudio.com) or one of serverHosts — the Azure
// DevOps Server hosts a caller knows from its own configuration, lowercased.
// ok=false for anything else — another forge's URL above all — which callers
// leave untouched. There is deliberately no "the path has a _git segment"
// guess: a name rule applied to a forge that is not Azure DevOps would admit
// escapes that forge never needs.
func CanonicalRepoURL(raw string, serverHosts []string) (string, bool) {
	_, _, host, ok := splitRepoAddress(raw)
	if !ok || !(azureDevOpsHost(host) || slices.Contains(serverHosts, host)) {
		return "", false
	}
	return CanonicalURL(raw)
}

// CanonicalURL is CanonicalPath applied to the path of an https:// or ssh://
// URL, or of scp-form [user@]host:path. The scheme, userinfo and host are left
// exactly as written, and none may hold a "%": the name rule covers the path
// alone, and an escape anywhere else is one no caller reads the way a server
// would. ok=false for that, a query or fragment, a URL with no path, or a
// segment that does not decode.
func CanonicalURL(raw string) (string, bool) {
	head, path, _, ok := splitRepoAddress(raw)
	if !ok || strings.Contains(head, "%") {
		return "", false
	}
	p, ok := CanonicalPath(path)
	if !ok {
		return "", false
	}
	return head + p, true
}

// splitRepoAddress splits raw into everything up to its path (kept verbatim),
// the path, and the lowercased host. String surgery, not url.Parse: a parse
// and re-serialise would re-escape the path by Go's rules, which is a second
// canonical form.
func splitRepoAddress(raw string) (head, path, host string, ok bool) {
	if i := strings.Index(raw, "://"); i >= 0 {
		if i == 0 || strings.ContainsFunc(raw[:i], func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') }) {
			return "", "", "", false
		}
		j := strings.IndexByte(raw[i+3:], '/')
		if j < 0 {
			return "", "", "", false
		}
		head, path = raw[:i+3+j], raw[i+3+j:]
		host = head[i+3:]
	} else {
		i := strings.IndexByte(raw, ':')
		if i <= 0 || strings.ContainsAny(raw[:i], "/ ") {
			return "", "", "", false
		}
		head, path, host = raw[:i+1], raw[i+1:], raw[:i]
	}
	if strings.ContainsAny(path, "?#") {
		return "", "", "", false
	}
	host = host[strings.LastIndexByte(host, '@')+1:]
	host, _, _ = strings.Cut(host, ":")
	return head, path, strings.ToLower(host), true
}

// segmentHazard names why a decoded segment would not be routed the way it
// reads, or "" when it would.
//
// Every case is a spelling the SERVICE normalises before it routes, so the
// text here and the route there disagree:
//   - "." and ".." are resolved outright;
//   - leading or trailing whitespace, and a trailing dot, are trimmed first —
//     Windows path canonicalisation — so ".. " is "..", "..." is "..", and
//     "hooks." is the denied "hooks" area. Refusing the edge characters is the
//     fail-closed reading, and no name on this API legitimately ends in one;
//   - a control character, which no Azure DevOps name may hold;
//   - a separator still inside the segment. While the split in decodeSegments
//     is correct nothing reaches this case: the split removed every raw
//     separator and UnescapeName refuses every decoded one. It is a SECOND,
//     INDEPENDENT LINE — with the split reverted to "/" alone and this kept,
//     every separator evasion is still refused, and the only cost is that a
//     legitimate backslash-delimited path is refused too.
func segmentHazard(seg string) string {
	switch {
	case seg == "." || seg == "..":
		return "is a dot segment — the service resolves it to a different route"
	case strings.TrimFunc(seg, unicode.IsSpace) != seg:
		return "has leading or trailing whitespace — the service trims it before routing"
	case strings.HasSuffix(seg, "."):
		return "ends in a dot — the service trims it before routing"
	case strings.ContainsFunc(seg, unicode.IsControl):
		return "holds a control character — no Azure DevOps name does"
	case strings.ContainsFunc(seg, isPathSeparator):
		return "still holds a separator"
	}
	return ""
}

// maxDecodeDepth bounds hidesStructure. Nothing legitimate on this API is
// percent-encoded more than once, so a segment still changing after this many
// further rounds is refused rather than followed.
const maxDecodeDepth = 4

// hidesStructure reports whether decoding seg AGAIN — as any layer between here
// and the service that decodes once more would — yields any segmentHazard at
// any depth.
//
// It exists because "%252F" decodes once to the literal text "%2F": harmless
// to a service that decodes once, and a separator to anything that decodes
// twice. Whether such a layer sits in the path is not knowable from here, so
// the answer that cannot be wrong is to refuse the segment.
//
// The re-decode is LENIENT on purpose — valid escapes decoded, malformed ones
// kept as literal text — because that is the most dangerous decoder a request
// could meet: a strict one refuses "%2F%zz" outright, a lenient one decodes it
// to "/%zz". Assuming the lenient one is the fail-closed reading, and it costs
// no legitimate name anything: "100%" and "50%off" reach a fixpoint unchanged.
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
// malformed one as literal text. See hidesStructure for why lenient.
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
