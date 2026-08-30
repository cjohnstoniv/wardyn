// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestGenerateDevcontainer_ProfilesToJSON asserts a couple of profiles produce
// the exact expected devcontainer.json (base image + one feature per
// feature-supported language, features sorted deterministically) plus the
// generated Dockerfile every GENERATED devcontainer now carries: the standard
// agent-tool install (genStandardTools) is unconditional, so devcontainer.json
// always points `build.dockerfile` at it rather than naming `image` directly.
func TestGenerateDevcontainer_ProfilesToJSON(t *testing.T) {
	cases := []struct {
		name string
		prof WorkspaceProfile
		want string
	}{
		{
			name: "no languages -> no features key",
			prof: WorkspaceProfile{},
			want: `{
  "build": {
    "dockerfile": "Dockerfile"
  }
}
`,
		},
		{
			name: "single language",
			prof: WorkspaceProfile{Languages: []string{"Go"}},
			want: `{
  "build": {
    "dockerfile": "Dockerfile"
  },
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
  "build": {
    "dockerfile": "Dockerfile"
  },
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
  "build": {
    "dockerfile": "Dockerfile"
  },
  "features": {
    "ghcr.io/devcontainers/features/go:1": {}
  }
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := GenerateDevcontainer(tc.prof)
			if err != nil {
				t.Fatalf("GenerateDevcontainer: %v", err)
			}
			got, ok := files[genDevcontainerPath]
			if !ok {
				t.Fatalf("missing %s in output; got keys %v", genDevcontainerPath, slices.Sorted(maps.Keys(files)))
			}
			if df := files[genDockerfilePath]; !strings.Contains(df, "FROM "+genBaseImage) {
				t.Errorf("expected a generated Dockerfile FROM the base image, got %q", df)
			}
			if len(files) != 2 {
				t.Errorf("expected exactly 2 generated files (devcontainer.json + Dockerfile), got %d: %v", len(files), slices.Sorted(maps.Keys(files)))
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
	a, err := GenerateDevcontainer(WorkspaceProfile{Languages: []string{"Python", "Go", "Rust"}})
	if err != nil {
		t.Fatalf("GenerateDevcontainer a: %v", err)
	}
	b, err := GenerateDevcontainer(WorkspaceProfile{Languages: []string{"Rust", "Go", "Python"}})
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
// install fails the whole build. The only keys we emit are build/features/
// containerEnv.
func TestGenerateDevcontainer_NoLifecycleCommand(t *testing.T) {
	files, err := GenerateDevcontainer(WorkspaceProfile{Languages: []string{"JavaScript"}})
	if err != nil {
		t.Fatalf("GenerateDevcontainer: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(files[genDevcontainerPath]), &keys); err != nil {
		t.Fatalf("devcontainer.json not valid JSON: %v", err)
	}
	for _, banned := range []string{"onCreateCommand", "updateContentCommand", "postCreateCommand", "postStartCommand"} {
		if _, bad := keys[banned]; bad {
			t.Errorf("emitted %q: a lifecycle hook bakes nothing into the pushed image", banned)
		}
	}
}

// TestGenAgentToolInstalls pins the bake table itself: claude-code resolves to
// the checksum-verified native lane, and codex-cli resolves to nothing at all —
// the honesty rule (no verified download contract, so no guessed URL). It also
// pins that the table and genStandardTools agree, which is what keeps the
// unconditional bake from ever naming a tool it cannot actually install.
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
	for _, tool := range genStandardTools {
		if _, ok := genAgentToolInstalls[tool]; !ok {
			t.Errorf("genStandardTools names %q, which genAgentToolInstalls cannot install", tool)
		}
	}
}

// TestGenAgentToolDockerfile pins the emitted Dockerfile: it FROMs the same
// base a no-tools devcontainer names directly, and every RUN leads with
// "set -eu" so a failed install fails the BUILD rather than shipping an image
// that claims a tool it does not carry. genAgentToolDockerfile itself stays a
// general tools->Dockerfile mechanism (genStandardTools is just its one
// caller today), so it's still exercised directly here rather than only
// through GenerateDevcontainer.
func TestGenAgentToolDockerfile(t *testing.T) {
	if got := genAgentToolDockerfile(nil, ""); got != "" {
		t.Errorf("no tools must emit no Dockerfile, got %q", got)
	}
	if got := genAgentToolDockerfile([]string{"codex-cli"}, ""); got != "" {
		t.Errorf("nothing bakeable must emit no Dockerfile, got %q", got)
	}
	df := genAgentToolDockerfile([]string{"claude-code"}, "")
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
