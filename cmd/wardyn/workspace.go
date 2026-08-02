// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// workspaceCmd onboards and inspects workspaces. `create` is the load-bearing
// verb: a run whose policy names workspace_mounts/workspace_repos is refused
// (422) unless that source is already ONBOARDED — an un-bypassable gate that
// runs over inline, stored and default policies alike. Without this family the
// gate was only clearable from the console.
//
// The interactive import pipeline (approved-egress, llm-cred, setup commands,
// verify, finalize, suggest-fix) stays console-only: it is a review loop, not a
// scriptable step.
func workspaceCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspace",
		Aliases: []string{"workspaces"},
		Short:   "Onboard and inspect workspaces (a run may only mount/clone an onboarded source)",
	}

	var req sdk.WorkspaceRequest
	var createJSON bool
	create := &cobra.Command{
		Use:   "create --kind local_dir --source <path>",
		Short: "Onboard a workspace (this is what clears the run-create onboarding gate)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Default the name to the source: onboarding one directory should not
			// require inventing a label for it.
			if req.Name == "" {
				req.Name = req.Source
			}
			ws, err := client().CreateWorkspace(cmd.Context(), req)
			if err != nil {
				return err
			}
			if createJSON {
				return emitJSON(ws)
			}
			fmt.Printf("created workspace %s (%q, %s %s, status %s)\n", ws.ID, ws.Name, ws.Kind, ws.Source, ws.Status)
			return nil
		},
	}
	create.Flags().StringVar(&req.Name, "name", "", "human-readable name (defaults to --source)")
	create.Flags().StringVar((*string)(&req.Kind), "kind", "", "local_dir (a host directory), repo (a git slug/URL) or container (a base image ref)")
	create.Flags().StringVar(&req.Source, "source", "", "absolute host path, repo slug/clone URL, or image ref — validated by the same deny-list the run path uses")
	create.Flags().StringVar(&req.Ref, "ref", "", "git ref (branch/tag/sha); repo kind only")
	create.Flags().StringVar(&req.DefaultTarget, "target", "", "default in-container mount/clone target (must be under /home/agent, /work or /workspace)")
	create.Flags().BoolVar(&req.Writable, "writable", false, "mount READ-WRITE for import Record/Verify runs — a sandboxed agent's changes then PERSIST to the host directory (default read-only)")
	create.Flags().BoolVar(&createJSON, "json", false, "emit the created workspace as JSON")
	_ = create.MarkFlagRequired("kind")
	_ = create.MarkFlagRequired("source")

	var listJSON bool
	var listLimit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List onboarded workspaces",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wss, err := client().ListWorkspaces(cmd.Context(), listPageOpts(listLimit)...)
			if err != nil {
				return err
			}
			if listJSON {
				return emitJSON(wss)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tNAME\tKIND\tSOURCE\tSTATUS")
			for _, ws := range wss {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ws.ID, ws.Name, ws.Kind, ws.Source, ws.Status)
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "emit raw JSON")
	list.Flags().IntVar(&listLimit, "limit", 0, "max rows to return (0 = server default page)")

	get := &cobra.Command{
		Use:   "get <workspace-id>",
		Short: "Show one workspace (full row, including its scan profile) as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("workspace", args[0])
			if err != nil {
				return err
			}
			ws, err := client().GetWorkspace(cmd.Context(), id)
			if err != nil {
				return err
			}
			return emitJSON(ws)
		},
	}

	del := &cobra.Command{
		Use:     "delete <workspace-id>",
		Aliases: []string{"rm"},
		Short:   "Delete a workspace (runs can no longer mount/clone its source)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("workspace", args[0])
			if err != nil {
				return err
			}
			if err := client().DeleteWorkspace(cmd.Context(), id); err != nil {
				return err
			}
			fmt.Printf("workspace %s deleted\n", args[0])
			return nil
		},
	}

	scan := &cobra.Command{
		Use:   "scan <workspace-id>",
		Short: "Scan a workspace to derive its least-privilege profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("workspace", args[0])
			if err != nil {
				return err
			}
			// The reply shape varies (a completed profile vs an accepted async
			// run), so emit it raw rather than pretending it is one shape.
			raw, err := client().ScanWorkspace(cmd.Context(), id)
			if err != nil {
				return err
			}
			return emitJSON(raw)
		},
	}

	cmd.AddCommand(create, list, get, del, scan)
	return cmd
}
