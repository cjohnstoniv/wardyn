// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool.go — the person-facing half of the runner pool contract: list the
// pools you may use and keep your own default pool (runner_pool_manage.go keeps
// the pools themselves). A server that does not manage
// pools yet answers each with runner_pools_unavailable.
//
//	wardyn runner pool list
//	wardyn runner pool default set <pool>
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
	cmd := &cobra.Command{Use: "pool", Short: "List the runner pools you may use, keep your own user pools and set your own default pool"}
	var asJSON, all bool
	list := &cobra.Command{Use: "list", Short: "List the runner pools you may use", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		out, err := client().ListRunnerPools(cmd.Context())
		if err != nil {
			return err
		}
		if asJSON {
			return emitJSON(cmd.OutOrStdout(), out)
		}
		return printRunnerPools(cmd.OutOrStdout(), out, all)
	}}
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	list.Flags().BoolVar(&all, "all", false, "every row the server returned, with the owner of each user pool (an operator or security admin sees every person's)")
	cmd.AddCommand(list, runnerPoolDefaultCmd(client), runnerPoolOrgDefaultCmd(client), runnerPoolUsePolicyCmd(client))
	cmd.AddCommand(runnerPoolManageCmds(client)...)
	return subcommandGroup(cmd)
}

// manageOnly is a row the caller may only manage, an administrator's oversight row of someone else's
// pool: it is not theirs to use or to put a runner in.
func manageOnly(p sdk.RunnerPoolChoice) bool {
	return p.Availability == sdk.RunnerPoolUnavailable && p.Reason == runnerpool.ReasonNotFound
}

// printRunnerPools is the list: ID NAME TYPE AVAILABILITY BARRIERS. A row the caller may only manage (availability
// unavailable, reason runner_pool_not_found) is left out unless all is set, and then an OWNER column
// is added.
func printRunnerPools(w io.Writer, list sdk.RunnerPoolList, all bool) error {
	tw := newTab(w)
	head := "ID\tNAME\tTYPE\tAVAILABILITY\tBARRIERS"
	if all {
		head = "ID\tNAME\tTYPE\tOWNER\tAVAILABILITY\tBARRIERS"
	}
	fmt.Fprintln(tw, head)
	for _, p := range list.Pools {
		if !all && manageOnly(p.RunnerPoolChoice) {
			continue
		}
		availability := p.Availability
		if p.Reason != "" {
			availability += " (" + string(p.Reason) + ")"
		}
		if all {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Scope, p.Owner, availability, barrierSummary(p.BarrierOptions))
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Scope, availability, barrierSummary(p.BarrierOptions))
		}
	}
	return tw.Flush()
}

// barrierSummary names each barrier with * on the default and the reason one is not available, e.g.
// "CC1 (below_floor) CC2* CC3 (not_offered: kvm_missing)"; "-" for a row that carries none.
func barrierSummary(opts []sdk.BarrierOption) string {
	if len(opts) == 0 {
		return "-"
	}
	cells := make([]string, 0, len(opts))
	for _, o := range opts {
		cell := string(o.Class)
		if o.Default {
			cell += "*"
		}
		if !o.Available {
			reason := string(o.ReasonCode)
			if o.ReasonDetail != "" {
				reason += ": " + o.ReasonDetail
			}
			cell += " (" + reason + ")"
		}
		cells = append(cells, cell)
	}
	return strings.Join(cells, " ")
}

func runnerPoolDefaultCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "default", Short: "Get, set or clear your own default pool"}
	get := &cobra.Command{Use: "get", Short: "Print your own default pool as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := client().GetMyRunnerPoolDefaults(cmd.Context())
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), d)
	}}
	set := &cobra.Command{
		Use:   "set <pool>",
		Short: "Make a pool you may use your default",
		Long: "Make a pool you may use, an organization pool or one of your own user pools, your default:\n" +
			"a run that names no pool starts there. Use `clear` to use the organisation's default again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := poolIDArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			out, err := client().SetMyRunnerPoolDefaults(cmd.Context(), sdk.RunnerPoolDefaults{PoolID: &id})
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), out)
		},
	}
	clear := &cobra.Command{Use: "clear", Short: "Remove your own default pool so the organisation's applies", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return client().ClearMyRunnerPoolDefaults(cmd.Context())
	}}
	cmd.AddCommand(get, set, clear)
	return subcommandGroup(cmd)
}
