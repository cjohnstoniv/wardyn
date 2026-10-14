// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_inventory.go — the runner management half of `wardyn runner`:
//
//	wardyn runner list [--all [--state active|revoked|all]]   your own runners; --all is every person's (admin)
//	wardyn runner tokens list [--owner P] | revoke <id>        the unused registration tokens (admin)
//	wardyn runner set --max-runs N <id>                        your own runner's runs at once

func runnerListCmd(client clientFn) *cobra.Command {
	var all, asJSON bool
	var state string
	var limit, offset int
	cmd := &cobra.Command{Use: "list", Short: "List your own runners, or with --all every person's", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var rows []sdk.RunnerView
		var truncated bool
		var err error
		switch {
		case all:
			rows, truncated, err = client().ListRunnersPage(cmd.Context(), sdk.RunnerFilter(state), listPageOptsAt(limit, offset)...)
		case state != "":
			return fmt.Errorf("--state applies to --all; your own list is your unclaimed and claimed runners, plus any revoked by someone else in the last 7 days")
		default:
			rows, err = client().ListMyRunners(cmd.Context())
		}
		if err != nil {
			return err
		}
		warnListTruncated(cmd, truncated, "runner", len(rows), offset)
		if asJSON {
			if rows == nil {
				rows = []sdk.RunnerView{}
			}
			return emitJSON(cmd.OutOrStdout(), rows)
		}
		if all {
			return printRunners(cmd.OutOrStdout(), rows)
		}
		remote, err := remoteRunnersToAdd(cmd.Context(), client())
		if err != nil {
			return err
		}
		return printOwnRunners(cmd.OutOrStdout(), rows, remote)
	}}
	cmd.Flags().BoolVar(&all, "all", false, "every person's runners (admin or security_admin)")
	cmd.Flags().StringVar(&state, "state", "", "with --all: active (default), revoked or all")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	cmd.Flags().IntVar(&limit, "limit", 0, "with --all: max rows (0 = server default page)")
	cmd.Flags().IntVar(&offset, "offset", 0, "with --all: skip this many rows")
	return cmd
}

func runnerStateWord(r sdk.RunnerView) string {
	switch {
	case r.State != "claimed":
		return string(r.State)
	case r.Online:
		return "online"
	}
	return "offline"
}

// printRunners is the --all table: every person's runners, with the columns an admin reads a runner by,
// then the same resource columns as your own list.
func printRunners(w io.Writer, rows []sdk.RunnerView) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "ID\tOWNER\tNAME\tMACHINE\tSTATE\tLAST SEEN\tRUNS\tAT ONCE\tFINGERPRINT\t"+resourceHeads)
	for _, r := range rows {
		lastSeen := "never"
		if r.LastSeenAt != nil {
			lastSeen = r.LastSeenAt.Format(time.RFC3339)
		}
		atOnce := fmt.Sprintf("%d/-", r.Usage.InUse.Runs)
		if r.Limits != nil {
			atOnce = fmt.Sprintf("%d/%d", r.Usage.InUse.Runs, r.Limits.RunsAtOnce)
		}
		cells := resourceCells(r.Limits, r.Usage)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", r.ID, r.Owner, r.Name, cmp.Or(r.MachineName, "-"), runnerStateWord(r), lastSeen,
			r.Usage.InUse.Runs, atOnce, r.KeyFingerprint, strings.Join(cells[1:], "\t"))
	}
	return tw.Flush()
}

func runnerTokensCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "tokens", Short: "List or revoke unused runner registration tokens (admin)"}
	var asJSON bool
	var owner string
	var limit, offset int
	list := &cobra.Command{Use: "list", Short: "List registration tokens not yet used, revoked or expired", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		tokens, truncated, err := client().ListRunnerTokensPage(cmd.Context(), owner, listPageOptsAt(limit, offset)...)
		if err != nil {
			return err
		}
		warnListTruncated(cmd, truncated, "registration token", len(tokens), offset)
		if asJSON {
			if tokens == nil {
				tokens = []sdk.RunnerRegistrationToken{}
			}
			return emitJSON(cmd.OutOrStdout(), tokens)
		}
		tw := newTab(cmd.OutOrStdout())
		fmt.Fprintln(tw, "ID\tFOR\tMINTED BY\tCREATED\tEXPIRES")
		for _, t := range tokens {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Owner, t.MintedBy, t.CreatedAt.Format(time.RFC3339), t.ExpiresAt.Format(time.RFC3339))
		}
		return tw.Flush()
	}}
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	list.Flags().StringVar(&owner, "owner", "", "only this person's tokens")
	list.Flags().IntVar(&limit, "limit", 0, "max rows (0 = server default page)")
	list.Flags().IntVar(&offset, "offset", 0, "skip this many rows")
	revoke := &cobra.Command{Use: "revoke <id>", Short: "Revoke a registration token that has not been used yet", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := uuid.Parse(args[0])
		if err != nil {
			return fmt.Errorf("registration token id: %w", err)
		}
		if err := client().RevokeRunnerToken(cmd.Context(), id); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "revoked registration token %s\n", id)
		return nil
	}}
	cmd.AddCommand(list, revoke)
	return subcommandGroup(cmd)
}
