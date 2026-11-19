// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A personal-data value used as a JSON key must not ride into a finding's field path, which is
// content-free, whichever detector found the value under it: the PII detector, or the corpus
// detector with detect_pii off.
func TestFieldPathMasksPersonalDataKeys(t *testing.T) {
	piiEng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectPII: true}, nil)
	if err != nil || piiEng == nil {
		t.Fatalf("NewEngine(pii): %v", err)
	}
	secretEng := newTestEngine(t, "alert", testSecret)
	for _, key := range []string{"123-45-6789", "alice@example.com", "4242424242424242", "GB82WEST12345698765432", "+14155552671"} {
		for _, det := range []struct {
			name  string
			eng   *Engine
			value string
		}{{"pii", piiEng, key}, {"secret", secretEng, testSecret}} {
			for _, shape := range []struct {
				name    string
				channel Channel
				body    string
			}{
				{"generic", ChannelGeneric, `{"` + key + `":"` + det.value + `"}`},
				{"tool-input", ChannelAnthropicMessages, `{"model":"c","max_tokens":1,"messages":[{"role":"assistant","content":` +
					`[{"type":"tool_use","name":"x","input":{"` + key + `":"` + det.value + `"}}]}]}`},
			} {
				t.Run(det.name+"/"+shape.name+"/"+key, func(t *testing.T) {
					res, _, err := det.eng.ScanRequest(shape.channel, []byte(shape.body))
					if err != nil || len(res.Findings) == 0 {
						t.Fatalf("scan: %v, findings %+v; want a finding on the value", err, res.Findings)
					}
					for _, f := range res.Findings {
						if strings.Contains(f.FieldPath, key) || !strings.Contains(f.FieldPath, maskedPlaceholder) {
							t.Fatalf("field path %q: want the key %q masked to %q", f.FieldPath, key, maskedPlaceholder)
						}
					}
				})
			}
		}
	}
}
