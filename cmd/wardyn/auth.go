// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

// authCmd is `wardyn auth`: where this computer's CLI points. Sign-in joins it in a later change.
func authCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Point this computer's CLI at your organisation"}
	cmd.AddCommand(&cobra.Command{
		Use:   "set-url <url>",
		Short: "Save your organisation's address, so commands need no --url",
		Long: `Save your organisation's address in this computer's CLI profile (a file holding the URL and nothing else).
Commands then use it unless --url or WARDYN_URL names another. Run it once on a computer where the CLI was
installed on its own; a computer set up with wardyn-runnerd setup already has it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := cliprofile.Normalize(args[0])
			if err != nil {
				return err
			}
			// Advisory, like every command's token warning: a TLS-terminating proxy is legitimate, and the
			// profile holds no secret, so it is saved either way.
			if host := plaintextHost(u); host != "" && !cliutil.EnvBool("WARDYN_CLI_ALLOW_PLAINTEXT", false) {
				fmt.Fprintf(cmd.ErrOrStderr(), "wardyn: WARNING: %s is plain http://, so a token sent to it travels in cleartext — "+
					"use https://, or set WARDYN_CLI_ALLOW_PLAINTEXT=1 if TLS terminates in front of it\n", host)
			}
			dir, err := cliprofile.Dir()
			if err != nil {
				return err
			}
			if err := cliprofile.Write(dir, u); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wardyn now points at %s (saved in %s). --url and WARDYN_URL still take precedence.\n", u, filepath.Join(dir, cliprofile.FileName))
			return err
		},
	})
	return subcommandGroup(cmd)
}
