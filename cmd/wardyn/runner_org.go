// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerOrgCmds are a security operator's organization runner commands: mint a token for one, claim the
// runner that redeemed it, and set any runner's trust class.
func runnerOrgCmds(client clientFn) []*cobra.Command {
	var name, fingerprint, reason string
	orgToken := subcommandGroup(&cobra.Command{Use: "org-token", Short: "Registration tokens for an organization's runner (security operator)"})
	mint := &cobra.Command{Use: "mint", Short: "Mint a single-use, 72 h registration token for an organization's runner", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c := client()
		if err := federation.CheckOrgURL(c.BaseURL); err != nil {
			return err
		}
		token, err := c.MintOrgRunnerToken(cmd.Context(), name)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Single-use organization runner token expires %s; a security operator then claims the runner with `wardyn runner claim-org`.\n", token.ExpiresAt.Format(time.RFC3339))
		fmt.Fprintln(cmd.OutOrStdout(), token.Token)
		return nil
	}}
	mint.Flags().StringVar(&name, "name", "", "a label for the token, kept in its audit row (required)")
	_ = mint.MarkFlagRequired("name")
	orgToken.AddCommand(mint)
	claimOrg := &cobra.Command{Use: "claim-org <runner-id>", Short: "Claim an organization's runner by the fingerprint it printed at registration (security operator, personal session)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := uuid.Parse(args[0])
		if err != nil {
			return fmt.Errorf("runner id %q is not a UUID", args[0])
		}
		registered, err := client().ClaimOrgRunner(cmd.Context(), id, fingerprint)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Claimed organization runner %s with fingerprint %s\n", registered.RunnerID, fingerprint)
		return nil
	}}
	claimOrg.Flags().StringVar(&fingerprint, "fingerprint", "", "the runner's key fingerprint, as wardyn-runnerd printed it at registration (required)")
	_ = claimOrg.MarkFlagRequired("fingerprint")
	trust := &cobra.Command{Use: "trust <runner-id> <class>", Short: "Set a runner's trust class: personal or managed_device (an organization's runner is always org_cluster)", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := uuid.Parse(args[0])
		if err != nil {
			return fmt.Errorf("runner id %q is not a UUID", args[0])
		}
		class := types.RunnerTrustClass(args[1])
		if !class.Valid() {
			return errors.New("the trust class is personal or managed_device (an organization's runner is always org_cluster)")
		}
		view, err := client().SetRunnerTrust(cmd.Context(), id, class, reason)
		if held, err := heldForApproval(cmd, err); held || err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Runner %s (%s) trust class is %s\n", view.Name, view.ID, view.TrustClass)
		return nil
	}}
	trust.Flags().StringVar(&reason, "reason", "", "why, kept in the audit row (required)")
	_ = trust.MarkFlagRequired("reason")
	return []*cobra.Command{orgToken, claimOrg, trust}
}
