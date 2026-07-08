// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
)

// assistCompletionJSON is a plain-text (non-schema) Chat Completions reply — the
// shape an Assist call gets when NO response_format is sent.
const assistCompletionJSON = `{
  "id": "chatcmpl-assist",
  "object": "chat.completion",
  "model": "gpt-4o",
  "choices": [
    {"index": 0, "message": {"role": "assistant", "content": "The sandbox has no internet access, so the agent cannot reach GitHub."}, "finish_reason": "stop"}
  ]
}`

// TestAssist_NoResponseFormat asserts Assist returns non-empty advisory text AND
// issues NO structured-output request (no response_format / json_schema).
func TestAssist_NoResponseFormat(t *testing.T) {
	var cap capturedRequest
	var calls int32
	srv := newFakeOpenAI(t, &cap, &calls, assistCompletionJSON)

	c, err := NewComposer(Config{Transport: TransportAPI, Model: "gpt-4o", APIKey: "sk-x", BaseURL: srv.URL, httpClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	as, ok := c.(composer.Assister)
	if !ok {
		t.Fatal("openai backend must implement composer.Assister")
	}

	got, err := as.Assist(context.Background(), sampleRequest(), "Can the agent reach GitHub?")
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if !strings.Contains(got, "cannot reach GitHub") {
		t.Errorf("Assist answer = %q, want the fixture text", got)
	}

	// No structured output on the wire.
	if _, ok := cap.body["response_format"]; ok {
		t.Error("Assist request unexpectedly sent response_format (structured output)")
	}
	// System prompt is the assist explainer, user carries the operator question.
	msgs, ok := cap.body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("Assist messages = %#v, want 2", cap.body["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	if sc, _ := sys["content"].(string); !strings.Contains(sc, "ADVISORY ONLY") {
		t.Errorf("Assist system message not the assist prompt: %q", sc)
	}
	usr, _ := msgs[1].(map[string]any)
	if uc, _ := usr["content"].(string); !strings.Contains(uc, "Operator question: Can the agent reach GitHub?") {
		t.Errorf("Assist user message missing operator question: %q", uc)
	}
	// The answer budget is capped.
	if _, ok := cap.body["max_completion_tokens"]; !ok {
		t.Error("Assist request missing max_completion_tokens cap")
	}
}
