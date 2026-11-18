// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"encoding/json"
	"io"
	"regexp"
	"slices"
)

// conventionalScriptKeys/conventionalMakeTargets are the only names setup-command
// synthesis cares about; only KEY PRESENCE is captured, never a script body
// (a hostile scripts.build must never become a command).
var conventionalScriptKeys = map[string]struct{}{"build": {}, "test": {}, "lint": {}}
var conventionalMakeTargets = map[string]struct{}{"build": {}, "test": {}, "install": {}, "lint": {}}

// makeTargetRE captures a Makefile target name at the start of a rule line.
var makeTargetRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*)\s*:`)

// detectPackageJSON records which conventional script KEYS a package.json
// declares, never the bodies. Parse failure is safe (no keys recorded).
func detectPackageJSON(path scanFile, facts *ScanFacts) {
	f, err := path.open()
	if err != nil {
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return
	}
	var pkg struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return
	}
	for k := range pkg.Scripts {
		if _, ok := conventionalScriptKeys[k]; ok && !slices.Contains(facts.ScriptKeys, k) {
			facts.ScriptKeys = append(facts.ScriptKeys, k)
		}
	}
}

// detectMakefile records which conventional TARGETS a Makefile defines —
// names only, never recipe bodies.
func detectMakefile(path scanFile, facts *ScanFacts) {
	eachLine(path, facts, func(line string) bool {
		if m := makeTargetRE.FindStringSubmatch(line); m != nil {
			t := m[1]
			if _, ok := conventionalMakeTargets[t]; ok && !slices.Contains(facts.MakeTargets, t) {
				facts.MakeTargets = append(facts.MakeTargets, t)
			}
		}
		return true
	})
}

// jsInstallCmd returns the install command for the detected JS package manager.
func jsInstallCmd(pkgMgrs map[string]struct{}) (string, bool) {
	switch {
	case has(pkgMgrs, "pnpm"):
		return "pnpm install --frozen-lockfile", true
	case has(pkgMgrs, "yarn"):
		return "yarn install --frozen-lockfile", true
	case has(pkgMgrs, "npm"):
		return "npm ci", true
	}
	return "", false
}

func jsRunPrefix(pkgMgrs map[string]struct{}) string {
	if has(pkgMgrs, "pnpm") {
		return "pnpm run "
	}
	if has(pkgMgrs, "yarn") {
		return "yarn "
	}
	return "npm run "
}

func has(m map[string]struct{}, k string) bool { _, ok := m[k]; return ok }

// deriveSetupCommands synthesizes ordered install/build/test/lint commands from
// FIXED templates keyed on detected package managers/tools/script keys —
// never taken from file content. Deduped by (stage, command).
func deriveSetupCommands(pkgMgrs, tools map[string]struct{}, scriptKeys, makeTargets []string, hasMvnw, hasGradlew bool) []SetupCommand {
	var out []SetupCommand
	seen := map[string]struct{}{}
	add := func(stage, cmd, src string) {
		if cmd == "" {
			return
		}
		k := stage + "\x00" + cmd
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, SetupCommand{Stage: stage, Command: cmd, Source: src})
	}
	scriptSet := map[string]struct{}{}
	for _, k := range scriptKeys {
		scriptSet[k] = struct{}{}
	}
	makeSet := map[string]struct{}{}
	for _, t := range makeTargets {
		makeSet[t] = struct{}{}
	}

	// JavaScript / Node.
	if cmd, ok := jsInstallCmd(pkgMgrs); ok {
		add("install", cmd, "convention:node")
		pre := jsRunPrefix(pkgMgrs)
		if _, ok := scriptSet["build"]; ok {
			add("build", pre+"build", "package.json:build")
		}
		if _, ok := scriptSet["test"]; ok {
			add("test", pre+"test", "package.json:test")
		}
		if _, ok := scriptSet["lint"]; ok {
			add("lint", pre+"lint", "package.json:lint")
		}
	}
	// Python.
	if has(pkgMgrs, "poetry") {
		add("install", "poetry install", "convention:poetry")
		add("test", "poetry run pytest", "convention:poetry")
	} else if has(pkgMgrs, "uv") {
		add("install", "uv sync", "convention:uv")
		add("test", "uv run pytest", "convention:uv")
	} else if has(pkgMgrs, "pipenv") {
		add("install", "pipenv install --dev", "convention:pipenv")
		add("test", "pipenv run pytest", "convention:pipenv")
	} else if has(pkgMgrs, "pip") {
		// venv because: Debian's system Python is PEP-668 externally-managed and
		// the sandbox agent is non-root; prefer an editable install, falling back
		// to requirements.txt; SETUPTOOLS_SCM_PRETEND_VERSION lets scm-versioned
		// builds succeed on the shallow (tagless) clone.
		add("install",
			"python3 -m venv .venv && SETUPTOOLS_SCM_PRETEND_VERSION=0.0.0 .venv/bin/pip install -e . "+
				"|| SETUPTOOLS_SCM_PRETEND_VERSION=0.0.0 .venv/bin/pip install -r requirements.txt",
			"convention:pip")
		add("test", ".venv/bin/python -m pytest", "convention:pip")
	}
	// Go.
	if has(pkgMgrs, "go") {
		add("install", "go mod download", "convention:go")
		add("build", "go build ./...", "convention:go")
		add("test", "go test ./...", "convention:go")
	}
	// Rust.
	if has(pkgMgrs, "cargo") {
		add("build", "cargo build", "convention:cargo")
		add("test", "cargo test", "convention:cargo")
	}
	// Java: maven / gradle (prefer the wrapper when present).
	if has(pkgMgrs, "maven") {
		mvn := "mvn"
		if hasMvnw {
			mvn = "./mvnw"
		}
		add("build", mvn+" -B -q -DskipTests package", "convention:maven")
		add("test", mvn+" -B -q test", "convention:maven")
	}
	if has(pkgMgrs, "gradle") {
		gr := "gradle"
		if hasGradlew {
			gr = "./gradlew"
		}
		add("build", gr+" build -x test", "convention:gradle")
		add("test", gr+" test", "convention:gradle")
	}
	// Ruby.
	if has(pkgMgrs, "gem") {
		add("install", "bundle install", "convention:bundler")
	}
	// PHP.
	if has(pkgMgrs, "composer") {
		add("install", "composer install --no-interaction", "convention:composer")
	}
	// Docs generators: marker-derived tool hints → fixed build-stage templates.
	// Docusaurus has none — its package.json build script rides the JS branch above.
	if has(tools, "mkdocs") {
		add("build", "mkdocs build", "convention:mkdocs")
	}
	if has(tools, "sphinx") {
		add("build", "sphinx-build -b html docs _build", "convention:sphinx")
	}
	if has(tools, "hugo") {
		add("build", "hugo --minify", "convention:hugo")
	}

	// Makefile targets corroborate but never override a language convention:
	// add one only for a stage with no convention-provided command yet.
	stageHas := map[string]bool{}
	for _, c := range out {
		stageHas[c.Stage] = true
	}
	for _, stage := range []string{"install", "build", "test", "lint"} {
		if _, ok := makeSet[stage]; ok && !stageHas[stage] {
			add(stage, "make "+stage, "Makefile:"+stage)
		}
	}
	return out
}
