// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

// Image generation: turn a derived WorkspaceProfile into a minimal
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
	"maps"
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

// AgentToolsForIntegrationTypes derives the agent CLIs implied by naming
// integration TYPES (types.Integration.Type values, e.g. "anthropic_api_key"/
// "anthropic_subscription"/"openai_api_key") on a workspace, so its
// recommended build can bake them in — inventory honesty: "Carries:
// claude-code" becomes true only once the server actually bakes it, never the
// reverse. Prefix-only and deliberately narrow: "bedrock"/"azure_openai" and
// every other unrecognized type stay unmapped and contribute nothing — this
// decides what gen.go BAKES into an image, a different question from
// run-time auth compatibility (ui/lib/integrations.ts's canDriveClaudeCode
// says bedrock CAN drive an already-baked claude-code at run time — not
// whether the image should bake one for it). Sorted + deduped so the result
// is a stable devcontainer/cache-key input regardless of call order.
func AgentToolsForIntegrationTypes(integrationTypes []string) []string {
	seen := map[string]bool{}
	for _, t := range integrationTypes {
		switch {
		case strings.HasPrefix(t, "anthropic_"):
			seen["claude-code"] = true
		case strings.HasPrefix(t, "openai_"):
			seen["codex-cli"] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(seen))
}

// claudeCodeNativeInstall is the checksum-verified native-binary install lane
// for claude-code when no npm/Node runtime is in the profile — the same
// downloads.claude.ai + manifest.json sha256 pattern as
// deploy/images/claude-code/Dockerfile's CLAUDE_INSTALL=native RUN block,
// adapted for a devcontainer onCreateCommand: a plain shell script, so the
// Dockerfile's host-staged-binary branch (needs a build-context COPY) drops
// out, and architecture comes from `dpkg --print-architecture` at container
// run time rather than a buildx TARGETARCH. "stable" resolves to a concrete
// version the same way the Dockerfile's default ARG CLAUDE_CODE_VERSION=stable
// does. Reachable because the generated devcontainer only ever builds with
// WARDYN_ENVBUILD_BUILD_NETWORK=host (deploy/compose); air-gapped is a
// documented limitation, not special-cased here.
const claudeCodeNativeInstall = `case "$(dpkg --print-architecture)" in
  amd64) plat=linux-x64 ;;
  arm64) plat=linux-arm64 ;;
  *) echo "unsupported architecture for native claude-code install" >&2; exit 1 ;;
esac
base=https://downloads.claude.ai/claude-code-releases
ver="$(curl -fsSL "$base/stable")"
curl -fsSL "$base/$ver/manifest.json" -o /tmp/claude-manifest.json
sum="$(grep -A3 "\"$plat\"" /tmp/claude-manifest.json | grep -oiE '[a-f0-9]{64}' | head -1)"
[ -n "$sum" ] || { echo "no sha256 for $plat in $ver manifest" >&2; exit 1; }
curl -fsSL "$base/$ver/$plat/claude" -o /usr/local/bin/claude
echo "$sum  /usr/local/bin/claude" | sha256sum -c -
chmod 0755 /usr/local/bin/claude
rm -f /tmp/claude-manifest.json`

// genAgentToolInstall returns the onCreateCommand shell script that installs
// tools (AgentToolsForIntegrationTypes' output) into the image being built,
// so "Carries: X" is true of the image rather than a run-time credential with
// nothing to authenticate. hasNode picks npm (fast, uses the ecosystem the
// profile's own Node feature already installs) over the native lane, keyed on
// the SAME signal genLangFeatures uses for the node feature
// (slices.Contains(p.Languages, "JavaScript")).
//
// codex-cli has NO Wardyn-verified public native-download contract
// (deploy/images/codex-cli/Dockerfile: native install is staged-only, "fails
// loudly... rather than guessing a URL"). When hasNode is false, codex-cli is
// silently DROPPED from the script rather than guessing a URL — the image is
// simply built without it, never with a false claim that it's there.
//
// The script starts "set -eu" so a failed install fails the BUILD, not just
// the tool: an image that silently failed to install a tool it claims to
// carry would violate the inventory-honesty this whole feature exists to
// uphold. Returns "" when nothing in tools is installable in this lane
// (including "no tools" and "codex-cli alone with hasNode=false") — callers
// must leave OnCreateCommand unset rather than emit an empty one.
func genAgentToolInstall(tools []string, hasNode bool) string {
	var blocks []string
	for _, t := range tools {
		switch t {
		case "claude-code":
			if hasNode {
				blocks = append(blocks, "npm install -g @anthropic-ai/claude-code")
			} else {
				blocks = append(blocks, claudeCodeNativeInstall)
			}
		case "codex-cli":
			if hasNode {
				blocks = append(blocks, "npm install -g @openai/codex")
			}
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return "set -eu\n" + strings.Join(blocks, "\n")
}

// genDevcontainer is the minimal devcontainer.json shape we emit. Struct field
// order controls JSON key order (image, features, onCreateCommand, then
// containerEnv); the features/containerEnv maps' own keys are sorted by
// encoding/json, so the whole document is deterministic. ContainerEnv is only
// populated by EmitEnvAsCode (GenerateDevcontainer leaves it unset — the
// envbuilder-built image's runs already get the fidelity env from dispatch's
// sandboxEnv, so it would be a no-op there).
//
// OnCreateCommand is a DIFFERENT trust class from postCreateCommand, which we
// still never emit: it runs INSIDE the envbuilder build and is baked into the
// pushed image (docs/ENVBUILD.md "Build sandbox" lists onCreate/updateContent
// alongside Dockerfile RUN as build-time-executed), so it carries ONLY
// genAgentToolInstall's checksum-verified agent-CLI install — never a
// scan-DETECTED, never-verified SetupCommand. Those stay prose-only in
// AGENTS.md (genAgentsMD): Wardyn has no verify step to prove a detected
// install/build command actually works, so nothing UNVERIFIED is ever
// auto-run, unattended, at container create.
type genDevcontainer struct {
	Image           string                    `json:"image"`
	Features        map[string]map[string]any `json:"features,omitempty"`
	OnCreateCommand string                    `json:"onCreateCommand,omitempty"`
	ContainerEnv    map[string]string         `json:"containerEnv,omitempty"`
}

// EmitEnvAsCode produces committable environment-as-code from a scanned
// profile: a devcontainer.json (base + language features + agent-tool
// install + artifact-registry redirects) and an AGENTS.md documenting the
// DETECTED toolchain and setup commands (profile.SetupCommands, a scan-time
// heuristic — never verified) as prose, for a human/agent to run
// deliberately. Returned as path -> content.
//
// artifactBases maps an artifact ecosystem (npm|pip|cargo|maven|go|nuget) to the
// operator's corporate registry base URL (from the persisted site-config,
// URL-ONLY — never a token). The caller (api.artifactBaseURLs) derives this
// from the Ecosystem-tier subset of types.SiteConfig.EgressRedirects, already
// skipping every NETWORK-ONLY row (Ecosystem "") — this function only ever
// sees an ecosystem that actually wants a config file. When non-empty, the
// matching per-tool config files (and go's containerEnv) are merged in so a
// committed workspace pulls from the corporate mirror; pass nil when no
// redirect is configured.
//
// tools is AgentToolsForIntegrationTypes' output (the caller derives it from
// the workspace's named integrations) — see genAgentToolInstall for the
// install-lane rule; pass nil when nothing is named.
func EmitEnvAsCode(p WorkspaceProfile, artifactBases map[string]string, tools []string) (map[string]string, error) {
	dc := genDevcontainer{Image: genBaseImage}
	if features := featuresFor(p.Languages); len(features) > 0 {
		dc.Features = features
	}
	dc.OnCreateCommand = genAgentToolInstall(tools, slices.Contains(p.Languages, "JavaScript"))
	// GOTMPDIR: dispatch's sandboxEnv (runs_dispatch.go) sets this for every
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
	b, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("workspacescan: marshal devcontainer: %w", err)
	}
	b = append(b, '\n')
	out := map[string]string{
		genDevcontainerPath: string(b),
		"AGENTS.md":         genAgentsMD(p),
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
// how to build/test the repo. Setup commands come from p.SetupCommands — a
// scan-time DETECTED heuristic (deriveSetupCommands), never verified — so they
// are prose for a human/agent to review and run deliberately, not something
// this generator or a devcontainer postCreateCommand ever auto-executes.
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
// feature per detected, feature-supported language, plus tools' agent-CLI
// install (see genAgentToolInstall). The returned map is path -> file content
// (a single entry); it is safe to feed straight to the envbuilder
// local-context build (BuildFromDevcontainerFiles).
//
// Pure: no I/O, no clock, no randomness. p.Languages is already sorted+deduped
// by DeriveProfile, so iterating it and letting encoding/json sort the features
// map yields identical bytes for identical profiles. tools is
// AgentToolsForIntegrationTypes' output; pass nil when nothing is named.
func GenerateDevcontainer(p WorkspaceProfile, tools []string) (files map[string]string, err error) {
	dc := genDevcontainer{Image: genBaseImage}
	if features := featuresFor(p.Languages); len(features) > 0 {
		dc.Features = features
	}
	dc.OnCreateCommand = genAgentToolInstall(tools, slices.Contains(p.Languages, "JavaScript"))

	b, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		// Unreachable for this struct, but the signature is honest and callers
		// get an error rather than a silently-empty context.
		return nil, fmt.Errorf("workspacescan: marshal devcontainer: %w", err)
	}
	b = append(b, '\n')
	return map[string]string{genDevcontainerPath: string(b)}, nil
}
