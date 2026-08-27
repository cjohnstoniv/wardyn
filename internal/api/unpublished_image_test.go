// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

// agentImage() falls back to the ghcr.io convention ref for any agent with no
// explicit WARDYN_AGENT_IMAGES entry. Wardyn does not publish agent-claude-code
// (it bundles a proprietary vendor CLI), so that fallback resolves to a tag which
// looks like it should exist and does not. Warn with the reason and the fix, so
// the operator is not decoding an ImagePullBackOff.
func TestUnpublishedAgentImage(t *testing.T) {
	t.Run("claude-code on the convention fallback warns", func(t *testing.T) {
		why := unpublishedAgentImage("claude-code", nil)
		if why == "" {
			t.Fatal("claude-code has no published convention image; the operator must be told")
		}
		// The warning is only useful if it says what to DO.
		if !strings.Contains(why, "make agent-images") {
			t.Errorf("warning names no fix: %q", why)
		}
		if !strings.Contains(why, "WARDYN_AGENT_IMAGES") {
			t.Errorf("warning does not mention the override that also resolves it: %q", why)
		}
	})

	// An operator who supplied their own image has already solved this; saying
	// anything would be noise, and worse, wrong.
	t.Run("explicit image override says nothing", func(t *testing.T) {
		images := map[string]string{"claude-code": "registry.corp/agents/claude:1.2.3"}
		if why := unpublishedAgentImage("claude-code", images); why != "" {
			t.Fatalf("an explicit override must silence the warning, got %q", why)
		}
	})

	// Every other agent resolves to an image we do publish.
	t.Run("published agents say nothing", func(t *testing.T) {
		for _, agent := range []string{"codex-cli", "aws-sso"} {
			if why := unpublishedAgentImage(agent, nil); why != "" {
				t.Errorf("%s is published; warning should be empty, got %q", agent, why)
			}
		}
	})
}
