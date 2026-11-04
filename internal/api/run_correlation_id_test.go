// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestCorrelationIDRefusal: the trimmed value is 128 characters of ASCII letters, digits and . _ : / -
// at most; anything else is a 422 with one sentence.
func TestCorrelationIDRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		refused  bool
	}{
		{"none", "", false},
		{"spaces only is none", "   ", false},
		{"every allowed class", "Build_42.a-b:c/d", false},
		{"trimmed", "  ref-1\t", false},
		{"128 characters", strings.Repeat("a", 128), false},
		{"129 characters", strings.Repeat("a", 129), true},
		{"a space inside", "build 42", true},
		{"a newline inside", "build\n42", true},
		{"a control byte", "build\x0042", true},
		{"non-ASCII", "büild", true},
		{"a quote", `a"b`, true},
		{"a bracket", "a[1]", true},
		{"a comma", "a,b", true},
		{"an equals sign", "a=b", true},
	} {
		refusal := correlationIDRefusal(createRunRequest{CorrelationID: tc.id})
		switch {
		case tc.refused && refusal == nil:
			t.Errorf("%s: %q was accepted", tc.name, tc.id)
		case !tc.refused && refusal != nil:
			t.Errorf("%s: %q was refused: %s", tc.name, tc.id, refusal.body.Error)
		case tc.refused && (refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonCorrelationIDInvalid || refusal.body.Error != correlationIDInvalidMsg):
			t.Errorf("%s: refusal = %d %q %q", tc.name, refusal.status, refusal.body.Reason, refusal.body.Error)
		}
	}
	if got := correlationID("  ref-1\t"); got != "ref-1" {
		t.Errorf("the run keeps %q, want ref-1", got)
	}
	if !strings.HasSuffix(correlationIDInvalidMsg, ".") || correlationIDInvalidMsg[0] < 'A' || correlationIDInvalidMsg[0] > 'Z' {
		t.Errorf("the sentence %q is not a capitalised sentence", correlationIDInvalidMsg)
	}
}
