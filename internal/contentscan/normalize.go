// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
)

// variant is one decoded form of a span plus the decode chain that produced it
// (e.g. "base64", "url+base64"), recorded so a finding can name how the value
// was hidden.
type variant struct {
	chain string
	text  string
}

// normalizedVariants returns bounded decoded forms of text (Base64 / hex / URL
// percent-decoding), applied up to maxDepth times in combination. This is the
// "normalize-before-match" pattern: it lets the exact-match detector catch a
// known secret that an agent encoded.
//
// It is intentionally bounded — capped variant count, capped decoded size, and
// dedup — so it cannot become a decode-bomb DoS. It does NOT attempt to decode
// encoded substrings embedded in mixed prose (that overlaps the entropy detector
// of a later phase); the encoding-evasion residual therefore STANDS.
func normalizedVariants(text string, maxDepth int) []variant {
	const (
		maxVariants = 8
		maxSize     = 1 << 16 // 64 KiB per decoded variant
	)
	if maxDepth < 1 {
		return nil
	}
	out := make([]variant, 0, maxVariants)
	seen := map[string]struct{}{text: {}}

	type item struct {
		chain string
		text  string
		depth int
	}
	queue := []item{{"", text, 0}}

	decoders := []struct {
		name string
		fn   func(string) (string, bool)
	}{
		{"url", urlDecode},
		{"base64", base64Decode},
		{"hex", hexDecode},
	}

	for len(queue) > 0 && len(out) < maxVariants {
		cur := queue[0]
		queue = queue[1:]
		for _, dec := range decoders {
			d, ok := dec.fn(cur.text)
			if !ok || d == cur.text || d == "" || len(d) > maxSize {
				continue
			}
			if _, dup := seen[d]; dup {
				continue
			}
			seen[d] = struct{}{}
			chain := dec.name
			if cur.chain != "" {
				chain = cur.chain + "+" + dec.name
			}
			out = append(out, variant{chain: chain, text: d})
			if len(out) >= maxVariants {
				break
			}
			if cur.depth+1 < maxDepth {
				queue = append(queue, item{chain: chain, text: d, depth: cur.depth + 1})
			}
		}
	}
	return out
}

// urlDecode percent-decodes text if it contains a '%' escape and decodes cleanly.
// Uses PathUnescape (NOT QueryUnescape) so a literal '+' in a secret is preserved
// rather than turned into a space.
func urlDecode(text string) (string, bool) {
	if !strings.Contains(text, "%") {
		return "", false
	}
	d, err := url.PathUnescape(text)
	if err != nil {
		return "", false
	}
	return d, true
}

// base64Decode decodes text when the WHOLE span is a plausible base64 token
// (std/url alphabet, length >= 12). Decoded bytes must be valid UTF-8-ish text
// for matching; we return the raw decoded string and let the caller bytes.Index.
func base64Decode(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if len(t) < 12 || !looksBase64(t) {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(t); err == nil && len(b) > 0 {
			return string(b), true
		}
	}
	return "", false
}

func looksBase64(t string) bool {
	for _, r := range t {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '+' || r == '/' || r == '-' || r == '_' || r == '=':
		default:
			return false
		}
	}
	return true
}

// hexDecode decodes text when the WHOLE span is an even-length hex string of at
// least 16 nibbles.
func hexDecode(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if len(t) < 16 || len(t)%2 != 0 {
		return "", false
	}
	b, err := hex.DecodeString(t)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}
