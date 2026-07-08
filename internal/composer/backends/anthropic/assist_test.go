// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/cjohnstoniv/wardyn/internal/composer"
)

// TestAssist_NoStructuredOutput asserts the Assist wire returns non-empty advisory
// text AND issues NO structured-output request (no output_config, no tools) — the
// answer is inert prose, not a schema-forced object.
func TestAssist_NoStructuredOutput(t *testing.T) {
	var rec capturedRequest
	const answer = "The sandbox has no internet access, so the agent cannot reach GitHub or any other site."
	srv := newFakeServer(t, &rec, answer, "")

	c, err := NewComposer(Config{
		Transport:    TransportAPI,
		Model:        "claude-sonnet-4-5",
		APIKey:       "test",
		extraOptions: []option.RequestOption{option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client())},
	})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	as, ok := c.(composer.Assister)
	if !ok {
		t.Fatal("anthropic backend must implement composer.Assister")
	}

	got, err := as.Assist(context.Background(), sampleRequest(), "Can the agent reach GitHub?")
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if strings.TrimSpace(got) != answer {
		t.Errorf("Assist answer = %q, want %q", got, answer)
	}

	// No structured output: neither the Structured Outputs format nor a forced tool.
	if _, ok := dig(rec.body, "output_config"); ok {
		t.Error("Assist request unexpectedly sent output_config (structured output)")
	}
	if _, ok := rec.body["tools"]; ok {
		t.Error("Assist request unexpectedly sent tools (forced-tool structured output)")
	}

	// System prompt is the plain assist explainer; user turn carries the question.
	sys, _ := rec.body["system"].([]any)
	if len(sys) == 0 {
		t.Fatalf("Assist request missing system prompt")
	}
	sysText, _ := dig(sys[0].(map[string]any), "text")
	if st, _ := sysText.(string); !strings.Contains(st, "ADVISORY ONLY") {
		t.Errorf("Assist system prompt is not the assist prompt: %q", st)
	}
	msgs, _ := rec.body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("Assist request missing messages")
	}
	uc, _ := dig(msgs[0].(map[string]any), "content")
	ucl, _ := uc.([]any)
	if len(ucl) == 0 {
		t.Fatalf("Assist user message empty")
	}
	userText, _ := dig(ucl[0].(map[string]any), "text")
	if ut, _ := userText.(string); !strings.Contains(ut, "Operator question: Can the agent reach GitHub?") {
		t.Errorf("Assist user message missing operator question: %q", ut)
	}

	// The answer budget is capped.
	if mt, _ := rec.body["max_tokens"].(float64); int(mt) != composer.AssistMaxTokens {
		t.Errorf("max_tokens = %v, want %d", rec.body["max_tokens"], composer.AssistMaxTokens)
	}
}
