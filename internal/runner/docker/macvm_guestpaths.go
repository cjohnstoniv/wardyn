// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// The Linux-to-macOS path table: the dispatcher builds every sandbox Linux-shaped, and a macOS guest's
// Init carries each of its paths through here. One table is the truth, kept runner-side as the CA
// variables are (macGuestInit).

// macGuestDirs maps each Linux directory a sandbox's env or files name to the guest's own: the run
// user's home (on macOS /home is an automount point), and the runtime directory a delivered secret lands
// in.
var macGuestDirs = [][2]string{
	{"/home/agent", guestagent.MacAgentHome},
	{"/run/wardyn", guestagent.MacRunDir},
}

// macGuestManagedFiles maps every managed file a harness reads at a Linux path to where the same harness
// reads it on macOS (the vendors' paths are cited at the guestagent constants). The two MCP files are
// internal/api's claudeMCPInteractivePath and claudeMCPAutonomousPath.
var macGuestManagedFiles = map[string]string{
	agentpolicy.ClaudeCodeManagedSettingsPath:   guestagent.MacClaudeCodeManagedSettingsPath,
	runner.ManagedFileDir + "/managed-mcp.json": guestagent.MacClaudeCodeManagedMCPPath,
	runner.ManagedFileDir + "/wardyn-mcp.json":  guestagent.MacClaudeMCPConfigPath,
	agentpolicy.CodexRequirementsPath:           guestagent.MacCodexRequirementsPath,
}

// macGuestFiles is the run's managed files at their guest paths, each at its resolved mode (the guest
// writes exactly the mode it is handed). A file the table does not map refuses the run: forwarded at its
// Linux path no macOS harness reads it, and dropped the run starts without the walls it carries.
func macGuestFiles(files []runner.ManagedFile) ([]guestagent.File, error) {
	out := make([]guestagent.File, 0, len(files))
	for _, f := range files {
		p, ok := macGuestManagedFiles[f.Path]
		if !ok && path.Dir(f.Path) == runner.ComponentSecretDir {
			p, ok = macGuestDirPath(f.Path)
		}
		if !ok {
			//lint:ignore ST1005 the sentence the run's owner reads
			return nil, fmt.Errorf("This run's policy writes %s, which has no macOS location yet, so the macOS VM stopped before the run started.", f.Path)
		}
		out = append(out, guestagent.File{Path: p, Mode: f.FileMode(), AgentOwned: f.AgentOwned, Content: f.Content})
	}
	return out, nil
}

// macGuestFilesEnv names files for the launcher's pre-start check (guestagent.EnvManagedFiles), or is ""
// for none.
func macGuestFilesEnv(files []guestagent.File) string {
	if len(files) == 0 {
		return ""
	}
	checks := make([]guestagent.ManagedFileCheck, 0, len(files))
	for _, f := range files {
		checks = append(checks, guestagent.ManagedFileCheck{Path: f.Path, Mode: f.Mode, AgentOwned: f.AgentOwned})
	}
	b, _ := json.Marshal(checks)
	return string(b)
}

// macGuestEnv moves every path in env's values to the guest's own (AWS_CONFIG_FILE under /home/agent,
// a comma-separated list of work folders, a repo target in JSON). A value that still names a /home or
// /run path anywhere afterwards names a place the guest does not have, and refuses the run rather than point a tool at
// nothing.
func macGuestEnv(env map[string]string) error {
	for k, v := range env {
		for _, d := range macGuestDirs {
			v = replacePathPrefix(v, d[0], d[1])
		}
		if startsPath(v, "/home") || startsPath(v, "/run") {
			//lint:ignore ST1005 the sentence the run's owner reads
			return fmt.Errorf("This run's %s names %s, which has no macOS location yet, so the macOS VM stopped before the run started.", k, env[k])
		}
		env[k] = v
	}
	return nil
}

// macGuestDirPath is p, a single absolute path, under the guest's counterpart of its Linux directory.
func macGuestDirPath(p string) (string, bool) {
	for _, d := range macGuestDirs {
		if p == d[0] || strings.HasPrefix(p, d[0]+"/") {
			return d[1] + p[len(d[0]):], true
		}
	}
	return "", false
}

// startsPath says whether dir starts a path anywhere in s, by replacePathPrefix's rule: a list or a JSON
// value whose first path moved can still name another later.
func startsPath(s, dir string) bool {
	return replacePathPrefix(s, dir, "\x00") != s
}

// replacePathPrefix replaces each occurrence of the directory from in s that starts a path: not inside a
// longer path ("/x/home/agent") and not a prefix of a longer name ("/home/agentx").
func replacePathPrefix(s, from, to string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, from)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(from)
		if (i == 0 || !isPathByte(s[i-1])) && (end == len(s) || s[end] == '/' || !isPathByte(s[end])) {
			b.WriteString(s[:i])
			b.WriteString(to)
		} else {
			b.WriteString(s[:end])
		}
		s = s[end:]
	}
}

// isPathByte reports a byte that continues a path element or path, so a match beside it is not a whole
// directory.
func isPathByte(c byte) bool {
	return c == '/' || c == '.' || c == '_' || c == '-' || c == '~' ||
		'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// macKeychainTrust is the guest's answer to Init.RunCA as the runner wire carries it: nil when the
// guest answered nothing (no run CA, or an agent that predates it), otherwise its codes, read through
// runner.KeychainTrust.Known so a code the runner does not know reads as unavailable.
func macKeychainTrust(res guestagent.InitResult) *runner.KeychainTrust {
	if res.KeychainTrust == "" && res.KeychainTrustReason == "" {
		return nil
	}
	return (&runner.KeychainTrust{Status: res.KeychainTrust, Reason: res.KeychainTrustReason}).Known()
}
