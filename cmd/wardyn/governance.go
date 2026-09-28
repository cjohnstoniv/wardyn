// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// governance.go — read/replace the admin-authored governance profiles and
// their subject assignments (migration 0052), the same get-then-apply shape
// drive.go established for the drive family: `subcommandGroup`, `emitJSON`,
// strict decoding, and `-` meaning stdin.
//
// Why this exists as a CLI: pkg/client/client.go's Coverage census named
// /api/v1/governance as SDK-uncovered — an admin who rebuilds an install, or
// runs one whose database is disposable, had no way to script or version
// control a profile and its assignments, even though the console's whole
// Governance screen is one GET away (#1108).
//
//	wardyn governance get > governance.json      # a snapshot, or before a reset
//	wardyn governance apply governance.json      # restore, or hand-authored additions
//
// `apply` UPSERTS every profile and assignment the file names, over the
// existing POST/PUT /governance/profiles and POST /governance/assignments
// routes — there is no bulk-write route, and this command adds none. A
// PROFILE is upserted BY NAME (its unique human handle), not by id — see
// ApplyGovernance's doc comment for why that, not id-routing, is what makes a
// hand-maintained, version-controlled file re-apply as a no-op. An
// ASSIGNMENT is upserted by its own natural key (subject_type, subject).
// Nothing the file omits is touched, and nothing is deleted, unless --prune is
// passed — so `get` immediately followed by `apply` is a no-op, the same round
// trip drive get/apply's pair promises.
func governanceCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "governance",
		Short: "Get or replace the governance profiles and their subject assignments",
		Long: "Read or upsert the governance profiles (named, assignable ceilings) and the\n" +
			"assignments binding them to a user, a group, or everyone:\n\n" +
			"    wardyn governance get > governance.json\n" +
			"    wardyn governance apply governance.json\n\n" +
			"`apply` upserts every profile and assignment the file names — a profile is\n" +
			"upserted BY NAME (its unique handle), an assignment by its own natural key\n" +
			"(subject_type, subject). A profile in the file but absent server-side is\n" +
			"created; one present server-side but absent from the file is left alone unless\n" +
			"--prune is passed, which also deletes any server-side assignment the file omits.\n" +
			"`wardyn governance get > f && wardyn governance apply f` is a no-op.",
	}
	cmd.AddCommand(governanceGetCmd(client), governanceApplyCmd(client))
	return subcommandGroup(cmd)
}

func governanceGetCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print every governance profile and assignment as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := client().GetGovernance(cmd.Context())
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), doc)
		},
	}
}

func governanceApplyCmd(client clientFn) *cobra.Command {
	var prune bool
	cmd := &cobra.Command{
		Use:   "apply [file]",
		Short: "Upsert the governance profiles and assignments in a JSON file (or stdin with '-')",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := "-"
			if len(args) == 1 {
				src = args[0]
			}
			var raw []byte
			var err error
			if src == "-" {
				raw, err = io.ReadAll(cmd.InOrStdin())
			} else {
				raw, err = os.ReadFile(src)
			}
			if err != nil {
				return fmt.Errorf("read governance document: %w", err)
			}
			// Strict decode, the same shape drive apply and site-config set
			// both take: a key this file mistypes must surface as a parse
			// error, not silently vanish from what ApplyGovernance then sends.
			var doc sdk.GovernanceDocument
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&doc); err != nil {
				return fmt.Errorf("parse governance document JSON: %w", err)
			}
			out, err := client().ApplyGovernance(cmd.Context(), doc, prune)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), out)
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false,
		"also delete every server-side profile and assignment the file omits (default: leave them alone)")
	return cmd
}
