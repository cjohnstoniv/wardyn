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

	pane := repoFile(t, "ui", "src", "app", "components", "screens", "settings", "login-flows.tsx")
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
		t.Fatal("no aws `cmd:` in login-flows.tsx's LOGIN_FLOWS")
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
	// And the Dockerfile is the ONLY thing that puts the file at that path,
	// while agent-run hard-sources it under `set -euo pipefail`: drop the COPY
	// and agent-run exits 1 at container start — the login sandbox never comes
	// up at all — with every assertion above still green.
	dockerfile := repoFile(t, "deploy", "images", "aws-sso", "Dockerfile")
	if !strings.Contains(dockerfile, "login-hint.sh /usr/local/lib/wardyn-attach-hint.sh") {
		t.Error("deploy/images/aws-sso/Dockerfile no longer installs login-hint.sh at /usr/local/lib/wardyn-attach-hint.sh; agent-run sources that path and would fail the container at start")
	}
}

// ── the sandbox signs itself in (finding 4) ──────────────────────────────────

var (
	shellSelfRunBanner = regexp.MustCompile(`WARDYN_AWS_SSO_SELFRUN_BANNER='([^']+)'`)
	paneSelfRunMarker  = regexp.MustCompile(`SELFRUN_MARKER\s*=\s*"([^"]+)"`)
)

// TestAWSSSOImage_HasTmuxAndShims: the three image facts the self-run rests on.
// Without tmux every attach falls through to a bare `bash -i` and nothing runs
// the pair; without the shims `claude` is `command not found`, which the operator
// who found this read (reasonably) as a misconfigured run. A Dockerfile source
// parse, because only building the image would prove it otherwise — and this has
// to red in a unit suite, not in a 10-minute image build.
func TestAWSSSOImage_HasTmuxAndShims(t *testing.T) {
	dockerfile := repoFile(t, "deploy", "images", "aws-sso", "Dockerfile")
	for _, want := range []string{
		"        tmux \\",
		"COPY deploy/images/common/tmux.conf /etc/tmux.conf",
		"COPY deploy/images/aws-sso/signin-pane.sh /usr/local/bin/signin-pane.sh",
		"COPY deploy/images/aws-sso/not-a-coding-agent.sh /usr/local/bin/claude",
		"COPY deploy/images/aws-sso/not-a-coding-agent.sh /usr/local/bin/codex",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("deploy/images/aws-sso/Dockerfile no longer has %q", want)
		}
	}
	// PRESENCE is not the property for the shims: a `claude` that returned 0
	// would be worse than none at all.
	shim := repoFile(t, "deploy", "images", "aws-sso", "not-a-coding-agent.sh")
	if !strings.Contains(shim, "exit 1") {
		t.Error("the coding-agent shim no longer fails closed; a script that shells out to `claude` here would look like it worked")
	}
}

// TestSelfRunBanner_UIParity: the console waits out a grace window and then types
// the chained command itself, for an operator-pinned image that predates the
// self-run. The ONE thing that stops it is seeing the sandbox's own banner — so
// the shell line has to START with the exact prefix the pane scans for, in two
// languages that cannot share a constant. Same source-parse idiom as
// TestLoginCommand_UIParity above.
func TestSelfRunBanner_UIParity(t *testing.T) {
	m := shellSelfRunBanner.FindStringSubmatch(repoFile(t, "deploy", "images", "aws-sso", "login-hint.sh"))
	if m == nil {
		t.Fatal("no WARDYN_AWS_SSO_SELFRUN_BANNER in deploy/images/aws-sso/login-hint.sh — the sign-in pane announces nothing and the console types over it")
	}
	p := paneSelfRunMarker.FindStringSubmatch(repoFile(t, "ui", "src", "app", "components", "screens", "settings", "login-pane-copy.ts"))
	if p == nil {
		t.Fatal("no SELFRUN_MARKER in login-pane-copy.ts")
	}
	if !strings.HasPrefix(m[1], p[1]) {
		t.Errorf("banner drift: the sandbox prints %q, the console watches for the prefix %q — the console would type a SECOND login into a sandbox already running one", m[1], p[1])
	}
}
