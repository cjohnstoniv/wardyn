// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGenerateDevcontainer_ProfilesToJSON asserts a couple of profiles produce
// the exact expected devcontainer.json (base image + one feature per
// feature-supported language, features sorted deterministically).
func TestGenerateDevcontainer_ProfilesToJSON(t *testing.T) {
	cases := []struct {
		name  string
		prof  WorkspaceProfile
		tools []string
		want  string
	}{
		{
			name: "no languages -> base image only, no features key",
			prof: WorkspaceProfile{},
			want: `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu"
}
`,
		},
		{
			name: "single language",
			prof: WorkspaceProfile{Languages: []string{"Go"}},
			want: `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "features": {
    "ghcr.io/devcontainers/features/go:1": {}
  }
}
`,
		},
		{
			name: "multiple languages -> one feature each, sorted by ref",
			prof: WorkspaceProfile{Languages: []string{"Go", "JavaScript", "Python"}},
			want: `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "features": {
    "ghcr.io/devcontainers/features/go:1": {},
    "ghcr.io/devcontainers/features/node:1": {},
    "ghcr.io/devcontainers/features/python:1": {}
  }
}
`,
		},
		{
			name: "language with no official feature is skipped",
			prof: WorkspaceProfile{Languages: []string{"Dart", "Go"}},
			want: `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "features": {
    "ghcr.io/devcontainers/features/go:1": {}
  }
}
`,
		},
		{
			// The honesty case: codex-cli has no verified native-download
			// contract (genAgentToolInstall), so naming it on a workspace with
			// no JS must bake NOTHING — never a guessed URL. The output is
			// byte-identical to a plain Go profile with no tools at all.
			name:  "codex-cli named but no JS in profile -> nothing bakeable, no onCreateCommand",
			prof:  WorkspaceProfile{Languages: []string{"Go"}},
			tools: []string{"codex-cli"},
			want: `{
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
  "features": {
    "ghcr.io/devcontainers/features/go:1": {}
  }
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := GenerateDevcontainer(tc.prof, tc.tools)
			if err != nil {
				t.Fatalf("GenerateDevcontainer: %v", err)
			}
			got, ok := files[genDevcontainerPath]
			if !ok {
				t.Fatalf("missing %s in output; got keys %v", genDevcontainerPath, keysOf(files))
			}
			if len(files) != 1 {
				t.Errorf("expected exactly one generated file, got %d: %v", len(files), keysOf(files))
			}
			if got != tc.want {
				t.Errorf("devcontainer.json mismatch:\n got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestGenerateDevcontainer_Deterministic guards the cache-key contract: the
// same profile must hash to identical bytes across calls regardless of the
// language slice order.
func TestGenerateDevcontainer_Deterministic(t *testing.T) {
	a, err := GenerateDevcontainer(WorkspaceProfile{Languages: []string{"Python", "Go", "Rust"}}, nil)
	if err != nil {
		t.Fatalf("GenerateDevcontainer a: %v", err)
	}
	b, err := GenerateDevcontainer(WorkspaceProfile{Languages: []string{"Rust", "Go", "Python"}}, nil)
	if err != nil {
		t.Fatalf("GenerateDevcontainer b: %v", err)
	}
	if a[genDevcontainerPath] != b[genDevcontainerPath] {
		t.Errorf("non-deterministic output:\n a:\n%s\n b:\n%s", a[genDevcontainerPath], b[genDevcontainerPath])
	}
}

// TestGenerateDevcontainer_AgentToolInstall pins the npm lanes' EXACT
// onCreateCommand content — safe to compare directly (unlike the native lane,
// these carry no characters JSON needs to escape). Parses the emitted
// devcontainer.json rather than hand-writing its escaped form.
func TestGenerateDevcontainer_AgentToolInstall(t *testing.T) {
	cases := []struct {
		name  string
		prof  WorkspaceProfile
		tools []string
		want  string
	}{
		{
			name:  "claude-code via npm when JS is in the profile",
			prof:  WorkspaceProfile{Languages: []string{"JavaScript"}},
			tools: []string{"claude-code"},
			want:  "set -eu\nnpm install -g @anthropic-ai/claude-code",
		},
		{
			name:  "codex-cli via npm when JS is in the profile",
			prof:  WorkspaceProfile{Languages: []string{"JavaScript"}},
			tools: []string{"codex-cli"},
			want:  "set -eu\nnpm install -g @openai/codex",
		},
		{
			name:  "both tools named, JS profile -> both npm lines",
			prof:  WorkspaceProfile{Languages: []string{"JavaScript"}},
			tools: []string{"claude-code", "codex-cli"},
			want:  "set -eu\nnpm install -g @anthropic-ai/claude-code\nnpm install -g @openai/codex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := GenerateDevcontainer(tc.prof, tc.tools)
			if err != nil {
				t.Fatalf("GenerateDevcontainer: %v", err)
			}
			var parsed struct {
				OnCreateCommand string `json:"onCreateCommand"`
			}
			if err := json.Unmarshal([]byte(files[genDevcontainerPath]), &parsed); err != nil {
				t.Fatalf("devcontainer.json not valid JSON: %v", err)
			}
			if parsed.OnCreateCommand != tc.want {
				t.Errorf("onCreateCommand = %q, want %q", parsed.OnCreateCommand, tc.want)
			}
		})
	}
}

// TestGenerateDevcontainer_NativeInstallRoundTrips checks the native lane
// (whose script contains characters JSON must escape) by round-tripping
// through the real marshal/unmarshal instead of hand-computing escaped JSON.
func TestGenerateDevcontainer_NativeInstallRoundTrips(t *testing.T) {
	prof := WorkspaceProfile{Languages: []string{"Go"}} // no JS -> native lane
	files, err := GenerateDevcontainer(prof, []string{"claude-code"})
	if err != nil {
		t.Fatalf("GenerateDevcontainer: %v", err)
	}
	var parsed struct {
		OnCreateCommand string `json:"onCreateCommand"`
	}
	if err := json.Unmarshal([]byte(files[genDevcontainerPath]), &parsed); err != nil {
		t.Fatalf("devcontainer.json not valid JSON: %v", err)
	}
	want := genAgentToolInstall([]string{"claude-code"}, false)
	if parsed.OnCreateCommand != want {
		t.Errorf("onCreateCommand = %q, want %q", parsed.OnCreateCommand, want)
	}
	if !strings.Contains(parsed.OnCreateCommand, "downloads.claude.ai") {
		t.Errorf("native lane must hit downloads.claude.ai, never a guessed URL: %s", parsed.OnCreateCommand)
	}
}

// TestAgentToolsForIntegrationTypes pins the prefix mapping: anthropic_* ->
// claude-code, openai_* -> codex-cli, everything else (including the OTHER
// ai_provider types, bedrock/azure_openai) contributes nothing — this
// decides what gets BAKED, a narrower question than run-time auth
// compatibility. Sorted + deduped.
func TestAgentToolsForIntegrationTypes(t *testing.T) {
	cases := []struct {
		name  string
		types []string
		want  []string
	}{
		{name: "nil input -> nil", types: nil, want: nil},
		{name: "anthropic api key", types: []string{"anthropic_api_key"}, want: []string{"claude-code"}},
		{name: "anthropic subscription", types: []string{"anthropic_subscription"}, want: []string{"claude-code"}},
		{name: "openai api key", types: []string{"openai_api_key"}, want: []string{"codex-cli"}},
		{name: "bedrock unmapped", types: []string{"bedrock"}, want: nil},
		{name: "azure_openai unmapped", types: []string{"azure_openai"}, want: nil},
		{name: "unknown type contributes nothing", types: []string{"github_app"}, want: nil},
		{
			name:  "both providers named -> sorted",
			types: []string{"openai_api_key", "anthropic_api_key"},
			want:  []string{"claude-code", "codex-cli"},
		},
		{
			name:  "two anthropic rows dedupe to one tool",
			types: []string{"anthropic_api_key", "anthropic_subscription"},
			want:  []string{"claude-code"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AgentToolsForIntegrationTypes(tc.types)
			if len(got) != len(tc.want) {
				t.Fatalf("AgentToolsForIntegrationTypes(%v) = %v, want %v", tc.types, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("AgentToolsForIntegrationTypes(%v) = %v, want %v", tc.types, got, tc.want)
				}
			}
		})
	}
}

// TestGenAgentToolInstall pins genAgentToolInstall directly: the npm-vs-native
// lane choice, and the core honesty assertion — codex-cli is silently DROPPED
// (never a guessed URL) when hasNode is false.
func TestGenAgentToolInstall(t *testing.T) {
	t.Run("no tools -> empty", func(t *testing.T) {
		if got := genAgentToolInstall(nil, true); got != "" {
			t.Errorf("genAgentToolInstall(nil, true) = %q, want empty", got)
		}
	})
	t.Run("claude-code, hasNode -> npm", func(t *testing.T) {
		want := "set -eu\nnpm install -g @anthropic-ai/claude-code"
		if got := genAgentToolInstall([]string{"claude-code"}, true); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("claude-code, no node -> checksum-verified native lane", func(t *testing.T) {
		got := genAgentToolInstall([]string{"claude-code"}, false)
		if !strings.HasPrefix(got, "set -eu\n") {
			t.Errorf("must still fail the build closed on a bad install: %q", got)
		}
		for _, want := range []string{"downloads.claude.ai", "sha256sum -c", "chmod 0755 /usr/local/bin/claude"} {
			if !strings.Contains(got, want) {
				t.Errorf("native lane missing %q: %s", want, got)
			}
		}
	})
	t.Run("codex-cli, hasNode -> npm", func(t *testing.T) {
		want := "set -eu\nnpm install -g @openai/codex"
		if got := genAgentToolInstall([]string{"codex-cli"}, true); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("codex-cli, no node -> dropped, never a guessed URL", func(t *testing.T) {
		if got := genAgentToolInstall([]string{"codex-cli"}, false); got != "" {
			t.Errorf("codex-cli has no verified native-download contract; got %q, want empty (dropped, not guessed)", got)
		}
	})
	t.Run("both tools, no node -> codex-cli dropped, claude-code native survives", func(t *testing.T) {
		got := genAgentToolInstall([]string{"claude-code", "codex-cli"}, false)
		if !strings.Contains(got, "downloads.claude.ai") {
			t.Errorf("claude-code's native lane must still run: %s", got)
		}
		if strings.Contains(got, "@openai/codex") {
			t.Errorf("codex-cli must not appear when it cannot be verified-installed: %s", got)
		}
	})
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
