// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

// Image generation (A5): turn a derived WorkspaceProfile into a minimal
// .devcontainer/devcontainer.json that a later envbuilder pass can build into a
// per-workspace image. This file is PURE + DETERMINISTIC — the same profile
// always produces byte-identical output — so its result can be profile-hashed
// and cache-keyed (Workspace.BuiltProfileHash, a later wave).
//
// The generated devcontainer only ever promises fields envbuilder actually
// consumes: a base `image` plus a `features` object of official
// ghcr.io/devcontainers/features/* refs (cross-checked against envbuilder's
// devcontainer-spec-support). One feature is selected per detected language;
// languages with no official core feature (Elixir, Dart, ...) are simply left
// off the image and surface via the profile's NeedsReview elsewhere.
//
// Symbols here are gen-prefixed to stay clear of the other new files in this
// package (ai.go); they never touch scan.go/markers.go/profile.go.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// genDevcontainerPath is the single file GenerateDevcontainer emits, at the
// canonical location envbuilder discovers by default (no ENVBUILDER_DEVCONTAINER_DIR
// needed).
const genDevcontainerPath = ".devcontainer/devcontainer.json"

// genBaseImage is the universal devcontainer base. Language toolchains are
// layered on as features rather than by swapping the base, which keeps the
// output deterministic and additive regardless of how many languages a profile
// detects.
const genBaseImage = "mcr.microsoft.com/devcontainers/base:ubuntu"

// genLangFeatures maps a WorkspaceProfile.Languages value (the exact strings
// markers.go emits) to its official devcontainers feature ref. Languages absent
// here have no official core feature and contribute no feature. Pinned to the
// features' major tag (":1") so builds are reproducible without chasing latest.
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

// featuresFor builds the devcontainer.json Features map for the detected
// languages that have an official feature (genLangFeatures); languages
// without one contribute nothing. Shared by EmitEnvAsCode and
// GenerateDevcontainer.
func featuresFor(langs []string) map[string]map[string]any {
	features := map[string]map[string]any{}
	for _, lang := range langs {
		if ref, ok := genLangFeatures[lang]; ok {
			features[ref] = map[string]any{} // "{}" = feature with default options
		}
	}
	return features
}

// genDevcontainer is the minimal devcontainer.json shape we emit. Struct field
// order controls JSON key order (image, features, containerEnv, then
// postCreateCommand); the features/containerEnv maps' own keys are sorted by
// encoding/json, so the whole document is deterministic. ContainerEnv is only
// populated by EmitEnvAsCode (GenerateDevcontainer leaves it unset — the
// envbuilder-built image's runs already get the fidelity env from
// dispatchWithVerify's sandboxEnv, so it would be a no-op there).
type genDevcontainer struct {
	Image             string                    `json:"image"`
	Features          map[string]map[string]any `json:"features,omitempty"`
	ContainerEnv      map[string]string         `json:"containerEnv,omitempty"`
	PostCreateCommand string                    `json:"postCreateCommand,omitempty"`
}

// EmitEnvAsCode produces committable environment-as-code from a VERIFIED
// profile + its operator-approved setup commands: a devcontainer.json (base +
// language features + the install/build steps as postCreateCommand) and an
// AGENTS.md documenting the detected toolchain and setup commands. Returned as
// path -> content. The install/build stages become postCreateCommand (env
// setup); test/lint are documented in AGENTS.md but not auto-run on create.
//
// artifactBases maps an artifact ecosystem (npm|pip|cargo|maven|go|nuget) to the
// operator's corporate registry base URL (from the persisted site-config,
// URL-ONLY — never a token). When non-empty, the matching per-tool config files
// (and go's containerEnv) are merged in so a committed workspace pulls from the
// corporate mirror; pass nil when no redirect is configured.
func EmitEnvAsCode(p WorkspaceProfile, approved []SetupCommand, artifactBases map[string]string) (map[string]string, error) {
	dc := genDevcontainer{Image: genBaseImage}
	if features := featuresFor(p.Languages); len(features) > 0 {
		dc.Features = features
	}
	// GOTMPDIR: dispatchWithVerify's sandboxEnv (runs.go) sets this for every
	// Wardyn-governed run because the sandbox /tmp is noexec and `go test`
	// compiles+execs its test binaries into $TMPDIR. Workspace-folder-relative
	// (not a home-dir guess) so it works under any base image's remoteUser; a
	// Go workspace built from this exported env-as-code keeps the fix even
	// outside a Wardyn sandbox. See the AGENTS.md note for the mkdir caveat.
	if slices.Contains(p.Languages, "Go") {
		dc.ContainerEnv = map[string]string{"GOTMPDIR": "${containerWorkspaceFolder}/.gotmp"}
	}
	// Artifact-redirect config: go rides containerEnv (GOPROXY/GOSUMDB); the
	// other ecosystems emit their own config files (merged into the return below).
	artifactFiles, artifactEnv := EmitArtifactConfig(artifactBases)
	for k, v := range artifactEnv {
		if dc.ContainerEnv == nil {
			dc.ContainerEnv = map[string]string{}
		}
		dc.ContainerEnv[k] = v
	}
	var setup []string
	for _, c := range approved {
		if c.Stage == "install" || c.Stage == "build" {
			setup = append(setup, c.Command)
		}
	}
	if len(setup) > 0 {
		dc.PostCreateCommand = strings.Join(setup, " && ")
	}
	b, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("workspacescan: marshal devcontainer: %w", err)
	}
	b = append(b, '\n')
	out := map[string]string{
		genDevcontainerPath: string(b),
		"AGENTS.md":         genAgentsMD(p, approved),
	}
	for path, content := range artifactFiles {
		out[path] = content
	}
	return out, nil
}

// artifactEcosystems is EmitArtifactConfig's deterministic emit order and the
// closed set of ecosystems it can configure (mirrors api.validArtifactEcosystems;
// kept in sync by the shared R5 findings).
var artifactEcosystems = []string{"npm", "pip", "cargo", "maven", "go", "nuget"}

// EmitArtifactConfig turns operator-configured artifact-registry redirects
// (ecosystem -> corporate base URL) into the per-tool config each toolchain reads
// to pull from the corporate mirror instead of the public registry. Returns
// (files, env):
//   - files: path -> content, keyed by each tool's real config location relative
//     to HOME (npm .npmrc, pip .config/pip/pip.conf, cargo .cargo/config.toml,
//     maven .m2/settings.xml, nuget .nuget/NuGet/NuGet.Config). A dispatch-time
//     writer drops them under $HOME; a committable export drops the
//     repo-cascading ones (.npmrc/.cargo) usefully at the repo root and the
//     rest as documentation.
//   - env: the go-toolchain variables (go redirects via GOPROXY/GOSUMDB env, not
//     a file).
//
// The output is URL-ONLY and carries NO secret — an injected registry token is
// applied proxy-side, never written into a committable/readable config file.
// Maven's settings.xml is intentionally MIRRORS-ONLY: the sandbox reaches the
// mirror THROUGH wardyn-proxy via MAVEN_OPTS (set platform-wide at dispatch), so
// no <proxies> block is emitted here — which also keeps a committed settings.xml
// free of the sandbox-only wardyn-proxy hostname (mirrors=which-URL is additive
// to proxies=how-to-reach, which lives in MAVEN_OPTS). GOPRIVATE is deliberately
// NOT set: GOPRIVATE="*" would route modules to direct VCS and defeat the corp
// GOPROXY, and GOSUMDB=off already disables the checksum DB the corp proxy may
// not serve. Pure + deterministic; unknown/empty ecosystems are skipped.
//
// Injection safety: base URLs come from site-config, which validateSiteConfig
// already rejects if they contain control chars or shell/XML metacharacters
// (`$;&|<>"'\), so embedding base verbatim into TOML/XML/ini here is safe.
func EmitArtifactConfig(bases map[string]string) (files map[string]string, env map[string]string) {
	files = map[string]string{}
	env = map[string]string{}
	for _, eco := range artifactEcosystems {
		base := strings.TrimSpace(bases[eco])
		if base == "" {
			continue
		}
		switch eco {
		case "npm":
			files[".npmrc"] = "registry=" + base + "\n"
		case "pip":
			files[".config/pip/pip.conf"] = "[global]\nindex-url = " + base + "\n"
		case "cargo":
			files[".cargo/config.toml"] = "[source.crates-io]\nreplace-with = \"corp\"\n\n" +
				"[registries.corp]\nindex = \"sparse+" + base + "\"\n"
		case "maven":
			files[".m2/settings.xml"] = mavenMirrorSettings(base)
		case "go":
			env["GOPROXY"] = base
			env["GOSUMDB"] = "off"
		case "nuget":
			files[".nuget/NuGet/NuGet.Config"] = nugetConfig(base)
		}
	}
	if len(files) == 0 {
		files = nil
	}
	if len(env) == 0 {
		env = nil
	}
	return files, env
}

// mavenMirrorSettings is a self-contained ~/.m2/settings.xml with a single
// mirror-of-* pointing at the corporate base URL. No <servers> credentials
// (token injection is proxy-side) and no <proxies> (MAVEN_OPTS carries the
// how-to-reach at dispatch).
func mavenMirrorSettings(base string) string {
	return `<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">
  <mirrors>
    <mirror>
      <id>corp</id>
      <name>Corporate Artifact Mirror</name>
      <mirrorOf>*</mirrorOf>
      <url>` + base + `</url>
    </mirror>
  </mirrors>
</settings>
`
}

// nugetConfig is a ~/.nuget/NuGet/NuGet.Config that clears the default public
// source and adds the corporate feed (no <packageSourceCredentials> — token
// injection is proxy-side).
func nugetConfig(base string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="corp" value="` + base + `" />
  </packageSources>
</configuration>
`
}

// genAgentsMD documents the detected environment + setup commands in the
// emerging AGENTS.md convention, so an agent (Wardyn's or a competitor's) knows
// how to build/test the repo.
func genAgentsMD(p WorkspaceProfile, approved []SetupCommand) string {
	var b strings.Builder
	b.WriteString("# AGENTS.md\n\n")
	b.WriteString("Environment generated by Wardyn's verified workspace import.\n\n")
	if len(p.Languages) > 0 {
		b.WriteString("## Languages\n\n" + strings.Join(p.Languages, ", ") + "\n\n")
	}
	if len(p.PackageManagers) > 0 {
		b.WriteString("## Package managers\n\n" + strings.Join(p.PackageManagers, ", ") + "\n\n")
	}
	if len(approved) > 0 {
		b.WriteString("## Setup commands (verified working)\n\n")
		for _, stage := range []string{"install", "build", "test", "lint"} {
			for _, c := range approved {
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
	// Environment fidelity notes: the toolchain-fidelity fixes Wardyn's own
	// sandbox applies at dispatch time (runs.go sandboxEnv) that this exported
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

// GenerateDevcontainer produces a minimal, deterministic .devcontainer/devcontainer.json
// for the profile: the universal base image plus one official devcontainer
// feature per detected, feature-supported language. The returned map is
// path -> file content (a single entry); it is safe to feed straight to the
// envbuilder local-context build (BuildFromDevcontainerFiles).
//
// Pure: no I/O, no clock, no randomness. p.Languages is already sorted+deduped
// by DeriveProfile, so iterating it and letting encoding/json sort the features
// map yields identical bytes for identical profiles.
func GenerateDevcontainer(p WorkspaceProfile) (files map[string]string, err error) {
	dc := genDevcontainer{Image: genBaseImage}
	if features := featuresFor(p.Languages); len(features) > 0 {
		dc.Features = features
	}

	b, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		// Unreachable for this struct, but the signature is honest and callers
		// get an error rather than a silently-empty context.
		return nil, fmt.Errorf("workspacescan: marshal devcontainer: %w", err)
	}
	b = append(b, '\n')
	return map[string]string{genDevcontainerPath: string(b)}, nil
}
