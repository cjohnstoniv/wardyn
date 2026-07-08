// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"math"
	"regexp"
)

// ─── regex secret catalog ─────────────────────────────────────────────────────

// secretRule matches a well-known secret FORMAT. The catalog is intentionally
// limited to HIGH-PRECISION, prefixed patterns (AKIA…, ghp_…, AIza…, etc.) — we
// deliberately omit broad "any 40-char base64" rules (the generic AWS secret-key
// shape) that would false-positive on every hash/asset in a codebase.
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

func (regexSecretDetector) Name() string { return "regex-secret" }

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
	// long base64-ish tokens with high per-char Shannon entropy are flagged. Pure
	// hex is skipped (git SHAs / hashes are everywhere and would storm).
	entropyMinLen    = 24
	entropyThreshold = 4.2 // bits/char (base64 max ~6; English prose ~3-4)
)

// entropyDetector flags long, high-entropy tokens (DetectEntropy). Best-effort,
// off by default; medium severity so block_min_severity can exclude it.
type entropyDetector struct{}

func (entropyDetector) Name() string { return "entropy" }

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

// sanitizePath masks any well-known secret FORMAT appearing in a field path
// (agent-controlled JSON object keys can contain one) so a Finding stays
// content-free by construction. The known-secret detector additionally masks
// operator-declared corpus values from the path (see knownSecretDetector.safePath).
func sanitizePath(path string) string {
	if path == "" {
		return path
	}
	out := path
	for _, rule := range secretRules {
		out = rule.re.ReplaceAllString(out, maskedPlaceholder)
	}
	return out
}
