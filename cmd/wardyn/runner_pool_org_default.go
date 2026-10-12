// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool_org_default.go — the organisation's default pool and default barrier, which an
// administrator sets.
//
//	wardyn runner pool org-default get
//	wardyn runner pool org-default set <pool-id> [--barrier CC2]
//	wardyn runner pool org-default set --barrier CC2
//
// The default pool is an organization pool: a run that names no pool, from a person with no default
// of their own, starts there; `none` clears it. An untouched launch gets the default barrier when it is
// at or above the caller's floor and the pool offers it; `--barrier none` clears it, so the strongest
// offered class applies again. Whatever is not named is kept.
func runnerPoolOrgDefaultCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "org-default", Short: "Get or set the organisation's default pool and barrier (admin)"}
	get := &cobra.Command{Use: "get", Short: "Print the organisation's defaults as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := client().GetOrgRunnerPoolDefaults(cmd.Context())
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), d)
	}}
	var barrier string
	set := &cobra.Command{
		Use:   "set [<pool-id>|none] [--barrier CC1|CC2|CC3|none]",
		Short: "Set the organisation's default pool or barrier, keeping what you do not name",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if barrier == "" && len(args) == 0 {
				return fmt.Errorf("name an organization pool id (or none), --barrier CC1, CC2, CC3 or none, or both")
			}
			c := client()
			d, err := c.GetOrgRunnerPoolDefaults(cmd.Context())
			if err != nil {
				return err
			}
			if len(args) == 1 {
				d.PoolID = nil
				if args[0] != "none" {
					if d.PoolID, err = poolIDFlag("pool id", args[0]); err != nil {
						return err
					}
				}
			}
			switch barrier {
			case "":
			case "none":
				d.Barrier = ""
			default:
				d.Barrier = sdk.ConfinementClass(barrier)
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
