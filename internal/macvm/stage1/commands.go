// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package stage1

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// stdinKind is what a command reads on stdin. Every variable value reaches
// the guest this way; no argv carries one.
type stdinKind int

const (
	stdinNone          stdinKind = iota
	stdinRotate                  // dscl's own command line: passwd <user> <code> <new>
	stdinAgent                   // the guest agent's binary
	stdinPlist                   // the guest agent's LaunchDaemon plist
	stdinAgentPassword           // the agent user's password, one line
	stdinKCPassword              // /etc/kcpassword: the agent user's password, obfuscated
)

// command is one fixed remote command. Root commands run under
// sudo -k -S with an empty prompt, which reads the admin password as stdin's first line
// (-k: never a cached credential, so that line is always sudo's).
type command struct {
	ID   string
	Step string // the MAC-VM §3.4 step it implements
	Root bool
	Argv []string
	In   stdinKind
	// OnlyWithCode: run only on the D1(c) path, where the login is the
	// Mac's person's one-time code.
	OnlyWithCode bool
	// Event, when set, is the prepare event detail emitted before this row.
	Event string
	// CheckAgentSHA: the output must start with the agent's SHA-256.
	CheckAgentSHA bool
	// CheckAccounts: the output is the guest's local users with their UniqueIDs,
	// and the row passes only when those of 500 or more are exactly the admin and the agent.
	CheckAccounts bool
	// MayDropSession: turning Remote Login off may end this very session
	// before it reports an exit status; the fresh-dial check after it decides.
	MayDropSession bool
}

var sudoArgv = []string{"/usr/bin/sudo", "-k", "-S", "-p", "", "--"}

const (
	agentUser    = "agent"
	kcpasswdPath = "/etc/kcpassword"
	agentLibexec = "/usr/local/libexec"
)

// commands is stage 1, in the order it runs. Every command and setting name
// here is UNVERIFIED (external) until a P2/P3 rental run witnesses it; this
// table is the one place to correct them.
var commands = []command{
	// D1(c): the first SSH step replaces the one-time code (§3.4 step 6, D1(c)).
	{ID: "rotate-admin-password", Step: "6 D1(c)", Event: "replacing the one-time code", Argv: []string{"/usr/bin/dscl", "."}, In: stdinRotate, OnlyWithCode: true},

	// a. the guest agent and its LaunchDaemon (§3.4 step 7a). They are
	// loaded only once b has made the user the agent runs as.
	{ID: "agent-dir", Step: "7a", Event: "installing the guest agent", Root: true, Argv: []string{"/usr/bin/install", "-d", "-o", "root", "-g", "wheel", "-m", "755", agentLibexec}},
	{ID: "agent-create", Step: "7a", Root: true, Argv: []string{"/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "755", "/dev/null", guestagent.MacAgentBinaryPath}},
	{ID: "agent-write", Step: "7a", Root: true, Argv: []string{"/bin/dd", "of=" + guestagent.MacAgentBinaryPath}, In: stdinAgent},
	{ID: "agent-verify", Step: "7a", Root: true, Argv: []string{"/usr/bin/shasum", "-a", "256", guestagent.MacAgentBinaryPath}, CheckAgentSHA: true},
	{ID: "plist-create", Step: "7a", Root: true, Argv: []string{"/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "644", "/dev/null", guestagent.MacAgentPlistPath}},
	{ID: "plist-write", Step: "7a", Root: true, Argv: []string{"/bin/dd", "of=" + guestagent.MacAgentPlistPath}, In: stdinPlist},

	// b. the standard user agent, and auto-login as it (§3.4 step 7b).
	{ID: "agent-user", Step: "7b", Event: "creating the agent user", Root: true, Argv: []string{"/usr/sbin/sysadminctl", "-addUser", agentUser, "-fullName", agentUser, "-password", "-"}, In: stdinAgentPassword},
	// Only the admin and the agent may be accounts the base ships with: a person's setup by hand could have
	// made another, and every VM cloned from the base would hold it.
	{ID: "accounts-check", Step: "7b", Root: true, Argv: []string{"/usr/bin/dscl", ".", "-list", "/Users", "UniqueID"}, CheckAccounts: true},
	{ID: "autologin-user", Step: "7b", Root: true, Argv: []string{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.loginwindow", "autoLoginUser", agentUser}},
	{ID: "kcpassword-create", Step: "7b", Root: true, Argv: []string{"/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "600", "/dev/null", kcpasswdPath}},
	{ID: "kcpassword-write", Step: "7b", Root: true, Argv: []string{"/bin/dd", "of=" + kcpasswdPath}, In: stdinKCPassword},

	// a, continued: the agent's user exists now, so its LaunchDaemon loads.
	{ID: "agent-enable", Step: "7a", Event: "starting the guest agent", Root: true, Argv: []string{"/bin/launchctl", "enable", "system/" + guestagent.MacAgentLabel}},
	{ID: "agent-load", Step: "7a", Root: true, Argv: []string{"/bin/launchctl", "bootstrap", "system", guestagent.MacAgentPlistPath}},

	// c. Screen Sharing as root, with the legacy VNC-password option; the
	// password itself is set later by the guest agent's file write (§3.4 step 7c, §3.8).
	{ID: "screensharing-enable", Step: "7c", Event: "turning on Screen Sharing", Root: true, Argv: []string{"/bin/launchctl", "enable", "system/com.apple.screensharing"}},
	{ID: "screensharing-load", Step: "7c", Root: true, Argv: []string{"/bin/launchctl", "bootstrap", "system", "/System/Library/LaunchDaemons/com.apple.screensharing.plist"}},
	{ID: "vnc-legacy", Step: "7c", Root: true, Argv: []string{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.RemoteManagement", "VNCLegacyConnectionsEnabled", "-bool", "true"}},

	// d. never sleep, no screen lock, no automatic software updates (§3.4 step 7d).
	{ID: "power-never-sleep", Step: "7d", Event: "setting power, screen lock and updates", Root: true, Argv: []string{"/usr/bin/pmset", "-a", "sleep", "0", "displaysleep", "0", "disksleep", "0"}},
	{ID: "screenlock-off", Step: "7d", Root: true, Argv: []string{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.screensaver", "askForPassword", "-int", "0"}},
	{ID: "screensaver-off", Step: "7d", Root: true, Argv: []string{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.screensaver", "idleTime", "-int", "0"}},
	{ID: "updates-check-off", Step: "7d", Root: true, Argv: []string{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.SoftwareUpdate", "AutomaticCheckEnabled", "-bool", "false"}},
	{ID: "updates-download-off", Step: "7d", Root: true, Argv: []string{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.SoftwareUpdate", "AutomaticDownload", "-bool", "false"}},
	{ID: "updates-macos-off", Step: "7d", Root: true, Argv: []string{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.SoftwareUpdate", "AutomaticallyInstallMacOSUpdates", "-bool", "false"}},

	// e. Remote Login off, last (§3.4 step 7e).
	{ID: "remote-login-off", Step: "7e", Event: "turning Remote Login off", Root: true, Argv: []string{"/usr/sbin/systemsetup", "-f", "-setremotelogin", "off"}, MayDropSession: true},
}

// steps are the §3.4 steps a row may cite.
var steps = []string{"6 D1(c)", "7a", "7b", "7c", "7d", "7e"}

// agentPlist is the guest agent's LaunchDaemon plist. Its only values are the
// shared constants, none of which needs XML escaping.
var agentPlist = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`, guestagent.MacAgentLabel, guestagent.MacAgentBinaryPath)

// argv is the command's whole argv, sudo included.
func (c command) argv() []string {
	if !c.Root {
		return c.Argv
	}
	return append(append([]string(nil), sudoArgv...), c.Argv...)
}

// line is the argv as the one string an SSH exec carries. Every word is a
// constant; the quoting only keeps the login shell from reading any of them.
func (c command) line() string {
	words := c.argv()
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = quote(w)
	}
	return strings.Join(out, " ")
}

func quote(w string) string {
	if w != "" && strings.Trim(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._=-") == "" {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// kcpasswordLen is the length of a kcpassword of an n-byte password.
func kcpasswordLen(n int) int { return (n + 1 + 11) / 12 * 12 }

// kcpassword is /etc/kcpassword's obfuscation of pw: pw and a NUL, padded
// with NULs to a multiple of 12 bytes, XORed with the fixed key.
// UNVERIFIED (external): the format on macOS 27.
func kcpassword(pw []byte) []byte {
	key := []byte{0x7D, 0x89, 0x52, 0x23, 0xD2, 0xBC, 0xDD, 0xEA, 0xA3, 0xB9, 0x1F}
	out := make([]byte, kcpasswordLen(len(pw)))
	copy(out, pw)
	for i := range out {
		out[i] ^= key[i%len(key)]
	}
	return out
}
