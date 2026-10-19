// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package hostrules

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestEmitArtifactConfig_PerEcosystemSyntax asserts each ecosystem's EXACT
// redirect config (R5 findings) — the bytes a toolchain reads to pull from the
// corp mirror. URL-only: no token ever appears in the emitted config.
func TestEmitArtifactConfig_PerEcosystemSyntax(t *testing.T) {
	bases := map[string]string{
		"npm":   "https://artifactory.corp/api/npm/npm-remote/",
		"pip":   "https://artifactory.corp/api/pypi/pypi-remote/simple",
		"cargo": "https://artifactory.corp/api/cargo/cargo-remote/index/",
		"maven": "https://artifactory.corp/artifactory/maven-remote",
		"go":    "https://artifactory.corp/api/go/go-remote",
		"nuget": "https://artifactory.corp/api/nuget/nuget-remote/v3/index.json",
	}
	files, env := EmitArtifactConfig(bases, false)

	// npm .npmrc
	if got, want := files[".npmrc"], "registry=https://artifactory.corp/api/npm/npm-remote/\n"; got != want {
		t.Errorf(".npmrc\n got: %q\nwant: %q", got, want)
	}
	// pip pip.conf
	if got, want := files[".config/pip/pip.conf"],
		"[global]\nindex-url = https://artifactory.corp/api/pypi/pypi-remote/simple\n"; got != want {
		t.Errorf("pip.conf\n got: %q\nwant: %q", got, want)
	}
	// cargo config.toml — sparse+ prefix is load-bearing; replace-with points at the corp registry
	cargo := files[".cargo/config.toml"]
	for _, sub := range []string{
		"[source.crates-io]",
		"replace-with = \"corp\"",
		"[registries.corp]",
		"index = \"sparse+https://artifactory.corp/api/cargo/cargo-remote/index/\"",
	} {
		if !strings.Contains(cargo, sub) {
			t.Errorf("cargo config.toml missing %q; got:\n%s", sub, cargo)
		}
	}
	// maven settings.xml — mirrorOf * + url; NO wardyn-proxy (proxy rides MAVEN_OPTS), NO <servers> token
	mvn := files[".m2/settings.xml"]
	for _, sub := range []string{
		"<mirrorOf>*</mirrorOf>",
		"<url>https://artifactory.corp/artifactory/maven-remote</url>",
	} {
		if !strings.Contains(mvn, sub) {
			t.Errorf("maven settings.xml missing %q; got:\n%s", sub, mvn)
		}
	}
	if strings.Contains(mvn, "wardyn-proxy") || strings.Contains(mvn, "<servers>") {
		t.Errorf("maven settings.xml must be mirrors-only (no proxy/servers); got:\n%s", mvn)
	}
	// nuget NuGet.Config — clear + add
	ng := files[".nuget/NuGet/NuGet.Config"]
	for _, sub := range []string{
		"<clear />",
		`<add key="corp" value="https://artifactory.corp/api/nuget/nuget-remote/v3/index.json" />`,
	} {
		if !strings.Contains(ng, sub) {
			t.Errorf("nuget config missing %q; got:\n%s", sub, ng)
		}
	}
	// go rides env, not a file (and no go file emitted)
	if env["GOPROXY"] != "https://artifactory.corp/api/go/go-remote" {
		t.Errorf("GOPROXY = %q, want the corp base", env["GOPROXY"])
	}
	if want := "sum.golang.org https://artifactory.corp/api/go/go-remote/sumdb/sum.golang.org"; env["GOSUMDB"] != want {
		t.Errorf("GOSUMDB = %q, want %q (the checksum database through the mirror)", env["GOSUMDB"], want)
	}
	if _, ok := env["GOPRIVATE"]; ok {
		t.Errorf("GOPRIVATE must NOT be set (GOPRIVATE=* would route modules to direct VCS, defeating the corp GOPROXY)")
	}
	// go rides env only: exactly the 5 file-based ecosystems, no go config file.
	if len(files) != 5 {
		t.Errorf("want 5 config files (npm/pip/cargo/maven/nuget), got %d: %v", len(files), slices.Sorted(maps.Keys(files)))
	}
}

// TestEmitArtifactConfig_PartialAndEmpty: only configured ecosystems emit; empty
// input yields nil maps; unknown keys are skipped; no secret leaks.
func TestEmitArtifactConfig_PartialAndEmpty(t *testing.T) {
	files, env := EmitArtifactConfig(nil, false)
	if files != nil || env != nil {
		t.Errorf("empty input must yield nil maps; got files=%v env=%v", files, env)
	}

	files, env = EmitArtifactConfig(map[string]string{
		"npm":     "https://corp/npm/",
		"unknown": "https://corp/x/",
		"go":      "  ", // whitespace-only base is skipped
	}, false)
	if len(files) != 1 {
		t.Fatalf("want exactly the npm file, got %v", files)
	}
	if _, ok := files[".npmrc"]; !ok {
		t.Errorf("expected .npmrc, got %v", files)
	}
	if env != nil {
		t.Errorf("blank go base must not emit env, got %v", env)
	}
}

// TestEmitArtifactConfig_GoChecksumDBOptOut: the per-mirror opt-out is the only way to GOSUMDB=off.
func TestEmitArtifactConfig_GoChecksumDBOptOut(t *testing.T) {
	_, env := EmitArtifactConfig(map[string]string{"go": "https://go-mirror.corp/repo/"}, true)
	if env["GOSUMDB"] != "off" || env["GOPROXY"] != "https://go-mirror.corp/repo/" {
		t.Errorf("opted-out env = %v, want GOSUMDB=off with the mirror as GOPROXY", env)
	}
	_, env = EmitArtifactConfig(map[string]string{"go": "https://go-mirror.corp/repo/"}, false)
	if want := "sum.golang.org https://go-mirror.corp/repo/sumdb/sum.golang.org"; env["GOSUMDB"] != want {
		t.Errorf("default env GOSUMDB = %q, want %q (one slash between the mirror and /sumdb/)", env["GOSUMDB"], want)
	}
}

// A cargo index URL is emitted with exactly one trailing slash, whether or not the configured base has one.
func TestEmitArtifactConfig_CargoIndexEndsInOneSlash(t *testing.T) {
	for _, base := range []string{"https://artifactory.corp/api/cargo/index", "https://artifactory.corp/api/cargo/index/", "https://artifactory.corp/api/cargo/index//"} {
		files, _ := EmitArtifactConfig(map[string]string{"cargo": base}, false)
		if want := `index = "sparse+https://artifactory.corp/api/cargo/index/"`; !strings.Contains(files[".cargo/config.toml"], want) {
			t.Errorf("base %q: config.toml = %q, want it to contain %s", base, files[".cargo/config.toml"], want)
		}
	}
}
