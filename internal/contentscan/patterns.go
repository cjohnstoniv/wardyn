// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ─── regex secret catalog ─────────────────────────────────────────────────────

// secretRule matches a well-known secret FORMAT. The catalog is intentionally
// limited to HIGH-PRECISION, prefixed patterns (AKIA…, ghp_…, AIza…, etc.) —
// broad "any 40-char base64" rules would false-positive on every hash/asset.
type secretRule struct {
	name     string
	re       *regexp.Regexp
	severity Severity
}

var secretRules = []secretRule{
	{"aws-access-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA|AROA|AIDA)[0-9A-Z]{16}\b`), SevHigh},
	{"github-pat", regexp.MustCompile(`\bgh[pousr]_[0-9A-Za-z]{36,255}\b`), SevHigh},
	{"github-fine-grained-pat", regexp.MustCompile(`\bgithub_pat_[0-9A-Za-z_]{22,255}\b`), SevHigh},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`), SevHigh},
	{"slack-webhook", regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9+/]{40,}`), SevHigh},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), SevHigh},
	{"stripe-key", regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{24,}\b`), SevHigh},
	{"private-key-pem", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`), SevCritical},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\b`), SevMedium},
}

// regexSecretDetector flags well-known secret formats (DetectSecretPatterns).
type regexSecretDetector struct{}

func (regexSecretDetector) Scan(s Span, dst *[]Finding) {
	path := sanitizePath(s.FieldPath)
	for _, rule := range secretRules {
		if loc := rule.re.FindStringIndex(s.Text); loc != nil {
			*dst = append(*dst, Finding{
				Detector:  "regex:" + rule.name,
				Category:  CategorySecret,
				FieldPath: path,
				Offset:    loc[0],
				Length:    loc[1] - loc[0],
				Severity:  rule.severity,
				Sample:    maskedPlaceholder,
			})
		}
	}
}

// ─── Shannon-entropy detector ─────────────────────────────────────────────────

const (
	// entropyMinLen / entropyThreshold gate the high-FP entropy detector: only
	// long base64-ish tokens with high per-char Shannon entropy are flagged.
	// Pure hex is skipped (git SHAs/hashes are everywhere and would storm).
	entropyMinLen    = 24
	entropyThreshold = 4.2 // bits/char (base64 max ~6; English prose ~3-4)
)

// entropyDetector flags long, high-entropy tokens (DetectEntropy). Best-effort,
// off by default; medium severity so block_min_severity can exclude it.
type entropyDetector struct{}

func (entropyDetector) Scan(s Span, dst *[]Finding) {
	path := sanitizePath(s.FieldPath)
	for _, tok := range entropyTokens(s.Text) {
		if len(tok.text) < entropyMinLen || isAllHex(tok.text) {
			continue
		}
		if shannonEntropy(tok.text) >= entropyThreshold {
			*dst = append(*dst, Finding{
				Detector:  "entropy",
				Category:  CategorySecret,
				FieldPath: path,
				Offset:    tok.offset,
				Length:    len(tok.text),
				Severity:  SevMedium,
				Sample:    maskedPlaceholder,
			})
		}
	}
}

type token struct {
	text   string
	offset int
}

// entropyTokens splits text into candidate secret tokens on characters that do
// not appear in base64/token alphabets.
func entropyTokens(text string) []token {
	var toks []token
	start := -1
	for i := 0; i <= len(text); i++ {
		if i < len(text) && isTokenChar(text[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			toks = append(toks, token{text: text[start:i], offset: start})
			start = -1
		}
	}
	return toks
}

func isTokenChar(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '+' || b == '/' || b == '-' || b == '_' || b == '=':
		return true
	}
	return false
}

func isAllHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// shannonEntropy returns the per-character Shannon entropy of s in bits.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]int
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// ─── shared path sanitizer ────────────────────────────────────────────────────

// maxFieldPathBytes bounds one Finding's FieldPath.
//
// TRUST BOUNDARY: the per-request findings cap bounds the NUMBER of
// findings, not their SIZE, and FieldPath is built from AGENT-CONTROLLED
// JSON keys — the scan budget only counts span TEXT (values), so huge KEYS
// burn neither limit (measured: a 324KB body of long-key leaves produced a
// 46 MB findings JSON, 44x the control plane's body limit, silently losing
// the decision). Bounding the path keeps the decision log bounded in BYTES,
// not only rows. 256 is far beyond any real field path yet small enough
// that a full cap of findings can't approach the control plane's limit.
const maxFieldPathBytes = 256

// sanitizePath masks any well-known secret FORMAT in a field path so a
// Finding stays content-free by construction, then TRUNCATES it to
// maxFieldPathBytes so one finding can't be arbitrarily large. Masking runs
// BEFORE truncation so a secret-shaped key is masked wherever it sits. The
// head+tail form keeps both ends an operator navigates by and states the
// original length. Idempotent.
func sanitizePath(path string) string {
	if path == "" {
		return path
	}
	out := path
	for _, rule := range secretRules {
		out = rule.re.ReplaceAllString(out, maskedPlaceholder)
	}
	return truncateFieldPath(out)
}

// truncateFieldPath cuts a path to maxFieldPathBytes, on rune boundaries so the
// audit stream never carries a split UTF-8 sequence.
func truncateFieldPath(path string) string {
	if len(path) <= maxFieldPathBytes {
		return path
	}
	const head, tail = 160, 48
	return strings.ToValidUTF8(path[:head], "") +
		"…[+" + strconv.Itoa(len(path)-head-tail) + "B]…" +
		strings.ToValidUTF8(path[len(path)-tail:], "")
}
