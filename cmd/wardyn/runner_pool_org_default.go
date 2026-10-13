// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool_org_default.go — the organization's default pool and default barrier, which an admin
// sets.
//
//	wardyn runner pool org-default get
//	wardyn runner pool org-default set --pool <pool>|none [--barrier CC1|CC2|CC3|none]
//	wardyn runner pool org-default clear
//
// The default pool is an organization pool: a run that names no pool, from a person with no default of
// their own, starts there. An untouched launch gets the default barrier when it is at or above the
// caller's floor and the pool offers it; `--barrier none` clears it, so the strongest offered class
// applies again. Whatever is not named is kept.
func runnerPoolOrgDefaultCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "org-default", Short: "Get, set or clear the organization's default pool and barrier (admin)"}
	get := &cobra.Command{Use: "get", Short: "Print the organization's defaults as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := client().GetOrgRunnerPoolDefaults(cmd.Context())
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), d)
	}}
	var pool, barrier string
	set := &cobra.Command{
		Use:   "set --pool <pool>|none [--barrier CC1|CC2|CC3|none]",
		Short: "Set the organization's default pool or barrier, keeping what you do not name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pool == "" && barrier == "" {
				return fmt.Errorf("name --pool (an organization pool, or none), --barrier (CC1, CC2, CC3 or none), or both")
			}
			c := client()
			d, err := c.GetOrgRunnerPoolDefaults(cmd.Context())
			if err != nil {
				return err
			}
			switch pool {
			case "":
			case "none":
				d.PoolID = nil
			default:
				p, err := readPoolArg(cmd, client, pool)
				if err != nil {
					return err
				}
				if p.Scope != sdk.RunnerPoolOrganization {
					return fmt.Errorf("there is no organization default for user pools; people choose their own")
				}
				d.PoolID = &p.ID
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
	set.Flags().StringVar(&pool, "pool", "", "the default pool: an organization pool's id or name, or none")
	set.Flags().StringVar(&barrier, "barrier", "", "default barrier: CC1, CC2, CC3, or none to clear it")
	clear := &cobra.Command{Use: "clear", Short: "Clear the organization's default pool, keeping its barrier", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c := client()
		d, err := c.GetOrgRunnerPoolDefaults(cmd.Context())
		if err != nil {
			return err
		}
		d.PoolID = nil
		if _, err := c.SetOrgRunnerPoolDefaults(cmd.Context(), d); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "organization default cleared; people choose an organization pool unless they saved their own")
		return err
	}}
	cmd.AddCommand(get, set, clear)
	return subcommandGroup(cmd)
}
