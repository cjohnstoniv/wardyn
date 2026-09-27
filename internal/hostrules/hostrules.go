// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package hostrules holds the host-shape rules and artifact-registry tables
// the RUNTIME governance paths depend on — approval write-back, egress
// substitution, site-config validation, and the artifact-redirect emitter.
// Not scanning: extracted from internal/workspacescan (scheduled for deletion
// in 0.5.x) so these could outlive it.
//
// Leaf package by design (stdlib only), so internal/api and — until it goes
// — internal/workspacescan can both depend on it without a cycle.
package hostrules

import (
	"regexp"
	"strings"
)

// suggestedHostRE is the post-validation charset for an egress host
// (lowercased, port/path already stripped). A dot is required so a bare word
// can't masquerade as a host.
var suggestedHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// ValidApprovedHost reports whether h is a plain lowercase dotted host of the
// exact shape the approved-egress API accepts for operator promotion (no
// scheme, port, path, or wildcard; wildcards remain a policy-allowlist
// privilege).
func ValidApprovedHost(h string) bool {
	return strings.Contains(h, ".") && suggestedHostRE.MatchString(h)
}

// HostOf extracts the lowercase host of an http(s) URL, or "" if unparseable.
func HostOf(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	s = strings.ToLower(s)
	if ValidApprovedHost(s) {
		return s
	}
	return ""
}

// ecosystemPublicHosts maps an artifact ecosystem key (matching
// types.SiteConfig.ArtifactOverrides — npm|pip|go|cargo|maven|nuget) to the
// public-registry hosts a corporate redirect REPLACES. Spelled out here
// rather than reusing workspacescan's marker-table literals: same strings,
// different (soon to diverge) purpose.
//
// maven maps to Central's mirror hosts; plugins.gradle.org is a separate
// concern a mirror override does not touch.
var ecosystemPublicHosts = map[string][]string{
	"npm":   {"registry.npmjs.org"},
	"pip":   {"pypi.org", "files.pythonhosted.org"},
	"go":    {"proxy.golang.org", "sum.golang.org"},
	"cargo": {"crates.io", "static.crates.io", "index.crates.io"},
	"maven": {"repo.maven.apache.org", "repo1.maven.org"}, // repo1 is Central's canonical host
	"nuget": {"api.nuget.org"},
}

// PublicRegistryHosts returns the public-registry hosts a corporate redirect
// replaces for an artifact ecosystem (npm|pip|go|cargo|maven|nuget), or nil for
// an unknown key. The egress-substitution layer drops these and adds the corp
// host when the operator configures a redirect for that ecosystem.
func PublicRegistryHosts(ecosystem string) []string {
	return ecosystemPublicHosts[ecosystem]
}

// artifactEcosystems is EmitArtifactConfig's deterministic emit order and the
// closed set of ecosystems it can configure (mirrors api.validArtifactEcosystems;
// kept in sync by the shared R5 findings).
var artifactEcosystems = []string{"npm", "pip", "cargo", "maven", "go", "nuget"}

// EmitArtifactConfig turns operator-configured artifact-registry redirects
// (ecosystem -> corporate base URL) into the per-tool config each toolchain
// reads to pull from the corporate mirror instead of the public registry.
// Returns files (path -> content, relative to HOME) and env (the go
// toolchain's GOPROXY/GOSUMDB, which has no config file).
//
// Output is URL-ONLY, no secret: a registry token is injected proxy-side,
// never written to a committable/readable config file. Maven's settings.xml
// is MIRRORS-ONLY — no <proxies>, since the sandbox reaches the mirror
// through wardyn-proxy via MAVEN_OPTS. GOPRIVATE is deliberately NOT set
// (it would route modules to direct VCS and defeat the corp GOPROXY).
//
// Injection safety: base URLs come from site-config, which validateSiteConfig
// already rejects if they contain shell/XML metacharacters, so embedding
// base verbatim into TOML/XML/ini here is safe.
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
