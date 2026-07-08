// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"errors"
	"strings"
	"testing"
)

// ws is a valid (ephemeral) workspace so size/prompt checks are reached.
var ws = Workspace{Kind: WorkspaceEphemeral}

func TestValidateRequest_AcceptsReasonableInput(t *testing.T) {
	err := ValidateRequest(ComposeRequest{
		Prompt:      "refactor the auth package and open a PR",
		Workspace:   ws,
		Attachments: []Attachment{{Name: "notes.md", Content: "some context"}},
		Sources:     []string{"https://example.com/spec"},
	})
	if err != nil {
		t.Fatalf("reasonable input rejected: %v", err)
	}
}

func TestValidateRequest_RequiresPromptOrAttachment(t *testing.T) {
	if err := ValidateRequest(ComposeRequest{Workspace: ws}); err == nil {
		t.Errorf("empty request should be rejected")
	}
	// An attachment alone (with a workspace) is allowed.
	if err := ValidateRequest(ComposeRequest{Workspace: ws, Attachments: []Attachment{{Name: "x", Content: "y"}}}); err != nil {
		t.Errorf("attachment-only request should be allowed: %v", err)
	}
}

func TestValidateRequest_RequiresWorkspace(t *testing.T) {
	// No workspace -> rejected even with a good prompt.
	if err := ValidateRequest(ComposeRequest{Prompt: "do a thing"}); err == nil {
		t.Errorf("missing workspace should be rejected")
	}
	// Local without a path -> rejected.
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: Workspace{Kind: WorkspaceLocal}}); err == nil {
		t.Errorf("local workspace without a path should be rejected")
	}
	// Git without a repo -> rejected.
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: Workspace{Kind: WorkspaceGit}}); err == nil {
		t.Errorf("git workspace without a repo should be rejected")
	}
	// Unknown kind -> rejected.
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: Workspace{Kind: "bogus"}}); err == nil {
		t.Errorf("unknown workspace kind should be rejected")
	}
	// Each valid shape is accepted.
	for _, w := range []Workspace{
		{Kind: WorkspaceEphemeral},
		{Kind: WorkspaceLocal, Path: "/home/me/project"},
		{Kind: WorkspaceGit, Repo: "acme/widgets"},
	} {
		if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: w}); err != nil {
			t.Errorf("workspace %+v should be accepted: %v", w, err)
		}
	}
}

func TestValidateRequest_EnforcesSizeCaps(t *testing.T) {
	big := strings.Repeat("a", MaxPromptBytes+1)
	if err := ValidateRequest(ComposeRequest{Prompt: big, Workspace: ws}); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("oversized prompt should be ErrInputTooLarge, got %v", err)
	}

	bigAtt := strings.Repeat("b", MaxAttachmentBytes+1)
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: ws, Attachments: []Attachment{{Name: "f", Content: bigAtt}}}); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("oversized attachment should be ErrInputTooLarge, got %v", err)
	}

	// Too many attachments.
	many := make([]Attachment, MaxAttachmentsCount+1)
	for i := range many {
		many[i] = Attachment{Name: "f", Content: "x"}
	}
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: ws, Attachments: many}); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("too many attachments should be ErrInputTooLarge, got %v", err)
	}

	// Total across many medium attachments exceeding the global cap.
	n := (MaxTotalInputBytes / MaxAttachmentBytes) + 2
	atts := make([]Attachment, 0, n)
	for i := 0; i < n && i < MaxAttachmentsCount; i++ {
		atts = append(atts, Attachment{Name: "f", Content: strings.Repeat("c", MaxAttachmentBytes)})
	}
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: ws, Attachments: atts}); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("total over cap should be ErrInputTooLarge, got %v", err)
	}

	// Too many sources.
	srcs := make([]string, MaxSources+1)
	for i := range srcs {
		srcs[i] = "https://x"
	}
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: ws, Sources: srcs}); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("too many sources should be ErrInputTooLarge, got %v", err)
	}
}

func TestValidateRequest_SessionID(t *testing.T) {
	// Empty is fine (the endpoint mints a fallback correlation id).
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: ws}); err != nil {
		t.Errorf("empty session_id should be accepted: %v", err)
	}
	// A real UUID is fine.
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: ws, SessionID: "8f14e45f-ceea-4f7a-9b9e-c3f5b2b0ad4a"}); err != nil {
		t.Errorf("UUID session_id should be accepted: %v", err)
	}
	// Anything else is a 400 (not ErrInputTooLarge — a different failure class).
	if err := ValidateRequest(ComposeRequest{Prompt: "x", Workspace: ws, SessionID: "not-a-uuid"}); err == nil {
		t.Errorf("non-UUID session_id should be rejected")
	} else if errors.Is(err, ErrInputTooLarge) {
		t.Errorf("non-UUID session_id should NOT be ErrInputTooLarge, got %v", err)
	}
}

func TestCapAuditText(t *testing.T) {
	short := "hello"
	if got := CapAuditText(short); got != short {
		t.Errorf("short text must pass through unchanged, got %q", got)
	}

	long := strings.Repeat("x", MaxAuditFieldBytes+500)
	got := CapAuditText(long)
	if len(got) > MaxAuditFieldBytes {
		t.Errorf("capped text len = %d, want <= %d", len(got), MaxAuditFieldBytes)
	}
	if !strings.HasSuffix(got, auditTruncatedMarker) {
		t.Errorf("truncated text must carry the explicit marker, got suffix %q", got[max(0, len(got)-30):])
	}
	// Exactly at the cap: unchanged, no marker.
	exact := strings.Repeat("y", MaxAuditFieldBytes)
	if got := CapAuditText(exact); got != exact {
		t.Errorf("text exactly at the cap must pass through unchanged")
	}
}
