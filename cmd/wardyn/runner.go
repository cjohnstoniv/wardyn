// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/spf13/cobra"
)

func runnerCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "runner", Short: "Register, claim and list runners, and set your runner pool defaults"}
	var stateDir, owner, pool string
	var makeDefault bool
	claim := &cobra.Command{Use: "claim", Short: "Claim the runner whose fingerprint is stored on this host, optionally into a pool", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if makeDefault && pool == "" {
			return errors.New("--default needs --pool")
		}
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
		if pool == "" {
			return nil
		}
		return claimIntoPool(cmd, c, registered.RunnerID, pool, makeDefault)
	}}
	claim.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory")
	claim.Flags().StringVar(&pool, "pool", "", "also put the runner in your user pool of this name (an id or a name), made if you have none by it")
	claim.Flags().BoolVar(&makeDefault, "default", false, "with --pool, also make that pool your default")
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
	cmd.AddCommand(claim, mint, runnerListCmd(client), runnerSetCmd(client), runnerRevokeCmd(client), runnerTokensCmd(client), runnerPoolCmd(client), runnerRemoteCmd(client))
	cmd.AddCommand(runnerOrgCmds(client)...)
	return subcommandGroup(cmd)
}
