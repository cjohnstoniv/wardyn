// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// sidecarDetector forwards each span's text to an OPERATOR-CONFIGURED
// out-of-process detection service over HTTP and maps its content-free
// findings back — how Python-only PII/NER engines plug into the Go proxy.
// Scan is FAIL-OPEN: any error/timeout/non-200 yields no findings and never
// blocks the traffic path; whether the outage itself blocks is a separate
// Engine-level decision (ShouldBlock, keyed on OnScannerError). The URL is
// trusted operator config, not agent-chosen, so this is not an SSRF surface.
type sidecarDetector struct {
	url    string
	client *http.Client
}

func newSidecarDetector(url string, client *http.Client) *sidecarDetector {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	return &sidecarDetector{url: url, client: client}
}

type sidecarRequest struct {
	Text      string `json:"text"`
	FieldPath string `json:"field_path"`
}

type sidecarFinding struct {
	Type     string `json:"type"`
	Category string `json:"category"` // "secret" | "pii" | "classified"
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Severity string `json:"severity"`
}

type sidecarResponse struct {
	Findings []sidecarFinding `json:"findings"`
}

// Scan satisfies the Detector interface, swallowing the underlying error so a
// sidecar hiccup never bricks the pipeline. The Engine calls scanReport
// directly instead, so it can record a failed scan as degraded rather than
// indistinguishable from a genuinely clean inspection.
func (d *sidecarDetector) Scan(s Span, dst *[]Finding) {
	_ = d.scanReport(s, dst)
}

// scanReport returns a non-nil error when the sidecar could NOT be consulted,
// letting the Engine mark Result.Skipped/"sidecar_error" so ShouldBlock can
// honor OnScannerError. The traffic path never blocks on this error directly.
func (d *sidecarDetector) scanReport(s Span, dst *[]Finding) error {
	body, err := json.Marshal(sidecarRequest{Text: s.Text, FieldPath: s.FieldPath})
	if err != nil {
		return fmt.Errorf("sidecar: marshal request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sidecar: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("sidecar: request failed: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sidecar: non-200 status %d", resp.StatusCode)
	}
	var sr sidecarResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&sr); err != nil {
		return fmt.Errorf("sidecar: decode response: %w", err)
	}
	path := sanitizePath(s.FieldPath)
	for _, f := range sr.Findings {
		cat := Category(strings.ToLower(f.Category))
		switch cat {
		case CategorySecret, CategoryPII, CategoryClassified:
		default:
			cat = CategoryPII
		}
		sev := Severity(strings.ToLower(f.Severity))
		if severityRank(sev) == 0 {
			sev = SevMedium
		}
		length := f.End - f.Start
		if length < 0 {
			length = 0
		}
		typ := f.Type
		if typ == "" {
			typ = "finding"
		}
		*dst = append(*dst, Finding{
			Detector:  "sidecar:" + typ,
			Category:  cat,
			FieldPath: path,
			Offset:    f.Start,
			Length:    length,
			Severity:  sev,
			Sample:    maskedPlaceholder,
		})
	}
	return nil
}
