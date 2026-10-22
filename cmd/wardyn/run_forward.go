// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// runForwardCmd returns `wardyn run forward <run-id> [local:]port...`: ssh -L
// to the sandbox's own loopback through the SSH gateway. It reaches nothing
// the gateway's -L does not already allow (docs/SSH.md §4).
func runForwardCmd(client clientFn) *cobra.Command {
	var advertisedProxy bool
	cmd := &cobra.Command{
		Use:   "forward <run-id> [<local-port>:]<sandbox-port>...",
		Short: "Forward local ports to a running sandbox over the SSH gateway",
		Long: `Forward ports on this computer to ports on the sandbox's own loopback over the
SSH gateway (WARDYN_SSH_LISTEN), with your registered SSH key, until Ctrl-C.

3000 forwards 127.0.0.1:3000 here to 127.0.0.1:3000 in the sandbox; 8080:3000
listens on 8080 here. The listener is bound to 127.0.0.1 only. The gateway
refuses any destination but the sandbox's loopback. A local port already in
use ends the command instead of leaving a forward that does not exist.

Without the gateway or a registered key it stops with one line saying so. The
key is made by 'wardyn ssh-key ensure', never by this command.
`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var forwards []string
			for _, spec := range args[1:] {
				l, err := parseForward(spec)
				if err != nil {
					return err
				}
				forwards = append(forwards, l)
			}
			acc, err := resolveSSHAccess(cmd.Context(), client(), "forward", args[0], advertisedProxy)
			if err != nil {
				return err
			}
			for _, f := range forwards {
				fmt.Fprintf(cmd.ErrOrStderr(), "forwarding %s (Ctrl-C to stop)\n", f)
			}
			return runSSHBinary(cmd.Context(), forwardSSHArgs(acc, args[0], forwards))
		},
	}
	cmd.Flags().BoolVar(&advertisedProxy, "advertised-proxy", false, "connect through the ProxyCommand this deployment advertises (it runs on this computer)")
	return cmd
}

// parseForward turns "R" or "L:R" into ssh's "127.0.0.1:L:127.0.0.1:R".
func parseForward(spec string) (string, error) {
	local, remote, found := strings.Cut(spec, ":")
	if !found {
		local, remote = spec, spec
	}
	for _, p := range []string{local, remote} {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 || strings.Trim(p, "0123456789") != "" {
			return "", fmt.Errorf("forward: %q is not <sandbox-port> or <local-port>:<sandbox-port> (ports are 1 to 65535)", spec)
		}
	}
	return "127.0.0.1:" + local + ":127.0.0.1:" + remote, nil
}

// forwardSSHArgs is ssh(1)'s argv for forward.
func forwardSSHArgs(acc sshAccess, runID string, forwards []string) []string {
	a := []string{"-N", "-o", "ExitOnForwardFailure=yes"}
	for _, f := range forwards {
		a = append(a, "-L", f)
	}
	return append(a, sshConnArgs(acc.gw, runID, acc.identity)...)
}
