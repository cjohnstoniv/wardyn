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
// consumes: a base (`image`, or a `build.dockerfile` naming the generated
// Dockerfile) plus a `features` object of official ghcr.io/devcontainers/features/*
// refs (cross-checked against envbuilder's devcontainer-spec-support). One
// feature is selected per detected language; languages with no official core
// feature (Elixir, Dart, ...) are simply left off the image and surface via
// the profile's NeedsReview elsewhere.
//
// The agent CLI an integration implies is baked by a generated
// .devcontainer/Dockerfile (genAgentToolDockerfile) that devcontainer.json
// points `build.dockerfile` at, so its RUN steps are layers of the image
// kaniko builds and PUSHES. Two mechanisms were tried and rejected against a
// real build, in this order:
//
//   - a devcontainer lifecycle hook (onCreate/postCreate): envbuilder runs
//     lifecycle scripts AFTER executor.DoBuild + DoPush, so nothing they do
//     reaches the image Wardyn delivers (the pushed base, which stage 2 wraps
//     FROM — internal/envbuild/builder.go finalizeImage, docs/ENVBUILD.md
//     "Two-stage build"), and the runner boots that finalized tag directly
//     with no devcontainer CLI, so they never run at run time either. They
//     also run as the base image's unprivileged remoteUser, where a
//     root-needing install fails the whole build.
//   - the tool vendor's own devcontainer FEATURE
//     (ghcr.io/anthropics/devcontainer-features/claude-code): correct
//     mechanism, wrong engine. That feature is an npm install and needs the
//     node feature beside it; envbuilder implements NEITHER `installsAfter`
//     nor `overrideFeatureInstallOrder` (verified against the binary), and it
//     ran claude-code's install.sh before node's, which hard-FAILED the build
//     ("Node.js and npm are required but could not be installed"). Feature
//     ordering is not a contract this engine offers, so a feature that
//     depends on another feature cannot be used here.
//
// A Dockerfile RUN has neither problem: kaniko snapshots it into the pushed
// image, it runs as root, it runs BEFORE any feature (so it depends on none),
// and it stays inside the hardened, resource-capped build container rather
// than on the host daemon (which is why this is not done in stage-2 finalize,
// whose whole trust story is "FROM + COPY, no untrusted RUN").
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

// genDockerfilePath is the OPTIONAL second file: emitted only when something
// has to be baked in with a RUN (genAgentToolDockerfile). It sits beside
// devcontainer.json, which is also the build context devcontainer.json's
// `build` block resolves `dockerfile` against.
const genDockerfilePath = ".devcontainer/Dockerfile"

// EnvAsCodeDockerfilePath exports genDockerfilePath for callers outside this
// package that need to single the generated Dockerfile out from
// EmitEnvAsCode's output — namely internal/api/workspaces.go's
// writeEnvAsCode, which refuses to overwrite a PRE-EXISTING file at this path.
// Unlike every other emitted key (devcontainer.json/AGENTS.md/the artifact
// redirect stubs, all Wardyn's own narrow, regenerate-on-demand output), a
// Dockerfile at .devcontainer/Dockerfile is exactly where an operator would
// already have hand-authored their own, for reasons that have nothing to do
// with Wardyn — so it is the one emitted file "write into the directory" must
// not silently clobber.
const EnvAsCodeDockerfilePath = genDockerfilePath

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
// GenerateDevcontainer. Agent CLIs do NOT ride here — see the package comment
// for why a feature cannot bake one on this engine.
func featuresFor(langs []string) map[string]map[string]any {
	features := map[string]map[string]any{}
	for _, lang := range langs {
		if ref, ok := genLangFeatures[lang]; ok {
			features[ref] = map[string]any{} // "{}" = feature with default options
		}
	}
	return features
}

// genAgentToolInstalls maps an AgentToolsForIntegrationTypes value to the
// Dockerfile RUN body that BAKES that agent CLI in. This table is the whole
// bake surface: anything absent from it is not bakeable, and
// AgentToolsForIntegrationTypes must therefore not name it.
//
// claude-code's is the checksum-verified native-binary lane — the same
// downloads.claude.ai + manifest.json sha256 pattern as
// deploy/images/claude-code/Dockerfile's CLAUDE_INSTALL=native RUN block,
// flattened onto one RUN (kaniko supports no heredoc) and reading the
// architecture from `dpkg --print-architecture` rather than a buildx
// TARGETARCH. "stable" resolves to a concrete version exactly as that
// Dockerfile's default ARG CLAUDE_CODE_VERSION=stable does. It needs only
// curl + dpkg + coreutils from the base image, so it depends on no
// devcontainer feature — which matters, because this RUN executes BEFORE any
// feature and envbuilder offers no way to order the two. Reachable because
// the generated devcontainer only ever builds with
// WARDYN_ENVBUILD_BUILD_NETWORK=host (deploy/compose); air-gapped is a
// documented limitation, not special-cased here.
//
// codex-cli is deliberately absent: it has NO Wardyn-verified public
// native-download contract (deploy/images/codex-cli/Dockerfile: native install
// is staged-only, "fails loudly... rather than guessing a URL"), and its npm
// lane would need a Node runtime this stage does not have. So it is not
// bakeable today — the image is built without it, never with a guessed URL or
// a false claim that it is there.
//
// Dockerfile note: RUN is not one of the instructions the builder performs
// environment replacement on, so the $plat/$ver/$sum shell variables below
// reach /bin/sh verbatim.
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

// genAgentToolDockerfile returns the .devcontainer/Dockerfile that bakes tools
// into the image, or "" when nothing in tools is bakeable (including the
// no-tools case) — callers must then emit no Dockerfile at all and leave
// devcontainer.json on the plain `image` base. baseImage is the FROM line;
// "" falls back to genBaseImage (WSPIPE-9 — a non-recommended base pick's own
// image, not the universal convention one, when baseOrBuild's caller names it).
//
// Every RUN leads with "set -eu" so a failed install fails the BUILD rather
// than silently producing an image that claims a tool it does not carry: that
// inventory honesty is the whole point of this feature.
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
//
// The result is exactly the set genAgentToolInstalls can INSTALL, never a
// wider "requested" set: this value is both the generator's input and (via
// WorkspaceProfile.CacheKey) the built-image cache key, so a tool the
// generator would silently drop must not appear here either — it would key a
// rebuild that reproduces a byte-identical devcontainer. That is why
// "openai_*" contributes nothing: codex-cli has no bakeable install lane (see
// genAgentToolInstalls).
func AgentToolsForIntegrationTypes(integrationTypes []string) []string {
	seen := map[string]bool{}
	for _, t := range integrationTypes {
		if strings.HasPrefix(t, "anthropic_") {
			seen["claude-code"] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(seen))
}

// genDevcontainer is the minimal devcontainer.json shape we emit. Struct field
// order controls JSON key order (image or build, then features, then
// containerEnv); the features/containerEnv maps' own keys are sorted by
// encoding/json, so the whole document is deterministic. ContainerEnv is only
// populated by EmitEnvAsCode (GenerateDevcontainer leaves it unset — the
// envbuilder-built image's runs already get the fidelity env from dispatch's
// sandboxEnv, so it would be a no-op there).
//
// Image and Build are mutually exclusive, which is why both are omitempty:
// with nothing to bake we name the base image directly, and with something to
// bake we point at the generated Dockerfile (whose own FROM is that same base).
//
// There is deliberately NO lifecycle-command field. postCreateCommand was
// always refused (a scan-DETECTED, never-verified SetupCommand must not
// auto-run unattended — those stay prose-only in AGENTS.md, genAgentsMD), and
// onCreateCommand is refused too: it cannot bake anything into the image
// Wardyn delivers, and it would run as the base image's unprivileged
// remoteUser, where a root-needing install fails the whole build (see the
// package comment).
type genDevcontainer struct {
	Image        string                    `json:"image,omitempty"`
	Build        *genBuild                 `json:"build,omitempty"`
	Features     map[string]map[string]any `json:"features,omitempty"`
	ContainerEnv map[string]string         `json:"containerEnv,omitempty"`
}

// genBuild is devcontainer.json's `build` block, naming the Dockerfile beside
// devcontainer.json. No `context` key: it defaults to the folder holding
// devcontainer.json, which is where genDockerfilePath already puts it.
type genBuild struct {
	Dockerfile string `json:"dockerfile"`
}

// baseOrBuild points a devcontainer at either the resolved base image or the
// generated Dockerfile (whose own FROM is that same base), and returns the
// extra files to emit alongside it. baseRef is the workspace's OWN resolved
// base (a registry/custom/byo pick's Image; "" for "recommended"/nil, which
// keeps the universal genBaseImage default) — WITHOUT it, EmitEnvAsCode
// described the generic devcontainer base regardless of what Wardyn actually
// boots for that workspace (WSPIPE-9). GenerateDevcontainer is reached only
// for the recommended/derived build, so it always passes "".
// Shared by EmitEnvAsCode and GenerateDevcontainer so the committable export
// and the image Wardyn builds can never drift on what they carry.
func baseOrBuild(dc *genDevcontainer, tools []string, baseRef string) map[string]string {
	dockerfile := genAgentToolDockerfile(tools, baseRef)
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
// the workspace's named integrations) — see genAgentToolInstalls for the
// bake-lane rule; pass nil when nothing is named.
//
// baseRef is the workspace's OWN resolved base-image ref (a registry/custom/
// byo pick's Image), or "" for "recommended"/nil — see baseOrBuild.
func EmitEnvAsCode(p WorkspaceProfile, artifactBases map[string]string, tools []string, baseRef string) (map[string]string, error) {
	var dc genDevcontainer
	extra := baseOrBuild(&dc, tools, baseRef)
	if features := featuresFor(p.Languages); len(features) > 0 {
		dc.Features = features
	}
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
	for path, content := range extra {
		out[path] = content
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

// GenerateDevcontainer produces a minimal, deterministic
// .devcontainer/devcontainer.json for the profile: the universal base image
// plus one devcontainer feature per detected, feature-supported language. When
// tools names something bakeable, a .devcontainer/Dockerfile is emitted
// alongside it and devcontainer.json points `build.dockerfile` at that instead
// of naming the base directly (see genAgentToolInstalls). The returned map is
// path -> file content; it is safe to feed straight to the envbuilder
// local-context build (BuildFromDevcontainerFiles).
//
// Pure: no I/O, no clock, no randomness. p.Languages is already sorted+deduped
// by DeriveProfile, so iterating it and letting encoding/json sort the features
// map yields identical bytes for identical profiles. tools is
// AgentToolsForIntegrationTypes' output; pass nil when nothing is named.
func GenerateDevcontainer(p WorkspaceProfile, tools []string) (files map[string]string, err error) {
	var dc genDevcontainer
	// Always "" (the universal genBaseImage default): this path is reached
	// only for the recommended/derived build (resolveWorkspaceImage falls
	// through to it precisely when the workspace names no explicit base
	// image), which by definition has no OTHER base to resolve.
	out := baseOrBuild(&dc, tools, "")
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
	if out == nil {
		out = map[string]string{}
	}
	out[genDevcontainerPath] = string(b)
	return out, nil
}
