// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"html"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	runnerunits "github.com/cjohnstoniv/wardyn/deploy/runner"
)

var (
	unitFlag    = regexp.MustCompile(`--([a-z-]+) \$\{([A-Z0-9_]+)\}`)
	plistEnv    = regexp.MustCompile(`(?s)<key>EnvironmentVariables</key>\s*<dict>(.*?)</dict>`)
	plistArgs   = regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>(.*?)</array>`)
	plistKey    = regexp.MustCompile(`<key>([^<]+)</key>`)
	plistString = regexp.MustCompile(`<string>([^<]*)</string>`)
)

// The macOS LaunchAgent and the Linux service hand serve the same settings. Linux writes each into the
// env file, and its unit passes some of them as flags (--org ${WARDYN_RUNNER_ORG_URL}); the LaunchAgent
// sets each as an environment variable or passes it as that same flag. A setting one writer gives serve
// and the other drops (the state directory, once) fails here, whatever the name.
func TestSetupGivesServeTheSameSettingsOnMacOSAndLinux(t *testing.T) {
	opts := setupOptions{org: "https://org.example", proxyImage: "registry.example/proxy:1", relayListen: "127.0.0.1", network: "wardyn-internal",
		relayHostTarget: "host-gateway", trustedCAFile: "/etc/wardyn/ca.pem"}
	plan := func(goos string) setupPlan {
		paths, err := setupHost{goos: goos, home: t.TempDir()}.paths()
		if err != nil {
			t.Fatal(err)
		}
		return setupPlan{setupOptions: opts, paths: paths, flagged: true, dockerHost: "unix:///var/run/docker.sock"}
	}
	flagVar := map[string]string{}
	for _, m := range unitFlag.FindAllStringSubmatch(runnerunits.SystemdUnit, -1) {
		flagVar[m[1]] = m[2]
	}
	linux := map[string]bool{}
	for line := range strings.Lines(plan("linux").serviceEnv()) {
		if k, _, ok := strings.Cut(line, "="); ok {
			linux[k] = true
		}
	}
	for flag, v := range flagVar {
		if !linux[v] {
			t.Fatalf("the unit passes --%s ${%s}, which the env file does not set", flag, v)
		}
	}
	mac := plan("darwin")
	agent := renderLaunchAgent(mac)
	darwin := map[string]bool{}
	if m := plistEnv.FindStringSubmatch(agent); m != nil {
		for _, k := range plistKey.FindAllStringSubmatch(m[1], -1) {
			darwin[k[1]] = true
		}
	}
	values := map[string]string{}
	if m := plistArgs.FindStringSubmatch(agent); m != nil {
		args := plistString.FindAllStringSubmatch(m[1], -1)
		for i := 0; i+1 < len(args); i++ {
			if v, ok := flagVar[strings.TrimPrefix(args[i][1], "--")]; ok && strings.HasPrefix(args[i][1], "--") {
				darwin[v], values[v] = true, html.UnescapeString(args[i+1][1])
			}
		}
	}
	if l, d := slices.Sorted(maps.Keys(linux)), slices.Sorted(maps.Keys(darwin)); !slices.Equal(l, d) {
		t.Fatalf("the Linux service sets %v; the LaunchAgent sets %v", l, d)
	}
	if got := values["WARDYN_RUNNER_STATE_DIR"]; got != mac.paths.stateDir {
		t.Fatalf("the LaunchAgent passes --state-dir %q, want setup's %q", got, mac.paths.stateDir)
	}
}
