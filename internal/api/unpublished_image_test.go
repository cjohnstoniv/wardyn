// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

// Wardyn does not redistribute a vendor CLI, so `--agent claude-code` cannot get
// one from us. What the operator SEES changed in 0.7: the catalog row's ImageKey
// now points at the published agent-base, so the pull succeeds and the CLI is
// simply absent from PATH, where it used to be a registry 404 on a tag that
// looks like it should exist. Warn about the symptom they will actually hit,
// with the fix, so they are not debugging an empty PATH either.
func TestUnpublishedAgentImage(t *testing.T) {
	t.Run("claude-code on the convention fallback warns", func(t *testing.T) {
		got := withUnpublishedImageWarning(nil, "claude-code", nil)
		if len(got) != 1 {
			t.Fatalf("claude-code resolves to an image with no vendor CLI; the operator must be told. got %v", got)
		}
		why := got[0]
		// The warning is only useful if it says what to DO.
		if !strings.Contains(why, "make agent-images") {
			t.Errorf("warning names no fix: %q", why)
		}
		if !strings.Contains(why, "WARDYN_AGENT_IMAGES") {
			t.Errorf("warning does not mention the override that also resolves it: %q", why)
		}
		// It must describe the CURRENT failure. A warning still promising a 404
		// sends the operator hunting a pull error that no longer happens.
		if !strings.Contains(why, "agent-base") {
			t.Errorf("warning does not name the image the run actually resolves to: %q", why)
		}
		if !strings.Contains(why, "not be on PATH") {
			t.Errorf("warning does not describe the symptom (CLI absent), which is what the operator now hits: %q", why)
		}
	})

	// The re-point itself: `--agent claude-code` must resolve to an image that
	// EXISTS on a published install. Before 0.7 it resolved to
	// agent-claude-code, which is not published, so this was the only agent name
	// besides codex-cli a user could pick — and it 404'd.
	t.Run("claude-code resolves to the published base image", func(t *testing.T) {
		def, ok := harnessByID("claude-code")
		if !ok {
			t.Fatal("claude-code is not in the harness catalog")
		}
		if def.ImageKey != "base" {
			t.Errorf("claude-code ImageKey = %q, want \"base\" — agent-claude-code is NOT published, so any other key 404s on every published install", def.ImageKey)
		}
		if got := agentImage("claude-code", nil); !strings.Contains(got, "agent-base") {
			t.Errorf("agentImage(claude-code) = %q, want the published agent-base ref", got)
		}
	})

	// ...but the container-login lane must NOT follow that re-point: a login
	// sandbox has to carry the vendor CLI it is logging into, and agent-base
	// ships none, so "Log in with Claude" would open a box where `claude` does
	// not exist and the flow could never complete.
	t.Run("the login lane keeps the vendor-CLI image", func(t *testing.T) {
		def, ok := harnessByID("claude-code")
		if !ok || def.Login == nil {
			t.Fatal("claude-code has no login convention")
		}
		if def.Login.loginImageKey != "claude-code" {
			t.Fatalf("login lane imageKey = %q, want \"claude-code\": agent-base carries no vendor CLI, so a login sandbox built from it can never complete the flow", def.Login.loginImageKey)
		}
		got := agentImageForKey("claude-code", def.Login.loginImageKey, nil)
		if !strings.Contains(got, "agent-claude-code") {
			t.Errorf("login image = %q, want the vendor-CLI ref", got)
		}
		// The operator map still wins for BOTH lanes — an operator who builds
		// the vendor image once pins it everywhere.
		images := map[string]string{"claude-code": "registry.corp/agents/claude:1.2.3"}
		if got := agentImageForKey("claude-code", def.Login.loginImageKey, images); got != images["claude-code"] {
			t.Errorf("login image ignored the operator override: %q", got)
		}
	})

	// An operator who supplied their own image has already solved this; saying
	// anything would be noise, and worse, wrong.
	t.Run("explicit image override says nothing", func(t *testing.T) {
		images := map[string]string{"claude-code": "registry.corp/agents/claude:1.2.3"}
		if got := withUnpublishedImageWarning(nil, "claude-code", images); len(got) != 0 {
			t.Fatalf("an explicit override must silence the warning, got %v", got)
		}
	})

	// Every other agent resolves to an image we do publish.
	t.Run("published agents say nothing", func(t *testing.T) {
		for _, agent := range []string{"codex-cli", "aws-sso"} {
			if got := withUnpublishedImageWarning(nil, agent, nil); len(got) != 0 {
				t.Errorf("%s is published; warning should be empty, got %v", agent, got)
			}
		}
	})
}
