// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"slices"
	"testing"
)

func setupCmdStrings(cmds []SetupCommand) []string {
	if len(cmds) == 0 {
		return nil
	}
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Stage+":"+c.Command)
	}
	return out
}

func TestScan_DocsMarkersEmitFixedBuildTemplates(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "mkdocs.yml", "site_name: Test\n")
	got := Scan(dir)
	eq(t, "mkdocs SetupCommands", setupCmdStrings(got.SetupCommands),
		[]string{"build:mkdocs build"})

	hugo := t.TempDir()
	writeFile(t, hugo, "hugo.toml", "baseURL = 'https://example.org/'\n")
	eq(t, "hugo SetupCommands", setupCmdStrings(Scan(hugo).SetupCommands),
		[]string{"build:hugo --minify"})
}

func TestScan_SphinxOnlyUnderDocsDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "docs/conf.py", "project = 'Test'\n")
	eq(t, "sphinx SetupCommands", setupCmdStrings(Scan(dir).SetupCommands),
		[]string{"build:sphinx-build -b html docs _build"})

	// A conf.py NOT under docs/ is any Python module, not Sphinx.
	bare := t.TempDir()
	writeFile(t, bare, "conf.py", "x = 1\n")
	if cmds := Scan(bare).SetupCommands; len(cmds) != 0 {
		t.Fatalf("bare conf.py commands = %v, want none", setupCmdStrings(cmds))
	}
}

func TestScan_DocusaurusDoesNotDoubleEmitBuild(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "docusaurus.config.js", "module.exports = {};\n")
	writeFile(t, dir, "package.json", `{"scripts":{"build":"docusaurus build"}}`)
	writeFile(t, dir, "package-lock.json", "{}")
	got := Scan(dir)
	// Exactly one build command — the JS branch's fixed template; the
	// docusaurus marker adds a tool hint but no second build.
	eq(t, "docusaurus SetupCommands", setupCmdStrings(got.SetupCommands),
		[]string{"install:npm ci", "build:npm run build"})
	if !slices.Contains(got.Tools, "docusaurus") {
		t.Errorf("Tools = %v, want docusaurus hint", got.Tools)
	}
}
