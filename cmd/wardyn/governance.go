// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// governance.go — read/replace the admin-authored governance profiles and
// their subject assignments (migration 0052), the same get-then-set shape
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
//	wardyn governance set governance.json      # restore, or hand-authored additions
//
// `set` UPSERTS every profile and assignment the file names, over the
// existing POST/PUT /governance/profiles and POST /governance/assignments
// routes — there is no bulk-write route, and this command adds none. A
// PROFILE is upserted BY NAME (its unique human handle), not by id — see
// ApplyGovernance's doc comment for why that, not id-routing, is what makes a
// hand-maintained, version-controlled file re-apply as a no-op. An
// ASSIGNMENT is upserted by its own natural key (subject_type, subject).
// Nothing the file omits is touched, and nothing is deleted, unless --prune is
// passed — so `get` immediately followed by `set` is a no-op, the same round
// trip drive get/set's pair promises.
//
// Where the deployment requires a second approver for governance writes, a
// write is held as a pending change rather than applied: `set` prints the
// pending and deferred lists on stderr (stdout stays the document) and exits
// 0, because pending is the expected outcome there. `changes list|approve|
// reject` is the second approver's side.
func governanceCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "governance",
		Short: "Get or replace the governance profiles and their subject assignments",
		Long: "Read or upsert the governance profiles (named, assignable ceilings) and the\n" +
			"assignments binding them to a user, a group, or everyone:\n\n" +
			"    wardyn governance get > governance.json\n" +
			"    wardyn governance set governance.json\n\n" +
			"`set` upserts every profile and assignment the file names — a profile is\n" +
			"upserted BY NAME (its unique handle), an assignment by its own natural key\n" +
			"(subject_type, subject). A profile in the file but absent server-side is\n" +
			"created; one present server-side but absent from the file is left alone unless\n" +
			"--prune is passed, which also deletes any server-side assignment the file omits.\n" +
			"`wardyn governance get > f && wardyn governance set f` is a no-op.\n\n" +
			"Where governance writes need a second approver, `set` holds them as pending\n" +
			"changes (it exits 0 and lists them); a second human then runs\n" +
			"`wardyn governance changes approve <id>`.",
	}
	cmd.AddCommand(governanceGetCmd(client), governanceSetCmd(client), governanceChangesCmd(client))
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

func governanceSetCmd(client clientFn) *cobra.Command {
	var prune bool
	cmd := &cobra.Command{
		Use:   "set [file]",
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
			// Strict decode, the same shape drive set and site-config set
			// both take: a key this file mistypes must surface as a parse
			// error, not silently vanish from what ApplyGovernance then sends.
			var doc sdk.GovernanceDocument
			if err := decodeOneJSONStrict(bytes.NewReader(raw), &doc); err != nil {
				return fmt.Errorf("parse governance document JSON: %w", err)
			}
			res, err := client().ApplyGovernanceResult(cmd.Context(), doc, prune)
			if err != nil {
				return err
			}
			printGovernancePending(cmd.ErrOrStderr(), res)
			return emitJSON(cmd.OutOrStdout(), res.Document)
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false,
		"also delete every server-side profile and assignment the file omits (default: leave them alone)")
	return cmd
}

// printGovernancePending writes what an apply held for approval to w, nothing
// when nothing is pending. It goes to stderr so stdout stays a document that
// `governance set` can strict-decode again.
func printGovernancePending(w io.Writer, res sdk.GovernanceApplyResult) {
	if len(res.Pending) == 0 {
		return
	}
	fmt.Fprintf(w, "pending approval: %d change(s) stored, not applied; a second approver runs `wardyn governance changes approve <id>`\n", len(res.Pending))
	for _, ch := range res.Pending {
		fmt.Fprintf(w, "  %s  %s %s %s  proposed by %s  expires %s\n", ch.ID, ch.Op, ch.TargetKind, ch.TargetKey, ch.ProposedBy, ch.ExpiresAt.Format(time.RFC3339))
	}
	var profiles, assignments []sdk.GovernanceDeferredWrite
	for _, d := range res.Deferred {
		if d.Base != "" {
			profiles = append(profiles, d)
		} else {
			assignments = append(assignments, d)
		}
	}
	if len(profiles) > 0 {
		fmt.Fprintf(w, "deferred: %d profile(s) not sent, the profile they compose on is pending; apply again once it is approved\n", len(profiles))
		for _, d := range profiles {
			fmt.Fprintf(w, "  profile %q -> base %q\n", d.Profile, d.Base)
		}
	}
	if len(assignments) > 0 {
		fmt.Fprintf(w, "deferred: %d assignment(s) not sent, their profile is pending; apply again once it is approved\n", len(assignments))
		for _, d := range assignments {
			fmt.Fprintf(w, "  %s %q -> profile %q\n", d.SubjectType, d.Subject, d.Profile)
		}
	}
	if res.PruneSkipped {
		fmt.Fprintln(w, "prune skipped: a write is pending; apply again with --prune once it is decided")
	}
}

func governanceChangesCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "changes",
		Short: "List, approve or reject governance changes held for a second approver",
	}
	var state string
	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List governance changes (optionally filtered by --state)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			changes, err := client().ListGovernanceChanges(cmd.Context(), state)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), changes)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tSTATE\tOP\tKIND\tTARGET\tPROPOSED BY\tEXPIRES")
			for _, ch := range changes {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					ch.ID, ch.State, ch.Op, ch.TargetKind, ch.TargetKey, ch.ProposedBy, ch.ExpiresAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&state, "state", "", "filter by state: pending, applied, rejected, expired or stale (default: the server's)")
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")

	approve := &cobra.Command{
		Use:   "approve <change-id>",
		Short: "Approve a pending governance change, which applies it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("governance change", args[0])
			if err != nil {
				return err
			}
			ch, err := client().ApproveGovernanceChange(cmd.Context(), id)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "governance change %s -> %s\n", ch.ID, ch.State)
			return nil
		},
	}

	var reason string
	reject := &cobra.Command{
		Use:   "reject <change-id>",
		Short: "Reject a pending governance change; nothing is applied",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("governance change", args[0])
			if err != nil {
				return err
			}
			ch, err := client().RejectGovernanceChange(cmd.Context(), id, reason)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "governance change %s -> %s\n", ch.ID, ch.State)
			return nil
		},
	}
	reject.Flags().StringVar(&reason, "reason", "", "reason recorded with the rejection")
	cmd.AddCommand(list, approve, reject)
	return subcommandGroup(cmd)
}
