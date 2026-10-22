// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// workspaceSetUnitCmd pins a workspace to a unit, or unpins it with --none.
func workspaceSetUnitCmd(client clientFn) *cobra.Command {
	var none bool
	cmd := &cobra.Command{
		Use:   "set-unit <workspace-id> [<unit-id-or-name>] [--none]",
		Short: "Pin a workspace to a unit (a run that attaches it runs in that unit or one below), or unpin it",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("workspace", args[0])
			if err != nil {
				return err
			}
			if none == (len(args) == 2) {
				return fmt.Errorf("name a unit to pin, or pass --none to unpin, not both and not neither")
			}
			var unit *uuid.UUID
			if !none {
				u, err := resolveUnitArg(cmd, client, args[1])
				if err != nil {
					return err
				}
				unit = &u
			}
			ws, err := client().SetWorkspaceOwningUnit(cmd.Context(), id, unit)
			if err != nil {
				return err
			}
			if ws.OwningUnitID == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "workspace %s is not pinned to a unit\n", args[0])
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "workspace %s is pinned to unit %s (%s)\n", args[0], ws.OwningUnitName, ws.OwningUnitID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&none, "none", false, "unpin the workspace")
	return cmd
}

// resolveUnitArg is a unit's id, or the id of the unit holding that exact name (read from the
// governance document, which needs an admin).
func resolveUnitArg(cmd *cobra.Command, client clientFn, arg string) (uuid.UUID, error) {
	if id, err := uuid.Parse(arg); err == nil {
		return id, nil
	}
	doc, err := client().GetGovernance(cmd.Context())
	if err != nil {
		return uuid.Nil, fmt.Errorf("look up unit %q by name: %w", arg, err)
	}
	for _, u := range doc.Units {
		if u.Name == arg {
			return u.ID, nil
		}
	}
	return uuid.Nil, fmt.Errorf("no unit is named %q", arg)
}
