// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/composertest"
)

// TestClaude_Assist_NoSchema asserts the claude Assist path returns the wrapper's
// .result prose AND passes NO --json-schema flag (advisory free text, not a schema
// object). It also carries the assist system prompt + the operator's question.
func TestClaude_Assist_NoSchema(t *testing.T) {
	const answer = "The sandbox has no internet access, so the agent cannot reach GitHub."
	fake := writeFakeCLI(t, "claude",
		`printf '%s' '{"type":"result","is_error":false,"result":"`+answer+`"}'`)

	c, err := NewComposer(Config{Tool: ToolClaude, Model: "claude-sonnet-4-5", BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	as, ok := c.(composer.Assister)
	if !ok {
		t.Fatal("cli backend must implement composer.Assister")
	}

	got, err := as.Assist(context.Background(), composertest.SampleRequest(), "Can the agent reach GitHub?")
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if got != answer {
		t.Errorf("Assist answer = %q, want %q", got, answer)
	}

	argv := fake.argv(t)
	if slices.Contains(argv, "--json-schema") {
		t.Error("Assist must NOT pass --json-schema (no structured output)")
	}
	// FIX #11: the assist path shells out to claude on the HOST too, so it must carry
	// the same read-only least-privilege flag as Propose/Clarify.
	if v, _ := flagValue(argv, "--permission-mode"); v != "plan" {
		t.Errorf("Assist --permission-mode = %q, want plan (read-only host posture)", v)
	}
	// The assist path also drops operator MCP servers on the host, same as Propose.
	if !slices.Contains(argv, "--strict-mcp-config") {
		t.Error("Assist must pass --strict-mcp-config (no operator MCP servers load)")
	}
	if v, _ := flagValue(argv, "--append-system-prompt"); v != composer.AssistSystemPrompt {
		t.Errorf("--append-system-prompt is not the assist prompt")
	}
	if v, _ := flagValue(argv, "--output-format"); v != "json" {
		t.Errorf("--output-format = %q, want json", v)
	}
	user, _ := flagValue(argv, "-p")
	if !strings.Contains(user, "Operator question: Can the agent reach GitHub?") {
		t.Errorf("-p value missing the operator question: %q", user)
	}
}

// TestCodex_Assist_NoSchema asserts the codex Assist path reads the -o file as
// plain text AND passes NO --output-schema flag.
func TestCodex_Assist_NoSchema(t *testing.T) {
	const answer = "It runs in an isolated sandbox with read-only access to your files."
	fake := writeFakeCLI(t, "codex", codexWriteOutBody(answer))

	c, err := NewComposer(Config{Tool: ToolCodex, Model: "gpt-5-codex", BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	as, ok := c.(composer.Assister)
	if !ok {
		t.Fatal("cli backend must implement composer.Assister")
	}

	got, err := as.Assist(context.Background(), composertest.SampleRequest(), "Can it edit my files?")
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if got != answer {
		t.Errorf("Assist answer = %q, want %q", got, answer)
	}

	argv := fake.argv(t)
	if slices.Contains(argv, "--output-schema") {
		t.Error("Assist must NOT pass --output-schema (no structured output)")
	}
	if v, _ := flagValue(argv, "--sandbox"); v != "read-only" {
		t.Errorf("--sandbox = %q, want read-only", v)
	}
	if v, _ := flagValue(argv, "--ask-for-approval"); v != "never" {
		t.Errorf("--ask-for-approval = %q, want never", v)
	}
	// codex has no system-prompt channel: the assist prompt is prepended to the user.
	if last := argv[len(argv)-1]; !strings.Contains(last, "Operator question: Can it edit my files?") {
		t.Errorf("final positional arg missing the operator question: %q", last)
	}
}
