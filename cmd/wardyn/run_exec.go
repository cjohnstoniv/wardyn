// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// runExecCmd returns `wardyn run exec <run-id> -- <command> [args...]`: one
// command in the sandbox over the SSH gateway, with ssh(1)'s exit status as
// its own. It needs the gateway on and a key on your account; it never
// creates the key.
func runExecCmd(client clientFn) *cobra.Command {
	var advertisedProxy bool
	cmd := &cobra.Command{
		Use:   "exec <run-id> -- <command> [args...]",
		Short: "Run one command in a running sandbox over the SSH gateway",
		Long: `Run one command in a RUNNING sandbox over the SSH gateway
(WARDYN_SSH_LISTEN) with your registered SSH key, and exit with its status.

Each word after -- reaches the sandbox as one argument: spaces and quotes in
a word are kept, and nothing on this computer interprets them. To use a pipe or
a redirect, run a shell: wardyn run exec <run-id> -- sh -c 'make test | tail'.

No terminal is allocated; standard input, output and error pass through.
Without the gateway or a registered key it stops with one line saying so. The
key is made by 'wardyn ssh-key ensure', never by this command.
`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
				return errors.New("exec: usage: wardyn run exec <run-id> -- <command> [args...]")
			}
			acc, err := resolveSSHAccess(cmd.Context(), client(), "exec", args[0], advertisedProxy)
			if err != nil {
				return err
			}
			return runSSHBinary(cmd.Context(), execSSHArgs(acc, args[0], args[1:]))
		},
	}
	cmd.Flags().BoolVar(&advertisedProxy, "advertised-proxy", false, "connect through the ProxyCommand this deployment advertises (it runs on this computer)")
	return cmd
}

// execSSHArgs is ssh(1)'s argv for exec. The gateway runs the command string
// with /bin/sh -c, so each word is quoted to arrive as exactly one argument.
func execSSHArgs(acc sshAccess, runID string, argv []string) []string {
	words := make([]string, len(argv))
	for i, w := range argv {
		words[i] = remoteWord(w)
	}
	return append(append([]string{"-T"}, sshConnArgs(acc.gw, runID, acc.identity)...), "--", strings.Join(words, " "))
}

var plainShellWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// remoteWord leaves a plain word bare and single-quotes any other.
func remoteWord(w string) string {
	if plainShellWord.MatchString(w) {
		return w
	}
	return shellQuote(w)
}
