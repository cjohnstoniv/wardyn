// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import "regexp"

// piiDetector flags common PII formats (detect_pii). It is BEST-EFFORT and
// OFF-BY-DEFAULT: PII detection in a coding context has high false-negative
// recall (~60-70% industry-wide) and is FP-prone, so it is a visibility signal,
// NEVER a control. Findings are content-free (Sample is the masking placeholder;
// the PII type lives in the Detector name + Category, not the value).
type piiDetector struct{}

func (piiDetector) Name() string { return "pii" }

// piiRule is a simple regex PII rule; validate optionally rejects a regex match
// (e.g. Luhn for credit cards) to cut false positives.
type piiRule struct {
	name     string
	re       *regexp.Regexp
	severity Severity
	validate func(string) bool // nil => accept any regex match
}

var piiRules = []piiRule{
	{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`), SevLow, nil},
	{"us-ssn", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), SevMedium, nil},
	{"credit-card", regexp.MustCompile(`\b(?:\d[ \-]?){13,19}\b`), SevMedium, validLuhn},
	{"iban", regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`), SevMedium, nil},
	{"e164-phone", regexp.MustCompile(`\+[1-9]\d{7,14}\b`), SevLow, nil},
}

func (piiDetector) Scan(s Span, dst *[]Finding) {
	path := sanitizePath(s.FieldPath)
	for _, rule := range piiRules {
		for _, loc := range rule.re.FindAllStringIndex(s.Text, -1) {
			match := s.Text[loc[0]:loc[1]]
			if rule.validate != nil && !rule.validate(match) {
				continue
			}
			*dst = append(*dst, Finding{
				Detector:  "pii:" + rule.name,
				Category:  CategoryPII,
				FieldPath: path,
				Offset:    loc[0],
				Length:    loc[1] - loc[0],
				Severity:  rule.severity,
				Sample:    maskedPlaceholder,
			})
			// One finding per (rule, span) is enough signal; stop after the first
			// validated match to bound output and avoid spamming repeated PII.
			break
		}
	}
}

// validLuhn reports whether the digits of s pass the Luhn checksum (cuts most
// random-number false positives for the credit-card rule).
func validLuhn(s string) bool {
	sum, alt, n := 0, false, 0
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
		n++
	}
	return n >= 13 && sum%10 == 0
}
