// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

// This file is the marker → meaning DATA table implementing the Tier 0-5
// detection ladder. Every row is keyed on a FILENAME (or a small path/suffix
// pattern below) — egress hosts and package-manager/language claims are only
// ever attached here, never derived from a file's content, so a hostile
// manifest body can never inject an egress host.
//
// Fidelity notes (deliberate Wave-1 scope limits):
//   - Tier 0: PRESENCE only — the JSONC body (image/build/features) isn't
//     parsed; that's a later wave (image generation, gen.go).
//   - Tier 1: presence only. Compose is matched without checking `version:`
//     — Compose v2 parses version-less files fine.
//   - Tier 2 declarative: presence + tool hint, no language claim (no
//     TOML/YAML parser here). The Nix/DSL family isn't statically
//     parseable, so those rows set unresolved=true, forcing NeedsReview
//     regardless of what else was found.
//   - Tier 3 toolchain pins: single-language pin files need no content
//     parsing — the FILENAME implies the language. Multi-language pin files
//     (.tool-versions, mise.toml) are presence-only: parsing them would need
//     a small parser, added later if empty Languages on a pin-only repo
//     becomes a real complaint.
//   - Tier 4 manifests carry the load-bearing language/package-manager/egress
//     claims. Credential/custom-registry files are egress-affecting but
//     never a language/package-manager claim, and their secret values are
//     never read.
//   - Tier 5 CI is corroborating only: presence + tool hint, no
//     language/package-manager/egress claim.

// Tier 4 egress hosts, named once so the table below stays typo-proof.
var (
	egressNPM   = []string{"registry.npmjs.org"}
	egressPyPI  = []string{"pypi.org", "files.pythonhosted.org"}
	egressGo    = []string{"proxy.golang.org", "sum.golang.org"}
	egressCargo = []string{"crates.io", "static.crates.io", "index.crates.io"}
	// repo1.maven.org is Central's canonical host; many builds hit it directly.
	egressMaven    = []string{"repo.maven.apache.org", "repo1.maven.org"}
	egressGradle   = []string{"repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org"}
	egressGem      = []string{"rubygems.org"}
	egressComposer = []string{"repo.packagist.org"}
	egressNuGet    = []string{"api.nuget.org"}
	egressHex      = []string{"hex.pm"}
	egressPub      = []string{"pub.dev"}
)

// marker is one filename/path-pattern → meaning row.
type marker struct {
	id              string // canonical marker id == ManifestHit.Marker
	languages       []string
	packageManagers []string
	egress          []string
	tools           []string
	isDevcontainer  bool
	isDockerfile    bool
	// unresolved marks a recognized-but-not-statically-parseable format
	// (Nix/Tilt/Earthly): always forces NeedsReview when matched.
	unresolved bool
}

// Canonical ids for the rows matched by path/suffix pattern rather than a
// bare filename (see lookupMarker in scan.go).
const (
	idDevcontainerNested = ".devcontainer/devcontainer.json"
	idCargoConfig        = ".cargo/config.toml"
	idGithubWorkflow     = ".github/workflows/*.yml"
	idGemspec            = "*.gemspec"
	idDotnetProj         = "*.csproj"
	idGradleWrapper      = "gradle/wrapper/gradle-wrapper.properties"
	idMavenWrapper       = ".mvn/wrapper/maven-wrapper.properties"
	// Sphinx's conf.py is matched by the docs/conf.py path suffix, never as a
	// bare filename — any Python project may have a conf.py, but docs/conf.py is
	// the Sphinx convention.
	idSphinxConf = "docs/conf.py"
)

// markerTable is the Tier 0-5 detection ladder. lookupMarker (scan.go)
// resolves a walked file to one row; DeriveProfile (scan.go) walks
// ScanFacts.ManifestsFound and looks rows back up by id via markersByID.
var markerTable = []marker{
	// --- Tier 0: devcontainer (authoritative) ---
	{id: ".devcontainer.json", isDevcontainer: true, tools: []string{"devcontainer"}},
	{id: idDevcontainerNested, isDevcontainer: true, tools: []string{"devcontainer"}},

	// --- Tier 1: Dockerfile / Containerfile / compose ---
	{id: "Dockerfile", isDockerfile: true, tools: []string{"docker"}},
	{id: "Containerfile", isDockerfile: true, tools: []string{"podman"}},
	{id: "docker-compose.yml", isDockerfile: true, tools: []string{"docker-compose"}},
	{id: "docker-compose.yaml", isDockerfile: true, tools: []string{"docker-compose"}},
	{id: "compose.yml", isDockerfile: true, tools: []string{"docker-compose"}},
	{id: "compose.yaml", isDockerfile: true, tools: []string{"docker-compose"}},

	// --- Tier 2: declarative, partial (presence + tool hint only) ---
	{id: "project.toml", tools: []string{"buildpacks"}},
	{id: "devbox.json", tools: []string{"devbox"}},
	{id: "devenv.yaml", tools: []string{"devenv"}},
	{id: ".gitpod.yml", tools: []string{"gitpod"}},
	{id: "okteto.yml", tools: []string{"okteto"}},
	{id: "skaffold.yaml", tools: []string{"skaffold"}},
	{id: ".envrc", tools: []string{"direnv"}},
	// Nix/DSL: not statically parseable — presence + always NeedsReview.
	{id: "flake.nix", tools: []string{"nix"}, unresolved: true},
	{id: "shell.nix", tools: []string{"nix"}, unresolved: true},
	{id: "devenv.nix", tools: []string{"nix", "devenv"}, unresolved: true},
	{id: "Tiltfile", tools: []string{"tilt"}, unresolved: true},
	{id: "Earthfile", tools: []string{"earthly"}, unresolved: true},

	// --- Tier 3: toolchain pins (filename alone implies the language) ---
	{id: ".tool-versions", tools: []string{"asdf"}},
	{id: "mise.toml", tools: []string{"mise"}},
	{id: ".mise.toml", tools: []string{"mise"}},
	{id: ".nvmrc", languages: []string{"JavaScript"}, tools: []string{"nvm"}},
	{id: ".node-version", languages: []string{"JavaScript"}, tools: []string{"node-version-pin"}},
	{id: ".python-version", languages: []string{"Python"}, tools: []string{"pyenv"}},
	{id: "runtime.txt", languages: []string{"Python"}, tools: []string{"pyenv"}},
	{id: ".ruby-version", languages: []string{"Ruby"}, tools: []string{"rbenv"}},
	{id: "rust-toolchain.toml", languages: []string{"Rust"}, tools: []string{"rustup"}},
	{id: "rust-toolchain", languages: []string{"Rust"}, tools: []string{"rustup"}},
	{id: ".sdkmanrc", languages: []string{"Java"}, tools: []string{"sdkman"}},
	{id: ".java-version", languages: []string{"Java"}, tools: []string{"jenv"}},
	{id: "global.json", languages: []string{"C#"}, tools: []string{"dotnet-sdk-pin"}},
	{id: ".php-version", languages: []string{"PHP"}, tools: []string{"phpenv"}},
	{id: ".terraform-version", languages: []string{"Terraform"}, tools: []string{"tfenv"}},

	// --- Tier 4: manifests + the fixed language/registry egress map ---
	// npm/yarn/pnpm (JavaScript). yarn Berry uses .yarnrc.yml, NOT .npmrc.
	{id: "package.json", languages: []string{"JavaScript"}},
	{id: "package-lock.json", languages: []string{"JavaScript"}, packageManagers: []string{"npm"}, egress: egressNPM},
	{id: "yarn.lock", languages: []string{"JavaScript"}, packageManagers: []string{"yarn"}, egress: egressNPM},
	{id: ".yarnrc.yml", languages: []string{"JavaScript"}, packageManagers: []string{"yarn"}, egress: egressNPM},
	{id: ".npmrc", egress: egressNPM},
	{id: "pnpm-lock.yaml", languages: []string{"JavaScript"}, packageManagers: []string{"pnpm"}, egress: egressNPM},

	// go
	{id: "go.mod", languages: []string{"Go"}, packageManagers: []string{"go"}, egress: egressGo},
	{id: "go.sum", languages: []string{"Go"}, packageManagers: []string{"go"}, egress: egressGo},

	// cargo (Rust)
	{id: "Cargo.toml", languages: []string{"Rust"}, packageManagers: []string{"cargo"}, egress: egressCargo},
	{id: "Cargo.lock", languages: []string{"Rust"}, packageManagers: []string{"cargo"}, egress: egressCargo},
	{id: idCargoConfig, egress: egressCargo},

	// maven / gradle (Java). Wrapper-properties rows match by path suffix
	// (lookupMarker): a wrapper self-downloads its build tool on first run,
	// so its distribution host is a first-build egress need even with no
	// host tool required.
	{id: idGradleWrapper, egress: []string{"services.gradle.org"}, tools: []string{"gradle-wrapper"}},
	{id: idMavenWrapper, egress: egressMaven, tools: []string{"maven-wrapper"}},
	{id: "pom.xml", languages: []string{"Java"}, packageManagers: []string{"maven"}, egress: egressMaven},
	{id: "build.gradle", languages: []string{"Java"}, packageManagers: []string{"gradle"}, egress: egressGradle},
	{id: "build.gradle.kts", languages: []string{"Java"}, packageManagers: []string{"gradle"}, egress: egressGradle},
	{id: "settings.gradle", languages: []string{"Java"}, packageManagers: []string{"gradle"}, egress: egressGradle},
	{id: "settings.gradle.kts", languages: []string{"Java"}, packageManagers: []string{"gradle"}, egress: egressGradle},
	{id: "settings.xml", egress: egressMaven},

	// gem / bundler (Ruby)
	{id: "Gemfile", languages: []string{"Ruby"}, packageManagers: []string{"gem"}, egress: egressGem},
	{id: "Gemfile.lock", languages: []string{"Ruby"}, packageManagers: []string{"gem"}, egress: egressGem},
	{id: idGemspec, languages: []string{"Ruby"}, packageManagers: []string{"gem"}, egress: egressGem},

	// composer (PHP)
	{id: "composer.json", languages: []string{"PHP"}, packageManagers: []string{"composer"}, egress: egressComposer},
	{id: "composer.lock", languages: []string{"PHP"}, packageManagers: []string{"composer"}, egress: egressComposer},

	// nuget (.NET / C#)
	{id: idDotnetProj, languages: []string{"C#"}, packageManagers: []string{"nuget"}, egress: egressNuGet},
	{id: "packages.config", languages: []string{"C#"}, packageManagers: []string{"nuget"}, egress: egressNuGet},
	{id: "nuget.config", egress: egressNuGet},

	// hex (Elixir)
	{id: "mix.exs", languages: []string{"Elixir"}, packageManagers: []string{"hex"}, egress: egressHex},
	{id: "mix.lock", languages: []string{"Elixir"}, packageManagers: []string{"hex"}, egress: egressHex},

	// pub (Dart)
	{id: "pubspec.yaml", languages: []string{"Dart"}, packageManagers: []string{"pub"}, egress: egressPub},
	{id: "pubspec.lock", languages: []string{"Dart"}, packageManagers: []string{"pub"}, egress: egressPub},

	// pip/poetry/pdm/pipenv/uv (Python) share one registry; each file
	// independently contributes its own package manager (set union, no
	// precedence) — requirements.txt + uv.lock yields [pip,uv].
	{id: "requirements.txt", languages: []string{"Python"}, packageManagers: []string{"pip"}, egress: egressPyPI},
	{id: "Pipfile", languages: []string{"Python"}, packageManagers: []string{"pipenv"}, egress: egressPyPI},
	{id: "Pipfile.lock", languages: []string{"Python"}, packageManagers: []string{"pipenv"}, egress: egressPyPI},
	// pyproject.toml alone is ambiguous among poetry/pdm/uv/setuptools —
	// claim language + egress, but no package manager unless a lockfile says.
	{id: "pyproject.toml", languages: []string{"Python"}, egress: egressPyPI},
	{id: "poetry.lock", languages: []string{"Python"}, packageManagers: []string{"poetry"}, egress: egressPyPI},
	{id: "pdm.lock", languages: []string{"Python"}, packageManagers: []string{"pdm"}, egress: egressPyPI},
	{id: "uv.lock", languages: []string{"Python"}, packageManagers: []string{"uv"}, egress: egressPyPI},
	{id: "pip.conf", egress: egressPyPI},
	{id: ".pypirc", egress: egressPyPI},

	// --- Docs generators: tool hint only (no language/egress claim). The tool
	// feeds a fixed docs-build SetupCommand template (deriveSetupCommands).
	// Docusaurus rides the JS branch instead of its own template. Hugo
	// detection is hugo.toml only; legacy config.toml sites are missed
	// rather than false-positived (too generic).
	{id: "mkdocs.yml", tools: []string{"mkdocs"}},
	{id: "mkdocs.yaml", tools: []string{"mkdocs"}},
	{id: idSphinxConf, tools: []string{"sphinx"}},
	{id: "hugo.toml", tools: []string{"hugo"}},
	{id: "docusaurus.config.js", tools: []string{"docusaurus"}},
	{id: "docusaurus.config.ts", tools: []string{"docusaurus"}},

	// --- Tier 5: CI, corroborating only (no language/package-manager/egress claim) ---
	{id: idGithubWorkflow, tools: []string{"github-actions"}},
	{id: ".gitlab-ci.yml", tools: []string{"gitlab-ci"}},
}

// markersByID indexes markerTable by id for both forward lookup (an exact
// filename match) and DeriveProfile's reverse lookup from a ManifestHit.Marker.
var markersByID = func() map[string]marker {
	m := make(map[string]marker, len(markerTable))
	for _, row := range markerTable {
		m[row.id] = row
	}
	return m
}()

// unmappedBuildFiles are filenames that are clearly a build/dependency
// descriptor but fall outside the egress table above (no safe registry
// mapping, or outside Wave-1 scope). Seeing one captures bounded evidence
// (UnrecognizedSamples, for a later fallback) and marks the profile
// low-confidence + NeedsReview.
//
// Makefile is deliberately EXCLUDED: a common convenience wrapper unrelated
// to the primary language/package manager — flagging it would make
// NeedsReview noise, not signal.
var unmappedBuildFiles = map[string]struct{}{
	"environment.yml":  {},
	"environment.yaml": {}, // conda
	"setup.py":         {},
	"setup.cfg":        {}, // legacy python setuptools, no lockfile-driven registry claim
	"CMakeLists.txt":   {},
	"meson.build":      {}, // C/C++
	"WORKSPACE":        {},
	"WORKSPACE.bazel":  {},
	"BUILD.bazel":      {}, // bazel
	"Podfile":          {}, // CocoaPods
	"conanfile.txt":    {},
	"conanfile.py":     {}, // Conan (C/C++)
	"shard.yml":        {}, // Crystal
	"stack.yaml":       {},
	"cabal.project":    {}, // Haskell
}
