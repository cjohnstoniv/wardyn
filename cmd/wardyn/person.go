// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// personCmd returns `wardyn person`: acts on a person's records that need the
// security tier.
func personCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "person",
		Short: "Act on a person's retained records (security tier)",
	}
	cmd.AddCommand(personEraseCmd(client))
	return subcommandGroup(cmd)
}

// personEraseCmd is POST /people/{principal}/erasure.
func personEraseCmd(client clientFn) *cobra.Command {
	var scopes []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "erase <principal>",
		Short: "Erase one person's retained records by scope",
		Long: `Erase one person's retained records, scope by scope, in one audited act.

--scope names what to erase, and may be repeated or comma-separated:

  credentials            their stored credentials and the key they sit under
  audit_personal_fields  their sealed audit fields (the key is destroyed; the chain still verifies)
  run_tasks              the task text of the runs they created
  run_outputs            the stored output of those runs
  recordings             their session recordings (nothing deletes one unless you ask)
  mask_copies            the masking manifests of those runs, after their live consumers are fenced

The scopes always run in the order above. The command exits non-zero unless every
scope finished; a failure names the scopes left, and running it again with the same
scopes finishes them. You cannot erase yourself except --scope credentials.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(scopes) == 0 {
				return fmt.Errorf("pass --scope with at least one scope to erase")
			}
			res, err := client().ErasePerson(cmd.Context(), args[0], scopes)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "erased %s for %s\n", strings.Join(res.Scopes, ", "), res.Person)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "scope to erase (repeatable): credentials, audit_personal_fields, run_tasks, run_outputs, recordings, mask_copies")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}
