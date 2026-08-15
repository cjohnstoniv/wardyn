// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestTRYITDoc_LLMPrecedenceMatchesResolveLLMTransport is the W5-S1-6
// regression: docs/TRY-IT.md used to claim a three-way "subscription → Bedrock
// → api-key" precedence, omitting the managed-subscription step AND the
// api-key-opt-in-suppresses-managed exception resolveLLMTransport actually
// implements (runs_dispatch_llm.go). Anchor the doc's claim to that function's
// own precedence comment so the two can't drift apart silently again.
func TestTRYITDoc_LLMPrecedenceMatchesResolveLLMTransport(t *testing.T) {
	doc, err := os.ReadFile("../../docs/TRY-IT.md")
	if err != nil {
		t.Fatalf("read docs/TRY-IT.md: %v", err)
	}
	src, err := os.ReadFile("runs_dispatch_llm.go")
	if err != nil {
		t.Fatalf("read runs_dispatch_llm.go: %v", err)
	}

	// The source's own stated precedence is the ground truth this doc must match.
	if !regexp.MustCompile(`host-staged mount ?[>-]+ ?managed ?[>-]+ ?Bedrock ?[>-]+ ?api-key`).Match(src) {
		t.Fatal("runs_dispatch_llm.go's precedence comment changed shape — update this guard's pattern (and re-check docs/TRY-IT.md by hand)")
	}

	docStr := string(doc)
	for _, want := range []string{"managed subscription", "opt-in", "suppresses"} {
		if !strings.Contains(docStr, want) {
			t.Errorf("docs/TRY-IT.md's model-auth section is missing %q — it must state the managed step and the api-key opt-in exception", want)
		}
	}
	// The old three-way claim (no managed step, no exception) must be gone.
	if regexp.MustCompile(`precedence: subscription (→|->) Bedrock (→|->) api-key`).MatchString(docStr) {
		t.Error(`docs/TRY-IT.md still states the stale three-way "subscription → Bedrock → api-key" precedence`)
	}
}
