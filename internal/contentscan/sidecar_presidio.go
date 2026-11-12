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
	"unicode/utf8"
)

// This file speaks the analyzer API Presidio documents (POST /analyze). Only the request and
// reply shapes are used; nothing of Presidio is bundled or copied, and the operator runs it
// as their own deployment.

// presidioScoreThreshold is the lowest score the analyzer returns. 0.5 is the analyzer's own
// usual cut: below it a hit is more likely noise than a finding.
const presidioScoreThreshold = 0.5

// presidioHighScore is the score from which an entity counts as high severity.
const presidioHighScore = 0.85

// presidioRequest is the whole request. It names no entities, so the recognizers the operator
// configured decide what is looked for, and it has no field path, which the analyzer does not take.
type presidioRequest struct {
	Text           string  `json:"text"`
	Language       string  `json:"language"`
	ScoreThreshold float64 `json:"score_threshold"`
}

type presidioResult struct {
	EntityType string  `json:"entity_type"`
	Start      int     `json:"start"` // code points, not bytes
	End        int     `json:"end"`
	Score      float64 `json:"score"`
}

// presidioLowEntities are the entity types that show up in ordinary code and prose. They are
// reported at low severity, so a block_min_severity of medium lets them through.
var presidioLowEntities = map[string]bool{
	"PERSON": true, "LOCATION": true, "NRP": true, "ORGANIZATION": true, "DATE_TIME": true,
	"URL": true, "UUID": true, "MAC_ADDRESS": true, "IP_ADDRESS": true, "US_DRIVER_LICENSE": true,
}

func presidioSeverity(entity string, score float64) Severity {
	switch {
	case presidioLowEntities[strings.ToUpper(entity)]:
		return SevLow
	case score >= presidioHighScore:
		return SevHigh
	default:
		return SevMedium
	}
}

func (d *sidecarDetector) scanPresidio(ctx context.Context, s Span, dst *[]Finding) error {
	if strings.TrimSpace(s.Text) == "" {
		return nil // the analyzer refuses empty text, and there is nothing to find in it
	}
	body, err := json.Marshal(presidioRequest{Text: s.Text, Language: "en", ScoreThreshold: presidioScoreThreshold})
	if err != nil {
		return fmt.Errorf("sidecar: marshal request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("sidecar: read response: %w", err)
	}
	// Only an array is a reply: a bare null would decode to "no findings" and read as clean.
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return fmt.Errorf("sidecar: presidio reply is not an array")
	}
	var results []presidioResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return fmt.Errorf("sidecar: decode response: %w", err)
	}
	if len(results) == 0 {
		return nil
	}
	byteAt := runeByteOffsets(s.Text)
	path := maskFormats(s.FieldPath)
	for _, r := range results {
		start, end := clampRunes(r.Start, len(byteAt)-1), clampRunes(r.End, len(byteAt)-1)
		if end < start {
			end = start
		}
		typ := r.EntityType
		if typ == "" {
			typ = "finding"
		}
		*dst = append(*dst, Finding{
			Detector:  "sidecar:" + typ,
			Category:  CategoryPII,
			FieldPath: path,
			Offset:    byteAt[start],
			Length:    byteAt[end] - byteAt[start],
			Severity:  presidioSeverity(r.EntityType, r.Score),
			Sample:    maskedPlaceholder,
		})
	}
	return nil
}

// runeByteOffsets maps a code point index to its byte offset in text, with one extra entry at
// the end for len(text). A byte that is not valid UTF-8 counts as one code point, as it does
// when the text is encoded to JSON.
func runeByteOffsets(text string) []int {
	out := make([]int, 0, utf8.RuneCountInString(text)+1)
	for i := range text {
		out = append(out, i)
	}
	return append(out, len(text))
}

func clampRunes(i, max int) int {
	if i < 0 {
		return 0
	}
	if i > max {
		return max
	}
	return i
}
