// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool_org_default.go — the organisation's default barrier, which an administrator sets.
//
//	wardyn runner pool org-default get
//	wardyn runner pool org-default set --barrier CC2
//
// An untouched launch on any pool gets it when it is at or above the caller's floor and the pool
// offers it; `--barrier none` clears it, so the strongest offered class applies again.
func runnerPoolOrgDefaultCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "org-default", Short: "Get or set the organisation's default barrier (admin)"}
	get := &cobra.Command{Use: "get", Short: "Print the organisation's defaults as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := client().GetOrgRunnerPoolDefaults(cmd.Context())
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), d)
	}}
	var barrier string
	set := &cobra.Command{
		Use:   "set --barrier CC1|CC2|CC3|none",
		Short: "Set the organisation's default barrier, keeping its default pools",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if barrier == "" {
				return fmt.Errorf("name --barrier CC1, CC2, CC3 or none")
			}
			c := client()
			d, err := c.GetOrgRunnerPoolDefaults(cmd.Context())
			if err != nil {
				return err
			}
			d.Barrier = sdk.ConfinementClass(barrier)
			if barrier == "none" {
				d.Barrier = ""
			}
			if err := d.Validate(); err != nil {
				return err
			}
			out, err := c.SetOrgRunnerPoolDefaults(cmd.Context(), d)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), out)
		},
	}
	set.Flags().StringVar(&barrier, "barrier", "", "default barrier: CC1, CC2, CC3, or none to clear it")
	cmd.AddCommand(get, set)
	return subcommandGroup(cmd)
}
