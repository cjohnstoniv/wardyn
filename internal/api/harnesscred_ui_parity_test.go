// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestHarnessLoginTask_UIParity pins the two SERVER-SIDE literals the run page's
// login-sandbox note keys on.
//
// `harnessLoginTask` is a discriminator the client never sends: the console can
// only recognise a login run by matching the string the server stamped. The same
// goes for `awsSSOAgent` — the task alone is provider-agnostic (the Anthropic
// container login carries it too), so the note needs BOTH or it says "AWS sign-in
// sandbox — the AWS CLI and nothing else" over a `claude setup-token` box.
//
// Without this, renaming either Go constant leaves every test green while the
// note silently stops rendering on the only run it exists for — the TS side can
// only ever assert its own literal against itself. Same source-parse idiom as
// cmd/wardyn-aws-sso's TestLoginCommand_UIParity: Go and TypeScript cannot share
// a constant, so the test reads both sides.
func TestHarnessLoginTask_UIParity(t *testing.T) {
	uiPath := filepath.Join("..", "..", "ui", "src", "app", "components", "screens",
		"run-detail", "login-sandbox-note.tsx")
	b, err := os.ReadFile(uiPath) //nolint:gosec // fixed in-repo path
	if err != nil {
		t.Fatalf("read %s: %v", uiPath, err)
	}
	ui := string(b)

	for _, c := range []struct {
		name  string
		re    *regexp.Regexp
		want  string
		wants string
	}{
		{"HARNESS_LOGIN_TASK", regexp.MustCompile(`HARNESS_LOGIN_TASK\s*=\s*"([^"]+)"`), harnessLoginTask, "harnessLoginTask"},
		{"AWS_SSO_LOGIN_AGENT", regexp.MustCompile(`AWS_SSO_LOGIN_AGENT\s*=\s*"([^"]+)"`), awsSSOAgent, "awsSSOAgent"},
	} {
		m := c.re.FindStringSubmatch(ui)
		if m == nil {
			t.Errorf("no %s in %s", c.name, uiPath)
			continue
		}
		if m[1] != c.want {
			t.Errorf("drift: console %s = %q, server %s = %q — the run page would stop naming the login sandbox",
				c.name, m[1], c.wants, c.want)
		}
	}
}
