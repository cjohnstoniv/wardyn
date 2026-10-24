// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// The macOS guest's side of the contract. The darwin server (server_darwin.go) is the only user of
// most of it; the parts that need no darwin API live here so they are tested on Linux.
const (
	// MacAgentUser is the standard (non-admin) account every exec runs as in a macOS guest, the
	// counterpart of AgentUID. Its numeric ids are looked up at start: macOS assigns them.
	MacAgentUser = "agent"
	MacAgentHome = "/Users/agent"

	// MacCABundlePath is where a run's proxy CA certificates land in a macOS guest. A managed file at
	// this path in Init makes the agent build MacCAFullPath and point the TLS tools' environment at
	// the two (CAEnv).
	MacCABundlePath = MacAgentHome + "/.wardyn/ca.pem"
	// MacCAFullPath is the system roots followed by MacCABundlePath, built at Init, for the tools whose
	// variable replaces their trust store rather than adding to it.
	MacCAFullPath = MacAgentHome + "/.wardyn/ca-bundle.pem"
	// MacSystemRoots is the macOS system root bundle.
	MacSystemRoots = "/etc/ssl/cert.pem"

	// MacAdminGID is macOS's admin group.
	MacAdminGID = 80

	// VsockCIDHost is the host's address for a guest-initiated vsock connection.
	VsockCIDHost = 2
	// VsockControlPort carries the agent protocol; VsockDesktopPort carries the desktop tunnel.
	VsockControlPort = 1024
	VsockDesktopPort = 1025

	// DesktopTarget is the only place the desktop tunnel dials: the guest's own Screen Sharing.
	DesktopTarget = "127.0.0.1:5900"
	// DesktopOpenByte is the first byte the host writes on the idle desktop connection when a viewer
	// connects. The agent drops it and only then dials DesktopTarget, because an RFB server speaks
	// first and an idle connection otherwise gives the agent nothing to tell it a viewer has come.
	DesktopOpenByte = 0x01
)

// caReplaceEnv are the variables whose value replaces the tool's trust store, so they name the full
// bundle (system roots plus the run's CA); caAddEnv adds to the tool's roots, so it names the CA alone.
// macOS has no supported way to add a root to the system trust programmatically, so a tool that reads
// the keychain (Safari, Go-built CLIs, Swift tools) does not see either file.
var (
	caReplaceEnv = []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "AWS_CA_BUNDLE", "GIT_SSL_CAINFO"}
	caAddEnv     = "NODE_EXTRA_CA_CERTS"
)

// CAEnv is KEY=VALUE for every CA variable: the replacing ones at fullPath, NODE_EXTRA_CA_CERTS at caPath.
func CAEnv(caPath, fullPath string) []string {
	out := []string{caAddEnv + "=" + caPath}
	for _, k := range caReplaceEnv {
		out = append(out, k+"="+fullPath)
	}
	return out
}

// isCAKey says whether k is one of the variables CAEnv sets.
func isCAKey(k string) bool {
	return k == caAddEnv || slices.Contains(caReplaceEnv, k)
}

// caEnvFor is CAEnv for the run's CA when files holds it, else nil.
func caEnvFor(files []File) []string {
	for _, f := range files {
		if f.Path == MacCABundlePath {
			return CAEnv(MacCABundlePath, MacCAFullPath)
		}
	}
	return nil
}

// initEnv is the environment Init gives every exec: env, then secret, then ca. The dispatcher already
// names these variables in Init's Env, with Linux guest paths, and on macOS they would point at files
// that are not there; so when ca is set, its values win over Init's for exactly the CA variables.
func initEnv(env, secret map[string]string, ca []string) []string {
	var out []string
	for _, m := range []map[string]string{env, secret} {
		for k, v := range m {
			if len(ca) > 0 && isCAKey(k) {
				continue
			}
			out = append(out, k+"="+v)
		}
	}
	return append(out, ca...)
}

// writeCAFull writes dst as the file at roots followed by ca, 0644 and root's: every exec reads it.
func writeCAFull(roots string, ca []byte, dst string) error {
	sys, err := os.ReadFile(roots)
	if err != nil {
		return fmt.Errorf("the system roots: %w", err)
	}
	var b bytes.Buffer
	b.Write(sys)
	if !bytes.HasSuffix(sys, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.Write(ca)
	tmp := dst + ".wardyn-tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// CmdRunner runs one command to completion and returns its stdout. The darwin code reaches launchctl,
// kickstart, osascript and id only through it, so a fake stands in for them in tests.
type CmdRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner is the CmdRunner that runs the command. A failure carries the command's stderr, never
// its arguments (one of them can be a password).
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// AgentIDs is the agent account's numeric ids and groups.
type AgentIDs struct {
	UID, GID uint32
	Groups   []uint32 // supplementary, without GID
}

// LookupAgentIDs asks id(1) for the account: os/user without cgo reads /etc/passwd, which does not
// hold a macOS account. It refuses an account that is uid 0 or in the admin group.
func LookupAgentIDs(ctx context.Context, r CmdRunner, user string) (AgentIDs, error) {
	var ids AgentIDs
	for _, flag := range []string{"-u", "-g", "-G"} {
		out, err := r.Run(ctx, "/usr/bin/id", flag, user)
		if err != nil {
			return AgentIDs{}, fmt.Errorf("the guest has no %q account: %w", user, err)
		}
		n, err := parseIDs(string(out))
		if err != nil || len(n) == 0 {
			return AgentIDs{}, fmt.Errorf("id %s %s: unexpected output %q", flag, user, strings.TrimSpace(string(out)))
		}
		switch flag {
		case "-u":
			ids.UID = n[0]
		case "-g":
			ids.GID = n[0]
		default:
			ids.Groups = nil
			for _, g := range n {
				if g != ids.GID {
					ids.Groups = append(ids.Groups, g)
				}
			}
		}
	}
	if ids.UID == 0 || ids.GID == MacAdminGID || slices.Contains(ids.Groups, MacAdminGID) {
		return AgentIDs{}, fmt.Errorf("%w: %q is uid %d, groups %v", ErrAgentPrivileged, user, ids.UID, append([]uint32{ids.GID}, ids.Groups...))
	}
	return ids, nil
}

// ErrAgentPrivileged is a guest whose agent account is root or an administrator: every exec runs as it,
// so the agent refuses to start rather than run untrusted work with that.
var ErrAgentPrivileged = errors.New("guestagent: the agent account is root or an administrator")

func parseIDs(s string) ([]uint32, error) {
	var out []uint32
	for _, f := range strings.Fields(s) {
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil {
			return nil, err
		}
		out = append(out, uint32(n))
	}
	return out, nil
}
