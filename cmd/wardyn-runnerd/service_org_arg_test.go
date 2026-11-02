// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runnerunits "github.com/cjohnstoniv/wardyn/deploy/runner"
)

// v090Plist is the ProgramArguments of the v0.9.8 LaunchAgent template, which setup filled with the escaped
// organisation address.
const v090Plist = `<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.wardyn.runnerd</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/x/bin/wardyn-runnerd</string>
    <string>serve</string>
    <string>--org</string>
    <string>%s</string>
    <string>--state-dir</string>
    <string>/Users/x/Library/Application Support/wardyn/runner</string>
  </array>
</dict>
</plist>
`

func TestServiceOrgArgReadsAnEarlierLaunchAgentAndNotAModernOne(t *testing.T) {
	const org = "https://org.example.com/?a=1&b=2"
	write := func(body string) setupPaths {
		f := filepath.Join(t.TempDir(), "com.wardyn.runnerd.plist")
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return setupPaths{plistFile: f}
	}
	got, from := write(strings.Replace(v090Plist, "%s", html.EscapeString(org), 1)).serviceOrgArg()
	if got != org || from != "the LaunchAgent's --org argument" {
		t.Errorf("earlier LaunchAgent: %q from %q, want the decoded address", got, from)
	}
	modern := strings.NewReplacer("__WARDYN_RUNNERD__", "/bin/wardyn-runnerd", "__WARDYN_CONFIG__", "/cfg/runnerd.json").Replace(plistComment.ReplaceAllString(runnerunits.LaunchAgent, ""))
	if got, from := write(modern).serviceOrgArg(); got != "" || from != "" {
		t.Errorf("modern LaunchAgent: %q from %q, want none", got, from)
	}
}

// #3870: prepare reads which runner the setup LaunchAgent runs from the agent itself, unescaped, and nothing
// from a missing one.
func TestServiceProgramReadsTheLaunchAgentsBinary(t *testing.T) {
	const bin = "/Users/a&b/Library/Application Support/wardyn/bin/wardyn-runnerd"
	f := filepath.Join(t.TempDir(), "com.wardyn.runnerd.plist")
	p := setupPaths{plistFile: f}
	if got := p.serviceProgram(); got != "" {
		t.Fatalf("no LaunchAgent: %q", got)
	}
	plan := setupPlan{paths: setupPaths{installed: bin, cliProfile: "/cfg", logFile: "/log/runnerd.log"}}
	if err := os.WriteFile(f, []byte(renderLaunchAgent(plan)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := p.serviceProgram(); got != bin {
		t.Fatalf("setup's LaunchAgent runs %q, want %q", got, bin)
	}
	if err := os.WriteFile(f, []byte(strings.Replace(v090Plist, "%s", "https://org.example", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := p.serviceProgram(); got != "/Users/x/bin/wardyn-runnerd" {
		t.Fatalf("an earlier LaunchAgent runs %q", got)
	}
	if got := (setupPaths{}).serviceProgram(); got != "" {
		t.Fatalf("no plist path: %q", got)
	}
}
