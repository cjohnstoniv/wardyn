// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

// Image generation: turn a derived WorkspaceProfile into a minimal
// .devcontainer/devcontainer.json a later envbuilder pass builds into a
// per-workspace image. PURE + DETERMINISTIC — same profile, byte-identical
// output — so results can be profile-hashed and cache-keyed.
//
// The generated devcontainer only promises fields envbuilder actually
// consumes: a base (`image`, or `build.dockerfile`) plus a `features` object
// of official ghcr.io/devcontainers/features/* refs, one per detected
// language; languages with no official feature are left off and surface via
// NeedsReview.
//
// The standard agent tool set (genStandardTools) is baked UNCONDITIONALLY via
// a generated .devcontainer/Dockerfile, not a lifecycle hook or the vendor's
// own devcontainer feature: a lifecycle hook runs AFTER DoBuild+DoPush so
// nothing it does reaches the pushed image, and runs as the unprivileged
// remoteUser; the vendor feature needs `installsAfter`/
// `overrideFeatureInstallOrder`, which envbuilder implements neither of, so it
// installed out of order and hard-failed the build. A Dockerfile RUN has
// neither problem: kaniko snapshots it, it runs as root, before any feature,
// inside the hardened build container.
//
// Symbols here are gen-prefixed to stay clear of this package's other files
// (ai.go, scan.go, markers.go, profile.go).

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
)

// genDevcontainerPath is the single file GenerateDevcontainer emits, at the
// canonical location envbuilder discovers by default.
const genDevcontainerPath = ".devcontainer/devcontainer.json"

// genDockerfilePath carries the standard-tooling RUN layers, always emitted
// beside devcontainer.json (also its `build` context).
const genDockerfilePath = ".devcontainer/Dockerfile"

// EnvAsCodeDockerfilePath exports genDockerfilePath so writeEnvAsCode
// (internal/api/workspace_envcode.go) can refuse to clobber a PRE-EXISTING
// hand-authored Dockerfile at this path — the one emitted file that must not
// be silently overwritten.
const EnvAsCodeDockerfilePath = genDockerfilePath

// genBaseImage is the universal devcontainer base; toolchains layer on as
// features rather than swapping the base, keeping output deterministic.
const genBaseImage = "mcr.microsoft.com/devcontainers/base:ubuntu"

// genLangFeatures maps a WorkspaceProfile.Languages value to its official
// devcontainers feature ref; absent languages contribute nothing. Pinned to
// major tag (":1") for reproducible builds.
var genLangFeatures = map[string]string{
	"Go":         "ghcr.io/devcontainers/features/go:1",
	"JavaScript": "ghcr.io/devcontainers/features/node:1",
	"Python":     "ghcr.io/devcontainers/features/python:1",
	"Rust":       "ghcr.io/devcontainers/features/rust:1",
	"Ruby":       "ghcr.io/devcontainers/features/ruby:1",
	"Java":       "ghcr.io/devcontainers/features/java:1",
	"C#":         "ghcr.io/devcontainers/features/dotnet:1",
	"PHP":        "ghcr.io/devcontainers/features/php:1",
	"Terraform":  "ghcr.io/devcontainers/features/terraform:1",
}

// featuresFor builds the devcontainer.json Features map for detected
// languages with an official feature; shared by EmitEnvAsCode and
// GenerateDevcontainer. Agent CLIs do not ride here — see the package comment.
func featuresFor(langs []string) map[string]map[string]any {
	features := map[string]map[string]any{}
	for _, lang := range langs {
		if ref, ok := genLangFeatures[lang]; ok {
			features[ref] = map[string]any{} // "{}" = feature with default options
		}
	}
	return features
}

// genAgentToolInstalls maps a genStandardTools entry to the Dockerfile RUN
// body that bakes that agent CLI in. Whole bake surface: anything absent here
// is not bakeable, and genStandardTools must not name it.
//
// claude-code's is the checksum-verified native-binary lane (same
// downloads.claude.ai + manifest.json sha256 pattern as
// deploy/images/claude-code/Dockerfile's CLAUDE_INSTALL=native), flattened to
// one RUN (kaniko supports no heredoc), reading arch from
// `dpkg --print-architecture`. Needs only curl+dpkg+coreutils, so it has no
// feature dependency — this RUN runs BEFORE any feature and envbuilder can't
// order the two.
//
// codex-cli is deliberately absent: no verified native-download contract, and
// its npm lane needs a Node runtime this stage doesn't have.
//
// RUN gets no environment replacement from the builder, so the $plat/$ver/$sum
// shell variables below reach /bin/sh verbatim.
var genAgentToolInstalls = map[string]string{
	"claude-code": `case "$(dpkg --print-architecture)" in \
      amd64) plat=linux-x64 ;; \
      arm64) plat=linux-arm64 ;; \
      *) echo "unsupported architecture for native claude-code install" >&2; exit 1 ;; \
    esac; \
    base=https://downloads.claude.ai/claude-code-releases; \
    ver="$(curl -fsSL "$base/stable")"; \
    curl -fsSL "$base/$ver/manifest.json" -o /tmp/claude-manifest.json; \
    sum="$(grep -A3 "\"$plat\"" /tmp/claude-manifest.json | grep -oiE '[a-f0-9]{64}' | head -1)"; \
    [ -n "$sum" ] || { echo "no sha256 for $plat in $ver manifest" >&2; exit 1; }; \
    curl -fsSL "$base/$ver/$plat/claude" -o /usr/local/bin/claude; \
    echo "$sum  /usr/local/bin/claude" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/claude; \
    rm -f /tmp/claude-manifest.json`,
}

// genStandardTools is the standard agent-tool set baked into EVERY generated
// devcontainer unconditionally, regardless of which integrations a workspace
// names. Today just claude-code.
var genStandardTools = []string{"claude-code"}

// genAgentToolDockerfile returns the .devcontainer/Dockerfile that bakes tools
// into the image, or "" when nothing is bakeable (callers then leave
// devcontainer.json on the plain `image` base). baseImage is the FROM line;
// "" falls back to genBaseImage. Every RUN leads with "set -eu" so a failed
// install fails the build rather than shipping an image missing a tool.
func genAgentToolDockerfile(tools []string, baseImage string) string {
	var b strings.Builder
	for _, t := range tools {
		if body, ok := genAgentToolInstalls[t]; ok {
			b.WriteString("RUN set -eu; \\\n    " + body + "\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	if baseImage == "" {
		baseImage = genBaseImage
	}
	return "FROM " + baseImage + "\n\n" + b.String()
}

// genDevcontainer is the minimal devcontainer.json shape emitted. Struct field
// order controls JSON key order; map keys are sorted by encoding/json, so the
// document is deterministic. ContainerEnv is populated only by EmitEnvAsCode
// (GenerateDevcontainer leaves it unset — dispatch's sandboxEnv covers it).
//
// Image and Build are mutually exclusive, both omitempty: nothing to bake
// names the base image directly; something to bake points at the generated
// Dockerfile.
//
// Deliberately NO lifecycle-command field: postCreateCommand would auto-run a
// scan-DETECTED, never-verified SetupCommand unattended (those stay
// prose-only in AGENTS.md); onCreateCommand can't bake into the delivered
// image and runs as the unprivileged remoteUser.
type genDevcontainer struct {
	Image        string                    `json:"image,omitempty"`
	Build        *genBuild                 `json:"build,omitempty"`
	Features     map[string]map[string]any `json:"features,omitempty"`
	ContainerEnv map[string]string         `json:"containerEnv,omitempty"`
}

// genBuild is devcontainer.json's `build` block. No `context` key: it defaults
// to the folder holding devcontainer.json, where genDockerfilePath lives.
type genBuild struct {
	Dockerfile string `json:"dockerfile"`
}

// baseOrBuild points a devcontainer at either the resolved base image or the
// generated Dockerfile baking genStandardTools in, and returns the extra
// files to emit alongside it. baseRef is the workspace's own resolved base
// ("" for "recommended"/nil, keeping genBaseImage) — without it EmitEnvAsCode
// would describe the generic base regardless of what Wardyn actually boots
// (WSPIPE-9). GenerateDevcontainer always passes "". Shared so the
// committable export and the built image can never drift.
func baseOrBuild(dc *genDevcontainer, baseRef string) map[string]string {
	dockerfile := genAgentToolDockerfile(genStandardTools, baseRef)
	if dockerfile == "" {
		dc.Image = baseRef
		if dc.Image == "" {
			dc.Image = genBaseImage
		}
		return nil
	}
	dc.Build = &genBuild{Dockerfile: "Dockerfile"}
	return map[string]string{genDockerfilePath: dockerfile}
}

// EmitEnvAsCode produces committable environment-as-code from a scanned
// profile: a devcontainer.json (base + language features + standard
// agent-tool install + artifact-registry redirects) and an AGENTS.md
// documenting the detected toolchain and setup commands as prose, for a
// human/agent to run deliberately. Returned as path -> content.
//
// artifactBases maps an artifact ecosystem to the operator's corporate
// registry base URL (URL-ONLY, never a token), already skipping NETWORK-ONLY
// rows; pass nil when no redirect is configured.
//
// baseRef is the workspace's own resolved base-image ref, or "" — see baseOrBuild.
func EmitEnvAsCode(p WorkspaceProfile, artifactBases map[string]string, baseRef string) (map[string]string, error) {
	var dc genDevcontainer
	extra := baseOrBuild(&dc, baseRef)
	if features := featuresFor(p.Languages); len(features) > 0 {
		dc.Features = features
	}
	// GOTMPDIR: sandbox /tmp is noexec and `go test` execs test binaries into
	// $TMPDIR. Workspace-folder-relative so it works under any remoteUser.
	if slices.Contains(p.Languages, "Go") {
		dc.ContainerEnv = map[string]string{"GOTMPDIR": "${containerWorkspaceFolder}/.gotmp"}
	}
	// go rides containerEnv (GOPROXY/GOSUMDB); other ecosystems emit their own
	// config files, merged into the return below.
	artifactFiles, artifactEnv := hostrules.EmitArtifactConfig(artifactBases)
	for k, v := range artifactEnv {
		if dc.ContainerEnv == nil {
			dc.ContainerEnv = map[string]string{}
		}
		dc.ContainerEnv[k] = v
	}
	b, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("workspacescan: marshal devcontainer: %w", err)
	}
	b = append(b, '\n')
	out := map[string]string{
		genDevcontainerPath: string(b),
		"AGENTS.md":         genAgentsMD(p),
	}
	for path, content := range extra {
		out[path] = content
	}
	for path, content := range artifactFiles {
		out[path] = content
	}
	return out, nil
}

// genAgentsMD documents the detected environment + setup commands in the
// emerging AGENTS.md convention. Setup commands (p.SetupCommands) are a
// scan-time heuristic, never verified, so they stay prose for a human/agent
// to review and run deliberately, never auto-executed.
func genAgentsMD(p WorkspaceProfile) string {
	var b strings.Builder
	b.WriteString("# AGENTS.md\n\n")
	b.WriteString("Environment generated by Wardyn's workspace import.\n\n")
	if len(p.Languages) > 0 {
		b.WriteString("## Languages\n\n" + strings.Join(p.Languages, ", ") + "\n\n")
	}
	if len(p.PackageManagers) > 0 {
		b.WriteString("## Package managers\n\n" + strings.Join(p.PackageManagers, ", ") + "\n\n")
	}
	if len(p.SetupCommands) > 0 {
		b.WriteString("## Detected setup commands (not verified — review before running)\n\n")
		for _, stage := range []string{"install", "build", "test", "lint"} {
			for _, c := range p.SetupCommands {
				if c.Stage == stage {
					b.WriteString("- **" + stage + "**: `" + c.Command + "`\n")
				}
			}
		}
		b.WriteString("\n")
	}
	if len(p.ServicesNeeded) > 0 {
		b.WriteString("## Backing services\n\n" + strings.Join(p.ServicesNeeded, ", ") + "\n\n")
	}
	// Fixes Wardyn's own sandbox applies at dispatch time that this exported
	// devcontainer can't fully replicate outside Wardyn.
	if slices.Contains(p.Languages, "Go") || slices.Contains(p.PackageManagers, "maven") {
		b.WriteString("## Environment fidelity notes\n\n")
		if slices.Contains(p.Languages, "Go") {
			b.WriteString("- **GOTMPDIR** is set in `containerEnv` (`go test` compiles+execs test binaries into " +
				"$TMPDIR; some sandboxes mount `/tmp` noexec). Create the directory if your tooling doesn't " +
				"auto-create it: `mkdir -p $GOTMPDIR`.\n")
		}
		if slices.Contains(p.PackageManagers, "maven") {
			b.WriteString("- **Maven proxy**: inside a Wardyn-governed run, the platform points Maven at the " +
				"in-sandbox proxy via `MAVEN_OPTS` (Maven alone ignores `HTTP_PROXY`/`HTTPS_PROXY`). Outside " +
				"Wardyn, configure your own `~/.m2/settings.xml` `<proxy>`/`<mirror>` if `mvn` can't reach " +
				"repo.maven.apache.org.\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// GenerateDevcontainer produces a minimal, deterministic
// .devcontainer/devcontainer.json for the profile: universal base image plus
// one feature per detected language plus the standard agent-tool install,
// baked unconditionally via an always-emitted .devcontainer/Dockerfile.
// Returned map is path -> content, safe to feed straight to the envbuilder
// local-context build.
//
// Pure: no I/O, no clock, no randomness. p.Languages is already sorted+deduped
// by DeriveProfile, so output bytes are identical for identical profiles.
func GenerateDevcontainer(p WorkspaceProfile) (files map[string]string, err error) {
	var dc genDevcontainer
	// Always "": reached only for the recommended/derived build, which has no
	// other base to resolve.
	out := baseOrBuild(&dc, "")
	if features := featuresFor(p.Languages); len(features) > 0 {
		dc.Features = features
	}

	b, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		// Unreachable for this struct; signature stays honest regardless.
		return nil, fmt.Errorf("workspacescan: marshal devcontainer: %w", err)
	}
	b = append(b, '\n')
	if out == nil {
		out = map[string]string{}
	}
	out[genDevcontainerPath] = string(b)
	return out, nil
}
