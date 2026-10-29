// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// workload.go — `wardyn workload`: read and manage stored workloads by name, and the bindings that
// let a namespace's custom resources launch as a service identity. A workload is created from a
// file with `wardyn apply -f` (apply.go).
func workloadCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workload",
		Short: "List, read, restart and delete stored workloads, and bind namespaces to service identities",
		Long: "A workload is a named run that Wardyn keeps running. Create or change one with\n" +
			"`wardyn apply -f FILE`; these commands read it back and manage it by name.",
	}
	cmd.AddCommand(workloadListCmd(client), workloadGetCmd(client), workloadRestartCmd(client), workloadRemoveCmd(client), workloadBindingCmd(client))
	return subcommandGroup(cmd)
}

// ownerFlag is --owner, for an administrator or a service identity's sponsor reading another
// principal's workloads. Unset, the caller's own.
func ownerFlag(cmd *cobra.Command, owner *string) {
	cmd.Flags().StringVar(owner, "owner", "", "the workload's owner, when it is not yours: an administrator, or a service identity's sponsor, may name it")
}

func workloadListCmd(client clientFn) *cobra.Command {
	var owner string
	var limit, offset int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workloads with their state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows, err := client().ListWorkloads(cmd.Context(), owner, listPageOptsAt(limit, offset)...)
			if err != nil {
				return err
			}
			if asJSON {
				if rows == nil {
					rows = []sdk.Workload{}
				}
				return emitJSON(cmd.OutOrStdout(), rows)
			}
			return printWorkloads(cmd.OutOrStdout(), rows)
		},
	}
	ownerFlag(cmd, &owner)
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = server default page)")
	cmd.Flags().IntVar(&offset, "offset", 0, "skip this many rows")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

// printWorkloads is the list table. GENERATION is current/latest: they differ while a changed spec
// has not been launched, which a refusal explains in the last column.
func printWorkloads(w io.Writer, rows []sdk.Workload) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "NAME\tOWNER\tSOURCE\tSTATE\tGENERATION\tRUN\tREFUSED")
	for _, r := range rows {
		run, refused := "-", "-"
		if r.RunID != nil {
			run = r.RunID.String()
		}
		if r.LastRefusal != nil {
			refused = r.LastRefusal.Reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\n", r.Name, r.Owner, r.Source, orDash(r.State), r.CurrentGeneration, r.Generation, run, refused)
	}
	return tw.Flush()
}

func workloadGetCmd(client clientFn) *cobra.Command {
	var owner string
	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Print a workload (its document and status) as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := client().GetWorkload(cmd.Context(), args[0], owner)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), w)
		},
	}
	ownerFlag(cmd, &owner)
	return cmd
}

func workloadRestartCmd(client clientFn) *cobra.Command {
	var owner string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "restart NAME",
		Short: "Start a fresh run of the stored spec, then end the old one",
		Long: "Start a new run of the workload as stored, then end the previous run. Use it to start a workload\n" +
			"again after its run ended, or to replace a run that is still going. If the organisation refuses the\n" +
			"new run, the previous one keeps running and the command exits 1.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := client().RestartWorkload(cmd.Context(), args[0], owner)
			if err != nil {
				return err
			}
			printWorkloadResult(cmd, args[0], "restarted", res, asJSON)
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			}
			if res.Launch.State == sdk.WorkloadRefused {
				return &exitError{code: 1, err: fmt.Errorf("workload/%s was refused: the previous run keeps running", args[0])}
			}
			return nil
		},
	}
	ownerFlag(cmd, &owner)
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the result as JSON")
	return cmd
}

// workloadRemoveCmd is `workload delete NAME`; `wardyn delete -f` is the same by file.
func workloadRemoveCmd(client clientFn) *cobra.Command {
	var owner string
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "End a workload's run and delete the workload",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client().DeleteWorkload(cmd.Context(), args[0], owner); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "workload/%s deleted\n", args[0])
			return nil
		},
	}
	ownerFlag(cmd, &owner)
	return cmd
}

func workloadBindingCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "binding",
		Short: "Bind a namespace on a pool's runners to a service identity (security tier)",
		Long: "A binding lets the WardynWorkload custom resources of one namespace, on the cluster behind a runner\n" +
			"pool, launch runs as a service identity. It is standing authority, so it is made by a signed-in\n" +
			"person with the security role, never with an API token, and it ends when the identity's sponsor\n" +
			"changes. Bind only pools whose runners are all in that one cluster.",
	}
	cmd.AddCommand(workloadBindingCreateCmd(client), workloadBindingListCmd(client), workloadBindingDeleteCmd(client))
	return subcommandGroup(cmd)
}

func workloadBindingCreateCmd(client clientFn) *cobra.Command {
	var req sdk.CreateWorkloadBindingRequest
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Bind a namespace to a service identity on a runner pool",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := client().CreateWorkloadBinding(cmd.Context(), req)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), b)
			}
			return printWorkloadBindings(cmd.OutOrStdout(), []sdk.WorkloadBinding{b})
		},
	}
	cmd.Flags().StringVar(&req.Pool, "pool", "", "the runner pool, by id or name")
	cmd.Flags().StringVar(&req.Namespace, "namespace", "", "the Kubernetes namespace whose custom resources may launch")
	cmd.Flags().StringVar(&req.Service, "service", "", "the service identity the runs launch as")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	for _, f := range []string{"pool", "namespace", "service"} {
		_ = cmd.MarkFlagRequired(f)
	}
	return cmd
}

func workloadBindingListCmd(client clientFn) *cobra.Command {
	var limit, offset int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the workload bindings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows, err := client().ListWorkloadBindings(cmd.Context(), listPageOptsAt(limit, offset)...)
			if err != nil {
				return err
			}
			if asJSON {
				if rows == nil {
					rows = []sdk.WorkloadBinding{}
				}
				return emitJSON(cmd.OutOrStdout(), rows)
			}
			return printWorkloadBindings(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = server default page)")
	cmd.Flags().IntVar(&offset, "offset", 0, "skip this many rows")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func printWorkloadBindings(w io.Writer, rows []sdk.WorkloadBinding) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "ID\tPOOL\tNAMESPACE\tSERVICE\tSPONSOR\tCREATED")
	for _, b := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", b.ID, b.PoolID, b.Namespace, b.Service, b.Sponsor, b.CreatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

func workloadBindingDeleteCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "delete ID",
		Short: "Remove a binding; the runs it launched continue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("workload binding", args[0])
			if err != nil {
				return err
			}
			if err := client().DeleteWorkloadBinding(cmd.Context(), id); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "workload binding %s deleted\n", id)
			return nil
		},
	}
}
