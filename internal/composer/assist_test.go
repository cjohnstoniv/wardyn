// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// assistFake implements Composer (via the embedded FakeComposer) AND Assister, so
// the registry's type-assert resolves it and passes the question through.
type assistFake struct {
	FakeComposer
	answer       string
	lastQuestion string
}

func (a *assistFake) Assist(_ context.Context, _ ComposeRequest, question string) (string, error) {
	a.lastQuestion = question
	return a.answer, nil
}

// TestRegistryAssist_PassThroughDegradeUnknown covers all three registry paths: a
// backend that implements Assister returns its answer verbatim; one that does NOT
// degrades to the friendly unavailable string (never an error); an unknown name is
// ErrUnknownBackend.
func TestRegistryAssist_PassThroughDegradeUnknown(t *testing.T) {
	af := &assistFake{answer: "The agent runs in an isolated sandbox and cannot touch your files."}
	reg, err := NewRegistry("explains", []RegistryEntry{
		{Info: BackendInfo{Name: "explains"}, Composer: af},
		{Info: BackendInfo{Name: "silent"}, Composer: &FakeComposer{}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Assister backend: answer passes through.
	got, err := reg.Assist(context.Background(), "explains", ComposeRequest{}, "Can it edit my files?")
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if got != af.answer {
		t.Errorf("Assist = %q, want %q", got, af.answer)
	}
	if !strings.Contains(af.lastQuestion, "Can it edit my files?") {
		t.Errorf("backend saw question %q, want the operator's question", af.lastQuestion)
	}

	// Non-Assister backend: friendly degrade, no error.
	got, err = reg.Assist(context.Background(), "silent", ComposeRequest{}, "Anything?")
	if err != nil {
		t.Fatalf("degrade Assist error: %v", err)
	}
	if !strings.Contains(got, "isn't available") {
		t.Errorf("degrade answer = %q, want the unavailable string", got)
	}

	// Unknown backend: ErrUnknownBackend.
	if _, err := reg.Assist(context.Background(), "nope", ComposeRequest{}, "q?"); !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("unknown-backend err = %v, want ErrUnknownBackend", err)
	}
}

// TestAssistUserMessage_CarriesQuestion asserts the shared user-turn builder folds
// the operator's question onto the grounded setup context.
func TestAssistUserMessage_CarriesQuestion(t *testing.T) {
	msg := AssistUserMessage(ComposeRequest{
		Prompt:    "add a healthz endpoint",
		Workspace: Workspace{Kind: WorkspaceEphemeral},
	}, "  Can it reach the internet?  ")
	if !strings.Contains(msg, "Operator question: Can it reach the internet?") {
		t.Errorf("AssistUserMessage missing trimmed operator question:\n%s", msg)
	}
	if !strings.Contains(msg, "TASK DESCRIPTION") {
		t.Errorf("AssistUserMessage missing grounded setup context:\n%s", msg)
	}
}
