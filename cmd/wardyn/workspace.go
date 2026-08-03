// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore SA1019 The CLI still accepts --kind/--source/--writable and
// sends them as the deprecated scalars; the server folds them into one source.
// Reading them here is the compatibility path, not an oversight.

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// parseWorkspaceSourceArg parses one --add TYPE:VALUE[@TARGET] argument into a
// WorkspaceSource. TYPE is "dir" (local_dir), "repo", or "ephemeral"; VALUE is
// the host path / repo slug (empty for ephemeral); an optional "@TARGET"
// suffix sets the in-container mount/clone/scratch path. There is no
// per-source --ref/--writable in this grammar — onboard a single source
// needing those with --kind/--source/--ref/--writable instead.
func parseWorkspaceSourceArg(s string) (sdk.WorkspaceSource, error) {
	typ, rest, ok := strings.Cut(s, ":")
	if !ok {
		return sdk.WorkspaceSource{}, fmt.Errorf("invalid --add %q: want TYPE:VALUE[@TARGET]", s)
	}
	value, target, _ := strings.Cut(rest, "@")
	switch typ {
	case "dir", "local_dir":
		if value == "" {
			return sdk.WorkspaceSource{}, fmt.Errorf("invalid --add %q: dir needs a host path", s)
		}
		return sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeLocalDir, Path: value, Target: target}, nil
	case "repo":
		if value == "" {
			return sdk.WorkspaceSource{}, fmt.Errorf("invalid --add %q: repo needs a slug/URL", s)
		}
		return sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeRepo, Source: value, Target: target}, nil
	case "ephemeral":
		return sdk.WorkspaceSource{Type: sdk.WorkspaceSourceTypeEphemeral, Target: target}, nil
	default:
		return sdk.WorkspaceSource{}, fmt.Errorf("invalid --add %q: type must be dir, repo, or ephemeral", s)
	}
}

// firstWorkspaceSourceLabel is the human-meaningful value of one source: the
// host path (local_dir), the repo slug/URL (repo), or its target (ephemeral,
// which has neither).
func firstWorkspaceSourceLabel(src sdk.WorkspaceSource) string {
	switch src.Type {
	case sdk.WorkspaceSourceTypeLocalDir:
		return src.Path
	case sdk.WorkspaceSourceTypeRepo:
		return src.Source
	default:
		return src.Target
	}
}

// workspaceComposition renders a workspace's composition for CLI display: the
// single source's type+value when there's exactly one (matching how a
// single-source row always rendered), or a "N dirs · N repos · N ephemeral"
// summary for a multi-source row — never an empty Kind, which a multi-source
// workspace's derived mirror always leaves blank.
func workspaceComposition(ws sdk.Workspace) string {
	if len(ws.Sources) == 1 {
		return string(ws.Sources[0].Type) + " " + firstWorkspaceSourceLabel(ws.Sources[0])
	}
	var dirs, repos, ephemeral int
	for _, src := range ws.Sources {
		switch src.Type {
		case sdk.WorkspaceSourceTypeLocalDir:
			dirs++
		case sdk.WorkspaceSourceTypeRepo:
			repos++
		case sdk.WorkspaceSourceTypeEphemeral:
			ephemeral++
		}
	}
	var parts []string
	if dirs > 0 {
		parts = append(parts, pluralCount(dirs, "dir"))
	}
	if repos > 0 {
		parts = append(parts, pluralCount(repos, "repo"))
	}
	if ephemeral > 0 {
		parts = append(parts, pluralCount(ephemeral, "ephemeral"))
	}
	if len(parts) == 0 {
		return "no sources"
	}
	return strings.Join(parts, " · ")
}

func pluralCount(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// workspaceCmd onboards and inspects workspaces. `create` is the load-bearing
// verb: a run whose policy names workspace_mounts/workspace_repos is refused
// (422) unless that source is already ONBOARDED — an un-bypassable gate that
// runs over inline, stored and default policies alike. Without this family the
// gate was only clearable from the console.
//
// Interactive workspace review — the requirements contract, approved-egress
// promotion, llm-cred binding, Record sessions and their confined replay, and
// env-as-code — stays console-only: it is a review loop, not a scriptable step.
func workspaceCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspace",
		Aliases: []string{"workspaces"},
		Short:   "Onboard and inspect workspaces (a run may only mount/clone an onboarded source)",
	}

	var req sdk.WorkspaceRequest
	var createJSON bool
	var addSources []string
	create := &cobra.Command{
		Use:   "create --kind local_dir --source <path> [--add TYPE:VALUE[@TARGET] ...]",
		Short: "Onboard a workspace (this is what clears the run-create onboarding gate)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, a := range addSources {
				src, err := parseWorkspaceSourceArg(a)
				if err != nil {
					return err
				}
				req.Sources = append(req.Sources, src)
			}
			// Default the name to the source (legacy shape) or the first --add
			// source: onboarding one directory should not require inventing a
			// label for it.
			if req.Name == "" {
				switch {
				case req.Source != "":
					req.Name = req.Source
				case len(req.Sources) > 0:
					req.Name = firstWorkspaceSourceLabel(req.Sources[0])
				}
			}
			ws, err := client().CreateWorkspace(cmd.Context(), req)
			if err != nil {
				return err
			}
			if createJSON {
				return emitJSON(ws)
			}
			fmt.Printf("created workspace %s (%q, %s, status %s)\n", ws.ID, ws.Name, workspaceComposition(ws), ws.Status)
			return nil
		},
	}
	create.Flags().StringVar(&req.Name, "name", "", "human-readable name (defaults to --source, or the first --add source)")
	create.Flags().StringVar((*string)(&req.Kind), "kind", "", "local_dir (a host directory), repo (a git slug/URL) or container (a base image ref) — a single legacy source; mutually exclusive with --add")
	create.Flags().StringVar(&req.Source, "source", "", "absolute host path, repo slug/clone URL, or image ref — validated by the same deny-list the run path uses")
	create.Flags().StringVar(&req.Ref, "ref", "", "git ref (branch/tag/sha); repo kind only")
	create.Flags().StringVar(&req.DefaultTarget, "target", "", "default in-container mount/clone target (must be under /home/agent, /work or /workspace)")
	create.Flags().BoolVar(&req.Writable, "writable", false, "mount READ-WRITE so a run attaching this source can PERSIST changes to the host directory (default read-only)")
	create.Flags().StringArrayVar(&addSources, "add", nil, `add one composition source, repeatable: "dir:/host/path[@/target]", "repo:org/lib[@/target]", or "ephemeral:[@/target]" (mutually exclusive with --kind/--source/--ref/--target/--writable; no per-source --ref/--writable in this form)`)
	create.Flags().BoolVar(&createJSON, "json", false, "emit the created workspace as JSON")

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
			fmt.Fprintln(tw, "ID\tNAME\tCOMPOSITION\tSTATUS")
			for _, ws := range wss {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ws.ID, ws.Name, workspaceComposition(ws), ws.Status)
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "emit raw JSON")
	list.Flags().IntVar(&listLimit, "limit", 0, "max rows to return (0 = server default page)")

	getJSON := true
	get := &cobra.Command{
		Use:   "get <workspace-id>",
		Short: "Show one workspace (full row, including its scan profile)",
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
			if getJSON {
				return emitJSON(ws)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tNAME\tCOMPOSITION\tSTATUS")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ws.ID, ws.Name, workspaceComposition(ws), ws.Status)
			return tw.Flush()
		},
	}
	get.Flags().BoolVar(&getJSON, "json", true, "emit raw JSON (--json=false for a one-line composition table)")

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
