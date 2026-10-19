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

// noSignInLine is what login says about signing in while no control plane offers a browser sign-in.
const noSignInLine = "This control plane does not offer CLI sign-in: set WARDYN_TOKEN to a personal API token. " +
	"A Wardyn admin can invite you to create one, and you claim it under Your account in the console."

// loginCmd is `wardyn login <url>`: where this computer's CLI points. The browser sign-in joins it in a later
// change; until then it saves the URL and says how to sign in without one.
func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login <url>",
		Short: "Point this computer's CLI at your organisation",
		Long: `Save your organisation's address in this computer's CLI profile (a file holding the URL and nothing secret).
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
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved %s. %s\nThe profile is %s; --url and WARDYN_URL still take precedence.\n",
				u, noSignInLine, filepath.Join(dir, cliprofile.FileName))
			return err
		},
	}
}
