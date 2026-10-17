// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// peopleCmd returns `wardyn people`: the people this deployment knows, for an admin who needs to
// see who has access before acting on a leaver.
func peopleCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "people",
		Short: "See who can reach this deployment",
	}
	cmd.AddCommand(peopleListCmd(client))
	return subcommandGroup(cmd)
}

func peopleListCmd(client clientFn) *cobra.Command {
	var opts sdk.PeopleListOpts
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the people this deployment knows, with their role and what they hold",
		Long: `List everyone who has signed in and everyone an admin set up beforehand, one page at a time.

Each row shows the person's role as the role mappings resolve it for their email,
when they signed in, and what they hold: a live session (0 or 1: sessions are
stateless, so this says whether the last sign-in could still hold one), API
tokens, SSH keys, stored credentials and runs still going. Those counts are what
the leaver actions act on.

A full page prints the cursor for the next one on stderr; pass it as --cursor.
Needs the security tier.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			page, err := client().ListPeople(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if asJSON {
				if page.People == nil {
					page.People = []sdk.PersonSummary{}
				}
				return emitJSON(cmd.OutOrStdout(), page)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "PRINCIPAL\tEMAIL\tISSUER\tROLE\tSTATE\tLAST SIGN-IN\tSESSIONS\tTOKENS\tSSH KEYS\tCREDENTIALS\tRUNS")
			for _, p := range page.People {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\n", p.Principal, p.Email, p.IssuerKind, p.Role,
					personState(p), personDay(p.LastSignedInAt), p.ActiveSessions, p.APITokens, p.SSHKeys, p.Credentials, p.ActiveRuns)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if page.NextCursor != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "more people: --cursor %s\n", page.NextCursor)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "page size (default 50, at most 200)")
	cmd.Flags().StringVar(&opts.Cursor, "cursor", "", "the cursor a previous page printed")
	cmd.Flags().StringVar(&opts.Query, "q", "", "keep people whose principal or email starts with this")
	cmd.Flags().StringVar(&opts.State, "state", "", "active or deactivated (default both)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the page as JSON, with next_cursor")
	return cmd
}

// personState is the STATE column: a deactivated person, one set up who has not signed in yet,
// or an ordinary active one.
func personState(p sdk.PersonSummary) string {
	switch {
	case p.DeactivatedAt != nil:
		return "deactivated"
	case p.LastSignedInAt == nil && p.PreCreated:
		return "pre-created"
	}
	return "active"
}

func personDay(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Format("2006-01-02")
}
