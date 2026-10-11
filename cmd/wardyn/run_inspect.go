// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The read-only `wardyn run` subcommands that inspect runs: list, get, kill and
// grants. Split from commands.go by seam; the create command and the wait logic stay there.

func runListCmd(client clientFn) *cobra.Command {
	var listJSON bool
	var listLimit, listOffset int
	list := &cobra.Command{
		Use:   "list",
		Short: "List all runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, truncated, err := client().ListRunsPage(cmd.Context(), listPageOptsAt(listLimit, listOffset)...)
			if err != nil {
				return err
			}
			warnListTruncated(cmd, truncated, "run", len(runs), listOffset)
			if listJSON {
				return emitJSON(cmd.OutOrStdout(), runs)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tAGENT\tREPO\tCC\tSTATE\tCREATED_BY\tCREATED")
			for _, r := range runs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.ID, r.Agent, r.Repo, r.ConfinementClass, r.State,
					r.CreatedBy, r.CreatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "emit raw JSON")
	list.Flags().IntVar(&listLimit, "limit", 0, "max rows to return (0 = server default page)")
	list.Flags().IntVar(&listOffset, "offset", 0, "skip this many rows (page forward past a truncated list)")
	return list
}

func runGetCmd(client clientFn) *cobra.Command {
	var getJSON bool
	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show one run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			run, err := client().GetRun(cmd.Context(), id)
			if err != nil {
				return err
			}
			if getJSON {
				return emitJSON(cmd.OutOrStdout(), run)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tAGENT\tREPO\tCC\tSTATE\tIMAGE\tCREATED")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				run.ID, run.Agent, run.Repo, run.ConfinementClass, run.State,
				run.Image, run.CreatedAt.Format(time.RFC3339))
			if err := tw.Flush(); err != nil {
				return err
			}
			// A FAILED run must not hide its reason on the default surface. Surface
			// the failure reason from audit inline; fall back to a pointer so the
			// user is never left with a bare "FAILED".
			if run.State == types.RunFailed {
				if reason := runFailureReason(cmd.Context(), client(), run.ID); reason != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "\nfailed: %s\n", reason)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "\nfailed — full detail: wardyn audit %s --json\n", run.ID)
				}
			}
			return nil
		},
	}
	get.Flags().BoolVar(&getJSON, "json", false, "emit raw JSON")
	return get
}

func runKillCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "kill <run-id>",
		Short: "Kill a run (tears down sandbox, revokes identity + credentials)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			if _, err := client().KillRun(cmd.Context(), id); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "kill requested for run %s\n", args[0])
			return nil
		},
	}
}

func runGrantsCmd(client clientFn) *cobra.Command {
	var grantsJSON bool
	grants := &cobra.Command{
		Use:   "grants <run-id>",
		Short: "List a run's credential-grant eligibility records (what it MAY request, not what was minted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			gs, err := client().ListGrants(cmd.Context(), id)
			if err != nil {
				return err
			}
			if grantsJSON {
				return emitJSON(cmd.OutOrStdout(), gs)
			}
			tw := newTab(cmd.OutOrStdout())
			// Full grant id, not short(): this is the row's own identity, not a
			// context column. APPROVAL is the load-bearing column — these are
			// ELIGIBILITY records, and a requires-approval grant mints nothing
			// until a human decides it.
			fmt.Fprintln(tw, "ID\tKIND\tAPPROVAL\tTTL\tSCOPE")
			for _, g := range gs {
				approval := "auto"
				if g.Spec.RequiresApproval {
					approval = "required"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%ds\t%s\n",
					g.ID, g.Spec.Kind, approval, g.Spec.TTLSeconds, orDash(string(g.Spec.Scope)))
			}
			return tw.Flush()
		},
	}
	grants.Flags().BoolVar(&grantsJSON, "json", false, "emit raw JSON")
	return grants
}
