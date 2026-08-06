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
		// wantDockerfile: the tool bake emits a SECOND file beside
		// devcontainer.json (and devcontainer.json then carries `build`, not
		// `image`).
		wantDockerfile bool
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
			// The honesty case: codex-cli has no verified native-download bake
			// lane (genAgentToolInstalls has no "codex-cli" entry), so naming it
			// must bake NOTHING — never a guessed URL or a third-party install
			// script. The output is byte-identical to a plain Go profile with no
			// tools at all, which is exactly why AgentToolsForIntegrationTypes
			// never emits it.
			name:  "codex-cli named -> nothing bakeable, no extra bake",
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
		{
			// The bake: devcontainer.json stops naming the base image and points
			// build.dockerfile at the generated Dockerfile whose RUN kaniko bakes
			// into the image it PUSHES — unlike a lifecycle command, which runs
			// after the push and reaches no delivered layer.
			name:  "claude-code named -> build.dockerfile instead of image, features unchanged",
			prof:  WorkspaceProfile{Languages: []string{"Go"}},
			tools: []string{"claude-code"},
			want: `{
  "build": {
    "dockerfile": "Dockerfile"
  },
  "features": {
    "ghcr.io/devcontainers/features/go:1": {}
  }
}
`,
			wantDockerfile: true,
		},
		{
			name:  "claude-code on a profile with no language features",
			prof:  WorkspaceProfile{},
			tools: []string{"claude-code"},
			want: `{
  "build": {
    "dockerfile": "Dockerfile"
  }
}
`,
			wantDockerfile: true,
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
			wantFiles := 1
			if tc.wantDockerfile {
				wantFiles = 2
				if df := files[genDockerfilePath]; !strings.Contains(df, "FROM "+genBaseImage) {
					t.Errorf("expected a generated Dockerfile FROM the base image, got %q", df)
				}
			}
			if len(files) != wantFiles {
				t.Errorf("expected exactly %d generated file(s), got %d: %v", wantFiles, len(files), keysOf(files))
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

// TestGenerateDevcontainer_NoLifecycleCommand pins the bake MECHANISM: the
// generated devcontainer must never carry a lifecycle command. envbuilder runs
// those AFTER the build+push, so they cannot reach the delivered image, and
// they run as the base image's unprivileged remoteUser, where a root-needing
// install fails the whole build. Whatever the tool set, the only keys we emit
// are image/features/containerEnv.
func TestGenerateDevcontainer_NoLifecycleCommand(t *testing.T) {
	for _, tools := range [][]string{nil, {"claude-code"}, {"codex-cli"}, {"claude-code", "codex-cli"}} {
		files, err := GenerateDevcontainer(WorkspaceProfile{Languages: []string{"JavaScript"}}, tools)
		if err != nil {
			t.Fatalf("GenerateDevcontainer(%v): %v", tools, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(files[genDevcontainerPath]), &keys); err != nil {
			t.Fatalf("devcontainer.json not valid JSON: %v", err)
		}
		for _, banned := range []string{"onCreateCommand", "updateContentCommand", "postCreateCommand", "postStartCommand"} {
			if _, bad := keys[banned]; bad {
				t.Errorf("tools=%v emitted %q: a lifecycle hook bakes nothing into the pushed image", tools, banned)
			}
		}
	}
}

// TestAgentToolsForIntegrationTypes pins the prefix mapping: anthropic_* ->
// claude-code; everything else — openai_* included, because codex-cli has no
// bakeable feature — contributes nothing. The result is what gets BAKED, a
// narrower question than run-time auth compatibility, and it is also the
// image cache key, so it must never name a tool the generator would drop.
// Sorted + deduped.
func TestAgentToolsForIntegrationTypes(t *testing.T) {
	cases := []struct {
		name  string
		types []string
		want  []string
	}{
		{name: "nil input -> nil", types: nil, want: nil},
		{name: "anthropic api key", types: []string{"anthropic_api_key"}, want: []string{"claude-code"}},
		{name: "anthropic subscription", types: []string{"anthropic_subscription"}, want: []string{"claude-code"}},
		{name: "openai api key -> nothing bakeable", types: []string{"openai_api_key"}, want: nil},
		{name: "bedrock unmapped", types: []string{"bedrock"}, want: nil},
		{name: "azure_openai unmapped", types: []string{"azure_openai"}, want: nil},
		{name: "unknown type contributes nothing", types: []string{"github_app"}, want: nil},
		{
			name:  "both providers named -> only the bakeable one",
			types: []string{"openai_api_key", "anthropic_api_key"},
			want:  []string{"claude-code"},
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

// TestGenAgentToolInstalls pins the bake table itself: claude-code resolves to
// the checksum-verified native lane, and codex-cli resolves to nothing at all —
// the honesty rule (no verified download contract, so no guessed URL). It also
// pins that the table and AgentToolsForIntegrationTypes agree, which is what
// keeps the cache key from rebuilding for a tool the generator would drop.
func TestGenAgentToolInstalls(t *testing.T) {
	body, ok := genAgentToolInstalls["claude-code"]
	if !ok {
		t.Fatal("claude-code must have a bake lane")
	}
	for _, want := range []string{"downloads.claude.ai", "sha256sum -c", "chmod 0755 /usr/local/bin/claude"} {
		if !strings.Contains(body, want) {
			t.Errorf("claude-code's lane missing %q: %s", want, body)
		}
	}
	// npm would need a Node runtime this stage does not have — and depending on
	// the node FEATURE is impossible, since envbuilder cannot order a Dockerfile
	// RUN after a feature (it runs strictly before every one of them).
	if strings.Contains(body, "npm install") {
		t.Errorf("the Dockerfile stage runs before every feature, so it cannot depend on npm: %s", body)
	}
	if _, ok := genAgentToolInstalls["codex-cli"]; ok {
		t.Error("codex-cli has no verified native-download contract; it must not be baked from a guessed URL")
	}
	for _, tool := range AgentToolsForIntegrationTypes([]string{"anthropic_api_key", "anthropic_subscription", "openai_api_key", "bedrock"}) {
		if _, ok := genAgentToolInstalls[tool]; !ok {
			t.Errorf("AgentToolsForIntegrationTypes emitted %q, which genAgentToolInstalls cannot install", tool)
		}
	}
}

// TestGenAgentToolDockerfile pins the emitted Dockerfile: it FROMs the same
// base a no-tools devcontainer names directly, and every RUN leads with
// "set -eu" so a failed install fails the BUILD rather than shipping an image
// that claims a tool it does not carry.
func TestGenAgentToolDockerfile(t *testing.T) {
	if got := genAgentToolDockerfile(nil); got != "" {
		t.Errorf("no tools must emit no Dockerfile, got %q", got)
	}
	if got := genAgentToolDockerfile([]string{"codex-cli"}); got != "" {
		t.Errorf("nothing bakeable must emit no Dockerfile, got %q", got)
	}
	df := genAgentToolDockerfile([]string{"claude-code"})
	if !strings.HasPrefix(df, "FROM "+genBaseImage+"\n") {
		t.Errorf("Dockerfile must FROM the same base image: %s", df)
	}
	// Parse it the way a Dockerfile builder does — fold every trailing-backslash
	// continuation into its instruction — and assert the result is exactly the
	// two instructions we meant. A continuation that is not last on its line
	// would leave the next line standing as its own bogus instruction, which is
	// precisely what this catches.
	var instrs []string
	var cur string
	for _, line := range strings.Split(strings.TrimRight(df, "\n"), "\n") {
		cur += strings.TrimSuffix(line, "\\")
		if strings.HasSuffix(line, "\\") {
			continue
		}
		if trimmed := strings.TrimSpace(cur); trimmed != "" {
			instrs = append(instrs, trimmed)
		}
		cur = ""
	}
	if len(instrs) != 2 {
		t.Fatalf("want exactly FROM + one RUN, got %d instructions: %q", len(instrs), instrs)
	}
	if !strings.HasPrefix(instrs[1], "RUN set -eu; ") {
		t.Errorf("every RUN must fail the build closed: %q", instrs[1])
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
