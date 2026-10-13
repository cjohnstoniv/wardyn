// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// invitation.go — the admin half of owner-redeemed invitations (#1508):
//
//	wardyn person invite <principal> [--expires-in 72h] [--token-name N] [--token-ttl D] [--replaces <token-id>]
//	wardyn person invitations <principal> [--state pending|claimed|expired|revoked]
//	wardyn invitation revoke <id>
//
// There is no claim verb: the target claims signed in, from the console's Account page.

func personInviteCmd(client clientFn) *cobra.Command {
	var expiresIn, tokenTTL time.Duration
	var tokenName, replaces string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "invite <principal>",
		Short: "Invite one person to create their own API token (security tier)",
		Long: `Invite one known person to create their own API token.

The invitation holds no secret. Only the person, signed in to the console as themselves,
can claim it (Account page), and the token is shown to them once; you never see it.
--replaces names a live token an admin created for them: claiming the invitation revokes it.
The invitation lasts 72 hours unless --expires-in says otherwise (at most 14 days).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := sdk.CreateInvitationRequest{
				ExpiresInSeconds: int64(expiresIn / time.Second), TokenName: tokenName, TokenTTLSeconds: int64(tokenTTL / time.Second),
			}
			if replaces != "" {
				id, err := parseID("token", replaces)
				if err != nil {
					return err
				}
				req.ReplacesTokenID = &id
			}
			inv, err := client().CreateInvitation(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), inv)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "invited %s: %s, expires %s\nthey claim it in the console at %s\n",
				inv.Target, inv.ID, inv.ExpiresAt.Format(time.RFC3339), inv.ConsolePath)
			return err
		},
	}
	cmd.Flags().DurationVar(&expiresIn, "expires-in", 0, "how long the invitation stays claimable (default 72h, at most 14 days)")
	cmd.Flags().StringVar(&tokenName, "token-name", "", "the name the token starts with; the person may change it")
	cmd.Flags().DurationVar(&tokenTTL, "token-ttl", 0, "limit the token's lifetime (default: the deployment's rule)")
	cmd.Flags().StringVar(&replaces, "replaces", "", "id of a live token an admin created for them, revoked when they claim")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func personInvitationsCmd(client clientFn) *cobra.Command {
	var state string
	var limit, offset int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "invitations <principal>",
		Short: "List one person's invitations (security tier)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, truncated, err := client().ListPersonInvitationsPage(cmd.Context(), args[0], sdk.InvitationState(state), listPageOptsAt(limit, offset)...)
			if err != nil {
				return err
			}
			warnListTruncated(cmd, truncated, "invitation", len(rows), offset)
			if asJSON {
				if rows == nil {
					rows = []sdk.Invitation{}
				}
				return emitJSON(cmd.OutOrStdout(), rows)
			}
			return printInvitations(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "pending, claimed, expired or revoked (default all)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = server default page)")
	cmd.Flags().IntVar(&offset, "offset", 0, "skip this many rows")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func printInvitations(w io.Writer, rows []sdk.Invitation) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "ID\tTARGET\tSTATE\tCREATED BY\tEXPIRES")
	for _, i := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", i.ID, i.Target, i.State, i.CreatedBy, i.ExpiresAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

// invitationCmd returns `wardyn invitation`: acts on an invitation by id.
func invitationCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "invitation", Short: "Act on an invitation to create an API token (security tier)"}
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a pending invitation",
		Long:  "Revoke a pending invitation. One that is missing, claimed, expired or already revoked is reported as not found.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("invalid invitation id %q: %w", args[0], err)
			}
			if err := client().RevokeInvitation(cmd.Context(), id); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked invitation %s\n", id)
			return err
		},
	})
	return subcommandGroup(cmd)
}
