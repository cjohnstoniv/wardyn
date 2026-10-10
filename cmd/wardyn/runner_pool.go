// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool.go — the person-facing half of the runner pool contract: list the
// pools you may use and keep your own defaults. A server that does not manage
// pools yet answers each with runner_pools_unavailable.
//
//	wardyn runner pool list
//	wardyn runner pool default set --hosting self_hosted --self-hosted-pool <id>
//
// A default only seeds a choice; it never grants a pool you could not choose.

// poolIDFlag parses a pool id flag client-side so a typo names its flag.
func poolIDFlag(flag, s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parse %s %q: %w", flag, s, err)
	}
	return &id, nil
}

func runnerPoolCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "pool", Short: "List the runner pools you may use and set your own defaults"}
	var asJSON bool
	list := &cobra.Command{Use: "list", Short: "List the runner pools you may use", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		out, err := client().ListRunnerPools(cmd.Context())
		if err != nil {
			return err
		}
		if asJSON {
			return emitJSON(cmd.OutOrStdout(), out)
		}
		return printRunnerPools(cmd.OutOrStdout(), out)
	}}
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	cmd.AddCommand(list, runnerPoolDefaultCmd(client))
	return subcommandGroup(cmd)
}

func printRunnerPools(w io.Writer, list sdk.RunnerPoolList) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "ID\tNAME\tHOSTING\tAVAILABILITY")
	for _, p := range list.Pools {
		availability := p.Availability
		if p.Reason != "" {
			availability += " (" + string(p.Reason) + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.ID, p.Name, p.HostingType, availability)
	}
	return tw.Flush()
}

func runnerPoolDefaultCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "default", Short: "Get, set or clear your own default hosting type and pools"}
	get := &cobra.Command{Use: "get", Short: "Print your own defaults as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := client().GetMyRunnerPoolDefaults(cmd.Context())
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), d)
	}}
	var hosting, remote, self string
	set := &cobra.Command{
		Use:   "set",
		Short: "Replace your own defaults",
		Long: "Replace your own defaults with exactly the flags given; a field you leave out\n" +
			"inherits the organisation's. Use `clear` to inherit everything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := sdk.RunnerPoolDefaults{PreferredHosting: sdk.RunnerPoolHosting(hosting)}
			var err error
			if d.RemoteProvided, err = poolIDFlag("--remote-pool", remote); err != nil {
				return err
			}
			if d.SelfHosted, err = poolIDFlag("--self-hosted-pool", self); err != nil {
				return err
			}
			if d == (sdk.RunnerPoolDefaults{}) {
				return fmt.Errorf("name at least one of --hosting, --remote-pool, --self-hosted-pool, or use `clear`")
			}
			if err := d.Validate(); err != nil {
				return err
			}
			out, err := client().SetMyRunnerPoolDefaults(cmd.Context(), d)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), out)
		},
	}
	set.Flags().StringVar(&hosting, "hosting", "", "preferred hosting type: remote_provided or self_hosted")
	set.Flags().StringVar(&remote, "remote-pool", "", "default Remote Provided pool id")
	set.Flags().StringVar(&self, "self-hosted-pool", "", "default Self-Hosted pool id")
	clear := &cobra.Command{Use: "clear", Short: "Remove your own defaults so the organisation's apply", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return client().ClearMyRunnerPoolDefaults(cmd.Context())
	}}
	cmd.AddCommand(get, set, clear)
	return subcommandGroup(cmd)
}
