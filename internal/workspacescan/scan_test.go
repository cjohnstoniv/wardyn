// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
)

// writeFile creates dir/rel (slash-separated) with the given content,
// creating parent directories as needed.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func eq(t *testing.T, label string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

type wantProfile struct {
	languages       []string
	packageManagers []string
	egressDomains   []string
	tools           []string
	hasDevcontainer bool
	hasDockerfile   bool
	needsReview     bool
	confidence      string
}

func TestScan(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  wantProfile
	}{
		{
			name: "go module",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "go.mod", "module example.com/foo\n\ngo 1.22\n")
			},
			want: wantProfile{
				languages:       []string{"Go"},
				packageManagers: []string{"go"},
				egressDomains:   []string{"proxy.golang.org", "sum.golang.org"},
				confidence:      ConfidenceHigh,
			},
		},
		{
			// Gotcha: go.mod can carry both a `go` and a `toolchain` directive
			// (toolchain should win if we ever extract a version). We don't
			// track a version in WorkspaceProfile, so we never read go.mod's
			// content at all — presence alone drives detection, and this
			// case proves the directive choice can't affect the result.
			name: "go module with both go and toolchain directives",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "go.mod", "module example.com/foo\n\ngo 1.21\ntoolchain go1.22.1\n")
			},
			want: wantProfile{
				languages:       []string{"Go"},
				packageManagers: []string{"go"},
				egressDomains:   []string{"proxy.golang.org", "sum.golang.org"},
				confidence:      ConfidenceHigh,
			},
		},
		{
			name: "node + pnpm",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "package.json", `{"name":"x"}`)
				writeFile(t, dir, "pnpm-lock.yaml", "lockfileVersion: '6.0'\n")
			},
			want: wantProfile{
				languages:       []string{"JavaScript"},
				packageManagers: []string{"pnpm"},
				egressDomains:   []string{"registry.npmjs.org"},
				confidence:      ConfidenceHigh,
			},
		},
		{
			// Gotcha: yarn Berry's config is .yarnrc.yml, NOT .npmrc. No
			// .npmrc is written here at all — yarn must still be detected.
			name: "yarn berry uses .yarnrc.yml, not .npmrc",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "package.json", `{"name":"x"}`)
				writeFile(t, dir, "yarn.lock", "# yarn lockfile v1\n")
				writeFile(t, dir, ".yarnrc.yml", "nodeLinker: node-modules\n")
			},
			want: wantProfile{
				languages:       []string{"JavaScript"},
				packageManagers: []string{"yarn"},
				egressDomains:   []string{"registry.npmjs.org"},
				confidence:      ConfidenceHigh,
			},
		},
		{
			name: "python + poetry",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "pyproject.toml", "[tool.poetry]\nname = \"x\"\n")
				writeFile(t, dir, "poetry.lock", "# generated\n")
			},
			want: wantProfile{
				languages:       []string{"Python"},
				packageManagers: []string{"poetry"},
				egressDomains:   []string{"files.pythonhosted.org", "pypi.org"},
				confidence:      ConfidenceHigh,
			},
		},
		{
			// Gotcha: uv doesn't suppress other Python package-manager
			// markers — this is a set union, not a precedence override.
			name: "python requirements.txt and uv.lock coexist (set union)",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "requirements.txt", "flask==3.0\n")
				writeFile(t, dir, "uv.lock", "version = 1\n")
			},
			want: wantProfile{
				languages:       []string{"Python"},
				packageManagers: []string{"pip", "uv"},
				egressDomains:   []string{"files.pythonhosted.org", "pypi.org"},
				confidence:      ConfidenceHigh,
			},
		},
		{
			// Gotcha: devcontainer.json is JSONC (comments allowed). Wave 1
			// only needs PRESENCE detection, so a comment in the body must
			// not break anything — we never parse the JSON at all.
			name: "devcontainer presence (JSONC comment in body)",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, ".devcontainer/devcontainer.json",
					"{\n  // a comment — real JSON would reject this\n  \"image\": \"mcr.microsoft.com/devcontainers/go\"\n}\n")
			},
			want: wantProfile{
				tools:           []string{"devcontainer"},
				hasDevcontainer: true,
				confidence:      ConfidenceHigh,
			},
		},
		{
			// Gotcha: docker-compose is version-less under Compose v2 — no
			// top-level `version:` key. We never look for one.
			name: "dockerfile + version-less docker-compose",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "Dockerfile", "FROM golang:1.22\n")
				writeFile(t, dir, "docker-compose.yml", "services:\n  web:\n    build: .\n")
			},
			want: wantProfile{
				tools:         []string{"docker", "docker-compose"},
				hasDockerfile: true,
				confidence:    ConfidenceHigh,
			},
		},
		{
			// Gotcha: flake.nix/shell.nix/Tiltfile/Earthfile are not
			// statically parseable — presence + NeedsReview, no semantic
			// guessing.
			name: "nix flake — recognized but unresolved",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "flake.nix", "{ outputs = { ... }: {}; }\n")
			},
			want: wantProfile{
				tools:       []string{"nix"},
				needsReview: true,
				confidence:  ConfidenceMedium,
			},
		},
		{
			// Gotcha: .tool-versions lines can use path:/ref:/system in the
			// version field instead of a plain version. We never read the
			// version field for language detection (no Version field on the
			// profile yet), so these variants must not break presence
			// detection of the pin file itself.
			name: ".tool-versions path/ref/system variants don't break detection",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, ".tool-versions", "nodejs path:./vendor/node\nruby ref:v3.2.0\npython system\n")
			},
			want: wantProfile{
				tools:      []string{"asdf"},
				confidence: ConfidenceHigh,
			},
		},
		{
			name: "unrecognized build system (CMake) — low confidence + review",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "CMakeLists.txt", "cmake_minimum_required(VERSION 3.20)\n")
			},
			want: wantProfile{
				needsReview: true,
				confidence:  ConfidenceLow,
			},
		},
		{
			name: "node_modules is excluded from the walk",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "package.json", `{"name":"x"}`)
				writeFile(t, dir, "package-lock.json", `{"lockfileVersion":3}`)
				writeFile(t, dir, "node_modules/some-dep/package.json", `{"name":"some-dep"}`)
			},
			want: wantProfile{
				languages:       []string{"JavaScript"},
				packageManagers: []string{"npm"},
				egressDomains:   []string{"registry.npmjs.org"},
				confidence:      ConfidenceHigh,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			got := Scan(dir)
			eq(t, "Languages", got.Languages, tc.want.languages)
			eq(t, "PackageManagers", got.PackageManagers, tc.want.packageManagers)
			eq(t, "EgressDomains", got.EgressDomains, tc.want.egressDomains)
			eq(t, "Tools", got.Tools, tc.want.tools)
			if got.HasDevcontainer != tc.want.hasDevcontainer {
				t.Errorf("HasDevcontainer = %v, want %v", got.HasDevcontainer, tc.want.hasDevcontainer)
			}
			if got.HasDockerfile != tc.want.hasDockerfile {
				t.Errorf("HasDockerfile = %v, want %v", got.HasDockerfile, tc.want.hasDockerfile)
			}
			if got.NeedsReview != tc.want.needsReview {
				t.Errorf("NeedsReview = %v, want %v", got.NeedsReview, tc.want.needsReview)
			}
			if got.Confidence != tc.want.confidence {
				t.Errorf("Confidence = %v, want %v", got.Confidence, tc.want.confidence)
			}
			if got.Source != SourceDeterministic {
				t.Errorf("Source = %v, want %v", got.Source, SourceDeterministic)
			}
		})
	}
}

func TestScan_GitRemotes(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = https://github.com/acme/web.git\n" +
		"[remote \"mirror\"]\n\turl = git@gitlab.com:acme/web.git\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Scan(dir)
	eq(t, "GitRemotes.GitHub", got.GitRemotes.GitHub, []string{"acme/web"})
	eq(t, "GitRemotes.OtherHosts", got.GitRemotes.OtherHosts, []string{"gitlab.com"})
}

func TestScan_DoesNotFollowSymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module x\n\ngo 1.22\n")
	outside := t.TempDir()
	writeFile(t, outside, "package.json", `{"name":"evil"}`)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got := Scan(root)
	if slices.Contains(got.Languages, "JavaScript") {
		t.Errorf("symlinked dir was followed: Languages = %v", got.Languages)
	}
	eq(t, "Languages", got.Languages, []string{"Go"})
}

func TestScan_DepthCap(t *testing.T) {
	root := t.TempDir()
	deep := root
	for i := 0; i < maxDepth+2; i++ {
		deep = filepath.Join(deep, "d")
	}
	writeFile(t, deep, "go.mod", "module x\n\ngo 1.22\n")
	got := Scan(root)
	if slices.Contains(got.Languages, "Go") {
		t.Errorf("a manifest below the depth cap should be skipped, got Languages = %v", got.Languages)
	}
}

func TestScan_ManifestCapTruncatesToLowConfidence(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxManifestHits+5; i++ {
		writeFile(t, root, fmt.Sprintf("pkg%d/go.mod", i), "module x\n\ngo 1.22\n")
	}
	got := Scan(root)
	if got.Confidence != ConfidenceLow || !got.NeedsReview {
		t.Errorf("hitting the manifest cap should force low confidence + NeedsReview, got Confidence=%v NeedsReview=%v",
			got.Confidence, got.NeedsReview)
	}
}

func TestDeriveProfile_UnknownMarkerIgnoredFailSafe(t *testing.T) {
	facts := ScanFacts{ManifestsFound: []ManifestHit{{Path: "weird.cfg", Marker: "not-a-real-marker-id"}}}
	got := DeriveProfile(facts)
	if got.Confidence != ConfidenceHigh || got.NeedsReview {
		t.Errorf("an unknown marker id should be ignored, not crash or lower confidence: %+v", got)
	}
	if len(got.Languages) != 0 || len(got.PackageManagers) != 0 {
		t.Errorf("an unknown marker id should contribute nothing: %+v", got)
	}
}

func TestProfileHash(t *testing.T) {
	p1 := WorkspaceProfile{Languages: []string{"Go"}, Confidence: ConfidenceHigh, Source: SourceDeterministic}
	p2 := WorkspaceProfile{Languages: []string{"Go"}, Confidence: ConfidenceHigh, Source: SourceDeterministic}
	if p1.ProfileHash() != p2.ProfileHash() {
		t.Error("identical profiles produced different hashes")
	}
	p3 := WorkspaceProfile{Languages: []string{"Python"}, Confidence: ConfidenceHigh, Source: SourceDeterministic}
	if p1.ProfileHash() == p3.ProfileHash() {
		t.Error("different profiles produced the same hash")
	}
	if got := len(p1.ProfileHash()); got != 64 {
		t.Errorf("hash length = %d, want 64 (sha256 hex)", got)
	}
}
