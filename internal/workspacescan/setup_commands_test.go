// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"strings"
	"testing"
)

// mgrSet builds a package-manager/tool set for deriveSetupCommands.
func mgrSet(names ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// findStage returns the first command emitted for a stage, or "".
func findStage(cmds []SetupCommand, stage string) string {
	for _, c := range cmds {
		if c.Stage == stage {
			return c.Command
		}
	}
	return ""
}

// TestDeriveSetupCommands_PipUsesVenv is the P0.4 regression: the sandbox agent
// is non-root against a PEP-668 externally-managed system Python, so the pip
// branch MUST emit a venv-based install (never a bare `pip install`), prefer an
// editable project install with a requirements.txt fallback, pin
// SETUPTOOLS_SCM_PRETEND_VERSION for shallow clones, and run pytest FROM the
// venv. A regression to `pip install -r requirements.txt` / bare `pytest` breaks
// every real Python workspace in the sandbox.
func TestDeriveSetupCommands_PipUsesVenv(t *testing.T) {
	cmds := deriveSetupCommands(mgrSet("pip"), mgrSet(), nil, nil, false, false)

	install := findStage(cmds, "install")
	test := findStage(cmds, "test")

	if install == "" || test == "" {
		t.Fatalf("pip: expected an install and a test command, got %+v", cmds)
	}
	for _, want := range []string{
		"python3 -m venv .venv",
		".venv/bin/pip install -e .",
		".venv/bin/pip install -r requirements.txt",
		"SETUPTOOLS_SCM_PRETEND_VERSION=0.0.0",
	} {
		if !strings.Contains(install, want) {
			t.Errorf("pip install command %q missing %q", install, want)
		}
	}
	if strings.Contains(install, "python3 -m pip install") || install == "pip install -r requirements.txt" {
		t.Errorf("pip install must not touch the system Python: %q", install)
	}
	if test != ".venv/bin/python -m pytest" {
		t.Errorf("pip test = %q, want .venv/bin/python -m pytest (must run from the venv)", test)
	}
}

// TestDeriveSetupCommands_PythonMgrPrecedence proves poetry/uv/pipenv each win
// over the bare-pip venv path (they manage their own environment), so a repo
// with a lockfile-managed tool never gets the venv fallback.
func TestDeriveSetupCommands_PythonMgrPrecedence(t *testing.T) {
	cases := []struct {
		mgr         string
		wantInstall string
	}{
		{"poetry", "poetry install"},
		{"uv", "uv sync"},
		{"pipenv", "pipenv install --dev"},
	}
	for _, c := range cases {
		// Even with "pip" also present, the higher-precedence manager wins.
		cmds := deriveSetupCommands(mgrSet(c.mgr, "pip"), mgrSet(), nil, nil, false, false)
		if got := findStage(cmds, "install"); got != c.wantInstall {
			t.Errorf("%s+pip install = %q, want %q (manager must win over bare-pip venv)", c.mgr, got, c.wantInstall)
		}
		if strings.Contains(findStage(cmds, "install"), "venv") {
			t.Errorf("%s must not emit the pip venv path, got %q", c.mgr, findStage(cmds, "install"))
		}
	}
}
