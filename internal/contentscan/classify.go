// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import "strings"

// classifyDetector flags operator-defined CLASSIFICATION markers (e.g.
// "INTERNAL ONLY", "CONFIDENTIAL//NOFORN") appearing in outbound content — the
// "proprietary content shouldn't leave the walled garden" sense. Case-insensitive
// substring match. The marker text is operator config (not a secret); findings
// stay content-free (Sample = placeholder).
type classifyDetector struct {
	markers []string // lowercased, non-empty
}

func newClassifyDetector(markers []string) (classifyDetector, bool) {
	out := make([]string, 0, len(markers))
	for _, m := range markers {
		if t := strings.ToLower(strings.TrimSpace(m)); t != "" {
			out = append(out, t)
		}
	}
	return classifyDetector{markers: out}, len(out) > 0
}

func (classifyDetector) Name() string { return "classify" }

func (d classifyDetector) Scan(s Span, dst *[]Finding) {
	lower := strings.ToLower(s.Text)
	path := sanitizePath(s.FieldPath)
	for _, m := range d.markers {
		if idx := strings.Index(lower, m); idx >= 0 {
			*dst = append(*dst, Finding{
				Detector:  "classified-marker",
				Category:  CategoryClassified,
				FieldPath: path,
				Offset:    idx,
				Length:    len(m),
				Severity:  SevHigh,
				Sample:    maskedPlaceholder,
			})
		}
	}
}
