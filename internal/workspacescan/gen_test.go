// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import "testing"

// TestGenerateDevcontainer_ProfilesToJSON asserts a couple of profiles produce
// the exact expected devcontainer.json (base image + one feature per
// feature-supported language, features sorted deterministically).
func TestGenerateDevcontainer_ProfilesToJSON(t *testing.T) {
	cases := []struct {
		name string
		prof WorkspaceProfile
		want string
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := GenerateDevcontainer(tc.prof)
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

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
