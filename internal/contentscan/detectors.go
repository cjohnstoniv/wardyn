// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"bytes"
	"strings"
)

// knownSecretDetector matches operator-declared known secret VALUES exactly
// (the same low-false-positive philosophy as internal/secretmask: exact byte
// match, no regex, no entropy). When normalize is set it additionally matches
// against bounded decoded variants of the span (normalize.go), narrowing the
// encoding-evasion residual without closing it.
//
// This is the only detector in the conservative v1 cut. Regex/entropy/PII
// detectors are added in later phases behind their own policy toggles.
type knownSecretDetector struct {
	secrets   [][]byte // each >= secretmask.MinLen, deduped (see filterCorpus)
	normalize bool
}

func (d *knownSecretDetector) Name() string { return "known-secret" }

func (d *knownSecretDetector) Scan(s Span, dst *[]Finding) {
	// Field paths are built partly from agent-controlled JSON object keys, so a
	// secret used AS a key could otherwise ride into the (SIEM-fanned) audit log.
	// Mask BOTH corpus values (safePath) AND well-known secret FORMATS
	// (sanitizePath) out of the path so a Finding is content-free by construction
	// — a regex-shaped key (e.g. "ghp_…") must not leak via the path either.
	path := sanitizePath(d.safePath(s.FieldPath))
	raw := []byte(s.Text)
	for i, sec := range d.secrets {
		// Report only the FIRST occurrence per (secret, span): one finding is
		// enough to flag the leak, and we never emit the matched bytes.
		if idx := bytes.Index(raw, sec); idx >= 0 {
			*dst = append(*dst, Finding{
				Detector:  "known-secret",
				Category:  CategorySecret,
				FieldPath: path,
				Offset:    idx,
				Length:    len(sec),
				Severity:  SevCritical,
				Sample:    maskedPlaceholder,
				matchID:   i + 1, // 1-based corpus index; content-free dedup key
			})
		}
	}
	if !d.normalize {
		return
	}
	// Decoded-variant pass: catches a known secret an agent percent-/base64-/
	// hex-encoded. Offsets are meaningless in the decoded space, so they are
	// reported as -1 and the detector name records the decode chain.
	for _, v := range normalizedVariants(s.Text, 2) {
		vb := []byte(v.text)
		for i, sec := range d.secrets {
			if bytes.Contains(vb, sec) {
				*dst = append(*dst, Finding{
					Detector:  "known-secret:" + v.chain,
					Category:  CategorySecret,
					FieldPath: path,
					Offset:    -1,
					Length:    len(sec),
					Severity:  SevCritical,
					Sample:    maskedPlaceholder,
					matchID:   i + 1, // distinguishes distinct corpus secrets in one variant
				})
			}
		}
	}
}

// safePath masks any corpus secret value that appears verbatim in a field path
// (agent-controlled object keys can contain one) so the path is content-free.
func (d *knownSecretDetector) safePath(path string) string {
	return maskCorpus(path, d.secrets)
}

// maskCorpus replaces any operator-declared corpus secret VALUE appearing
// verbatim in a field path with the masking placeholder, so an agent-controlled
// JSON key that IS a known secret cannot leak into the (SIEM-fanned) audit. Used
// both per-detector (safePath) and at the engine's central chokepoint
// (Engine.sanitizeFieldPath) so no detector path can bypass it. Idempotent.
func maskCorpus(path string, secrets [][]byte) string {
	if path == "" {
		return path
	}
	for _, sec := range secrets {
		if s := string(sec); s != "" && strings.Contains(path, s) {
			path = strings.ReplaceAll(path, s, maskedPlaceholder)
		}
	}
	return path
}
