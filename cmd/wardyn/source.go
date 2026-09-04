// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// parseAttachArg parses one --attach SOURCE-ID[@TARGET][:ro|:rw] argument:
// which library source to attach, an optional in-container target, and the
// per-ATTACHMENT writability (default read-only — the same safe default the
// mount path uses; ":rw" is the explicit opt-in, dirs only).
func parseAttachArg(s string) (id uuid.UUID, target string, writable bool, err error) {
	rest := s
	if cut, ok := strings.CutSuffix(rest, ":rw"); ok {
		rest, writable = cut, true
	} else if cut, ok := strings.CutSuffix(rest, ":ro"); ok {
		rest = cut
	}
	idPart, targetPart, _ := strings.Cut(rest, "@")
	id, err = uuid.Parse(idPart)
	if err != nil {
		return uuid.Nil, "", false, fmt.Errorf("invalid --attach %q: want SOURCE-ID[@TARGET][:ro|:rw] (find ids with `wardyn source list`)", s)
	}
	return id, targetPart, writable, nil
}

// sourceContractSummary counts a source's own requirement rows for the list
// table — enough to see "this repo declares things" at a glance without
// dumping the contract (use --json / `source list --json` for the rows).
func sourceContractSummary(src sdk.Source) string {
	if len(src.Requirements) == 0 {
		return "-"
	}
	return pluralCount(len(src.Requirements), "req")
}

// sourceCmd manages the tier-1 SOURCE LIBRARY: a repo/dir configured once —
// its own requirements contract, its own scan profile/status — and attached to
// any number of workspaces (`workspace create --attach <source-id>`). Creating
// a source that already exists lands on the existing row (dedupe on canonical
// identity), which is the reuse the library exists for.
//
// Interactive review (the per-source contract editor, per-attachment
// overrides) stays console-only, same posture as the workspace review loop.
func sourceCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "source",
		Aliases: []string{"sources"},
		Short:   "Manage the shared source library (repos/dirs configured once, attached to many workspaces)",
	}

	var listJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List library sources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			srcs, err := client().ListSources(cmd.Context())
			if err != nil {
				return err
			}
			if listJSON {
				return emitJSON(srcs)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tKIND\tLOCATOR\tREF\tSTATUS\tCONTRACT")
			for _, src := range srcs {
				ref := src.Ref
				if ref == "" {
					ref = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", src.ID, src.Kind, src.Locator, ref, src.Status, sourceContractSummary(src))
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "emit raw JSON (includes each source's contract rows)")

	var req sdk.SourceRequest
	var createJSON bool
	create := &cobra.Command{
		Use:   "create --kind repo --locator <slug/URL> [--ref <git-ref>]",
		Short: "Add a source to the library (idempotent: an existing identity returns the existing row)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := client().CreateSource(cmd.Context(), req)
			if err != nil {
				return err
			}
			if createJSON {
				return emitJSON(src)
			}
			fmt.Printf("source %s (%s %s, status %s)\n", src.ID, src.Kind, src.Locator, src.Status)
			return nil
		},
	}
	create.Flags().StringVar((*string)(&req.Kind), "kind", "", "local_dir (a host directory) or repo (a git slug/URL)")
	create.Flags().StringVar(&req.Locator, "locator", "", "absolute host path or repo slug/clone URL — validated by the same deny-list the run path uses")
	create.Flags().StringVar(&req.Ref, "ref", "", "git ref (branch/tag/sha); repo kind only — part of the identity")
	create.Flags().StringVar(&req.Name, "name", "", "human-readable name (defaults to the locator's last path segment)")
	create.Flags().BoolVar(&createJSON, "json", false, "emit the source as JSON")
	// W6-S1-6: fail locally with cobra's own "required flag(s)" message
	// instead of round-tripping an empty locator to the server for a 400.
	_ = create.MarkFlagRequired("kind")
	_ = create.MarkFlagRequired("locator")

	scan := &cobra.Command{
		Use:   "scan <source-id>",
		Short: "Scan a source to derive its profile (dir inline; repo as a governed run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("source", args[0])
			if err != nil {
				return err
			}
			// The reply shape varies (a completed profile vs an accepted async
			// run), so emit it raw rather than pretending it is one shape.
			raw, err := client().ScanSource(cmd.Context(), id)
			if err != nil {
				return err
			}
			return emitJSON(raw)
		},
	}

	var delForce bool
	del := &cobra.Command{
		Use:     "delete <source-id>",
		Aliases: []string{"rm"},
		Short:   "Remove a library source (409 naming the attaching workspaces; --force detaches them)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("source", args[0])
			if err != nil {
				return err
			}
			detachedFrom, err := client().DeleteSource(cmd.Context(), id, delForce)
			if err != nil {
				return err
			}
			fmt.Printf("source %s deleted\n", args[0])
			if len(detachedFrom) > 0 {
				fmt.Printf("  detached from: %s (those workspaces stop mounting this source; their next runs succeed without it)\n",
					strings.Join(detachedFrom, ", "))
			}
			return nil
		},
	}
	del.Flags().BoolVar(&delForce, "force", false, "detach from every workspace still using it (those workspaces stop mounting this source; their next runs succeed without it)")

	cmd.AddCommand(list, create, scan, del)
	return subcommandGroup(cmd)
}
