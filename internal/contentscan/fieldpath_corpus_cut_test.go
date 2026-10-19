// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A corpus secret inside a field path is masked before the path is cut to
// length: wherever the cut falls, no prefix of the secret survives in the
// finding's path (#2284).
func TestScan_CorpusSecretKeyStraddlingTheTruncationCutIsMasked(t *testing.T) {
	const secret = "internal-key-Zq9XvL2mTp8RwK4nBc7YdH3sJf6A"
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectPII: true}, [][]byte{[]byte(secret)})
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// The path prefix before the key is a fixed length, so sweeping the padding
	// puts the 160-byte cut at every offset of the secret.
	for pad := 40; pad <= 170; pad++ {
		key := strings.Repeat("a", pad) + secret + strings.Repeat("b", 100)
		body := []byte(`{"model":"c","max_tokens":1,"messages":[{"role":"assistant","content":` +
			`[{"type":"tool_use","name":"x","input":{"` + key + `":"alice@example.com"}}]}]}`)
		res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
		if err != nil || len(res.Findings) == 0 {
			t.Fatalf("pad %d: scan = %+v, %v; want a pii finding", pad, res, err)
		}
		for _, f := range res.Findings {
			if strings.Contains(f.FieldPath, secret[:10]) || strings.Contains(f.FieldPath, secret[len(secret)-10:]) {
				t.Fatalf("pad %d: field path carries a piece of the corpus secret: %q", pad, f.FieldPath)
			}
			if len(f.FieldPath) > maxFieldPathBytes {
				t.Fatalf("pad %d: field path is %d bytes, want <= %d", pad, len(f.FieldPath), maxFieldPathBytes)
			}
		}
	}
}
