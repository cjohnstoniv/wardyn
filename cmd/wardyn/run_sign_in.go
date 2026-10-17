// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runSignInCmd returns `wardyn run sign-in <run-id>`: whether an AWS sign-in
// run is waiting for its device-code approval, read once.
func runSignInCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "sign-in <run-id>",
		Short: "Show the device code an AWS sign-in run is waiting on",
		Long: `Show whether an AWS sign-in run is waiting for its device-code approval,
and if so the verification page and code of its latest attempt, so a sign-in
whose browser tab was lost can still be finished. Reads once.

Only the run's owner may read it. The page address comes from the sign-in
sandbox's terminal: check its host before opening it. A refusal exits non-zero
and names its reason.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			s, err := client().RunSignIn(cmd.Context(), id)
			if err != nil {
				var apiErr *sdk.APIError
				if errors.As(err, &apiErr) && apiErr.Reason != "" {
					return fmt.Errorf("%s: %w", apiErr.Reason, err)
				}
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), s)
			}
			out := cmd.OutOrStdout()
			if s.State != sdk.SignInWaiting {
				_, err = fmt.Fprintln(out, "not waiting: no sign-in on this run is waiting for approval")
				return err
			}
			_, err = fmt.Fprintf(out, "waiting: approve the sign-in on the verification page\n  page: %s\n  code: %s\n",
				escapeControls(s.VerificationURL), escapeControls(s.UserCode))
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the response body as JSON")
	return cmd
}
