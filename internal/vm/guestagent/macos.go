// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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

	// MacCABundlePath is where a run's proxy CA bundle lands in a macOS guest. A managed file at this
	// path in Init makes the agent point the TLS tools' environment at it (CAEnv).
	MacCABundlePath = MacAgentHome + "/.wardyn/ca.pem"

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

// caEnvNames are the variables the common TLS stacks read for an extra or replacement CA file. macOS
// has no supported way to add a root to the system trust programmatically, so a tool that reads the
// keychain (Safari, Go-built CLIs, Swift tools) does not see the file.
var caEnvNames = []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "AWS_CA_BUNDLE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"}

// CAEnv is KEY=VALUE for every variable in caEnvNames, pointing at path.
func CAEnv(path string) []string {
	out := make([]string, len(caEnvNames))
	for i, k := range caEnvNames {
		out[i] = k + "=" + path
	}
	return out
}

// caEnvFor is CAEnv(MacCABundlePath) when files holds the CA bundle, else nil.
func caEnvFor(files []File) []string {
	for _, f := range files {
		if f.Path == MacCABundlePath {
			return CAEnv(MacCABundlePath)
		}
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
// hold a macOS account.
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
	return ids, nil
}

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
