// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The chained login command had THREE copies, in three languages, and the one
// that mattered most was missing: a human who opened the login run from /runs
// got a bare prompt and no hint, so the reasonable next move was `aws sso login`
// alone — which leaves the token in ~/.aws/sso/cache, where it dies with the
// container. Now there is one definition (deploy/images/aws-sso/login-hint.sh,
// sourced by agent-run AND by the interactive attach shell) plus the console
// pane's own copy, which cannot import a shell variable.
//
// Same source-parse idiom as TestSuccessMarker_UIParity above: Go, TypeScript
// and shell cannot share a constant, so the test reads both sides.
var (
	shellLoginCommand = regexp.MustCompile(`WARDYN_AWS_SSO_LOGIN_COMMAND='([^']+)'`)
	paneFlowCommand   = regexp.MustCompile(`cmd:\s*"([^"]+)"`)
)

func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	b, err := os.ReadFile(path) //nolint:gosec // fixed in-repo path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestLoginCommand_UIParity: the command the console types into the attach PTY
// and the command the sandbox's own shell tells an operator to run are the same
// bytes. A drift here is a human typing half the flow.
func TestLoginCommand_UIParity(t *testing.T) {
	m := shellLoginCommand.FindStringSubmatch(repoFile(t, "deploy", "images", "aws-sso", "login-hint.sh"))
	if m == nil {
		t.Fatal("no WARDYN_AWS_SSO_LOGIN_COMMAND in deploy/images/aws-sso/login-hint.sh")
	}
	shell := m[1]

	pane := repoFile(t, "ui", "src", "app", "components", "screens", "settings", "harness-login-pane.tsx")
	var ui string
	// LOGIN_FLOWS has one `cmd:` per provider; the AWS row is the one that runs
	// the CLI this helper belongs to.
	for _, hit := range paneFlowCommand.FindAllStringSubmatch(pane, -1) {
		if strings.HasPrefix(hit[1], "aws sso login") {
			ui = hit[1]
			break
		}
	}
	if ui == "" {
		t.Fatal("no aws `cmd:` in harness-login-pane.tsx's LOGIN_FLOWS")
	}
	if ui != shell {
		t.Errorf("login command drift: console types %q, the sandbox's shell says %q", ui, shell)
	}
	// The helper is the half that makes the login capture anything at all.
	if !strings.Contains(shell, "wardyn-aws-sso") {
		t.Errorf("the chained command %q no longer runs the uploader — a login alone captures nothing", shell)
	}
}

// TestLoginCommand_NoFourthCopy: agent-run must READ the variable, not carry its
// own literal. A fourth copy is how the three drifted in the first place.
func TestLoginCommand_NoFourthCopy(t *testing.T) {
	agentRun := repoFile(t, "deploy", "images", "aws-sso", "agent-run")
	if strings.Contains(agentRun, "&& wardyn-aws-sso") {
		t.Error("deploy/images/aws-sso/agent-run spells the chained command out again; source login-hint.sh and use $WARDYN_AWS_SSO_LOGIN_COMMAND")
	}
	if !strings.Contains(agentRun, "WARDYN_AWS_SSO_LOGIN_COMMAND") {
		t.Error("deploy/images/aws-sso/agent-run no longer uses the shared login command")
	}
	// The attach shell is where the hint actually reaches a human.
	bashrc := repoFile(t, "deploy", "images", "common", "attach-bashrc.sh")
	if !strings.Contains(bashrc, "/usr/local/lib/wardyn-attach-hint.sh") {
		t.Error("the interactive attach shell no longer sources a per-image hint, so the login box says nothing")
	}
}
