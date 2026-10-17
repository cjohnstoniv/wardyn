// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runnerRevokeCmd revokes one of your own self-hosted runners, the command-line twin of the console's
// My runner page. A remote-hosted runner is the organisation's and is not yours to revoke.
func runnerRevokeCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <runner>",
		Short: "Revoke one of your own runners: it is disconnected and never admitted again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client()
			ref, err := resolveRunner(cmd, c, args[0], func() ([]sdk.RemoteRunnerView, error) { return nil, nil })
			if err != nil {
				return err
			}
			if ref.self == nil {
				return fmt.Errorf("%s is not one of your own runners; only a self-hosted runner you own can be revoked here", args[0])
			}
			if err := c.RevokeMyRunner(cmd.Context(), *ref.self); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked runner %s\n", args[0])
			return err
		},
	}
}
