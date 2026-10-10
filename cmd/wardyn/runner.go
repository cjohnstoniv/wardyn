// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/spf13/cobra"
)

func runnerCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "runner", Short: "Register and claim your runner, and set your runner pool defaults"}
	var stateDir, owner string
	claim := &cobra.Command{Use: "claim", Short: "Claim the runner whose fingerprint is stored on this host", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if stateDir == "" {
			var err error
			stateDir, err = runneridentity.DefaultDir()
			if err != nil {
				return err
			}
		}
		c := client()
		identity, err := runneridentity.Load(stateDir, c.BaseURL)
		if err != nil {
			return err
		}
		registered, err := c.ClaimRunner(cmd.Context(), identity.RunnerID, identity.Fingerprint)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Claimed runner %s with fingerprint %s\n", registered.RunnerID, identity.Fingerprint)
		return nil
	}}
	claim.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory")
	mint := &cobra.Command{Use: "token", Short: "Mint a single-use runner registration token", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c := client()
		if err := federation.CheckOrgURL(c.BaseURL); err != nil {
			return err
		}
		token, err := c.MintRunnerToken(cmd.Context(), owner)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Single-use registration token expires %s; registration still requires the owner's fingerprint claim.\n", token.ExpiresAt.Format(time.RFC3339))
		fmt.Fprintln(cmd.OutOrStdout(), token.Token)
		return nil
	}}
	mint.Flags().StringVar(&owner, "owner", "", "person's principal (operator only); omit for yourself")
	cmd.AddCommand(claim, mint, runnerPoolCmd(client))
	return subcommandGroup(cmd)
}
