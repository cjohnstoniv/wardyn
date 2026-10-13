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

// service_identity.go — service identities (#1508): a named, sponsored non-human principal.
//
//	wardyn service-identity create --name N --sponsor P --user-type T [--description D]   security tier
//	wardyn service-identity list | get <name> | update <name> | retire <name>              security tier
//	wardyn service-identity token list <name> | revoke <name> <id>                         the sponsor
//	wardyn token move <id> <name>                                                          security tier
//
// There is no verb that mints a service token: the sponsor mints it in the console, signed in.

func serviceIdentityCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service-identity",
		Short: "Create and manage service identities (security tier)",
		Long: `A service identity is automation that runs under its own name, not a person's. Its sponsor, a
signed-in person, creates its tokens in the console and can see and stop its runs.`,
	}
	cmd.AddCommand(serviceIdentityCreateCmd(client), serviceIdentityListCmd(client), serviceIdentityGetCmd(client),
		serviceIdentityUpdateCmd(client), serviceIdentityRetireCmd(client), serviceIdentityTokenCmd(client))
	return subcommandGroup(cmd)
}

func printServiceIdentity(cmd *cobra.Command, si sdk.ServiceIdentity, asJSON bool) error {
	if asJSON {
		return emitJSON(cmd.OutOrStdout(), si)
	}
	return printServiceIdentities(cmd.OutOrStdout(), []sdk.ServiceIdentity{si})
}

func printServiceIdentities(w io.Writer, rows []sdk.ServiceIdentity) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "NAME\tSTATE\tSPONSOR\tUSER TYPE\tCREATED\tDESCRIPTION")
	for _, s := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.State, s.Sponsor, s.UserType, s.CreatedAt.Format(time.RFC3339), s.Description)
	}
	return tw.Flush()
}

func serviceIdentityCreateCmd(client clientFn) *cobra.Command {
	var req sdk.CreateServiceIdentityRequest
	var asJSON bool
	cmd := &cobra.Command{
		Use: "create", Short: "Create a service identity", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			si, err := client().CreateServiceIdentity(cmd.Context(), req)
			if err != nil {
				return err
			}
			return printServiceIdentity(cmd, si, asJSON)
		},
	}
	cmd.Flags().StringVar(&req.Name, "name", "", "lowercase letters, digits and hyphens, 3 to 63 characters, starting with a letter")
	cmd.Flags().StringVar(&req.Sponsor, "sponsor", "", "the signed-in person who creates its tokens and sees its runs")
	cmd.Flags().StringVar(&req.UserType, "user-type", "", "an existing user type; it sets what the identity may do")
	cmd.Flags().StringVar(&req.Description, "description", "", "what it is for, at most 200 characters")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func serviceIdentityListCmd(client clientFn) *cobra.Command {
	var limit, offset int
	var asJSON bool
	cmd := &cobra.Command{
		Use: "list", Short: "List service identities with their state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows, truncated, err := client().ListServiceIdentitiesPage(cmd.Context(), listPageOptsAt(limit, offset)...)
			if err != nil {
				return err
			}
			warnListTruncated(cmd, truncated, "service identity", len(rows), offset)
			if asJSON {
				if rows == nil {
					rows = []sdk.ServiceIdentity{}
				}
				return emitJSON(cmd.OutOrStdout(), rows)
			}
			return printServiceIdentities(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = server default page)")
	cmd.Flags().IntVar(&offset, "offset", 0, "skip this many rows")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func serviceIdentityGetCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "get <name>", Short: "Show one service identity", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			si, err := client().GetServiceIdentity(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printServiceIdentity(cmd, si, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func serviceIdentityUpdateCmd(client clientFn) *cobra.Command {
	var description, sponsor string
	var suspend, resume, asJSON bool
	cmd := &cobra.Command{
		Use: "update <name>", Short: "Change a service identity's description, sponsor or suspended state",
		Long: `Change what you name: --description, --sponsor, and --suspend or --resume.
Changing the sponsor revokes every token of the identity.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if suspend && resume {
				return fmt.Errorf("pass --suspend or --resume, not both")
			}
			var req sdk.UpdateServiceIdentityRequest
			flags := cmd.Flags()
			if flags.Changed("description") {
				req.Description = &description
			}
			if flags.Changed("sponsor") {
				req.Sponsor = &sponsor
			}
			if suspend || resume {
				req.Suspended = &suspend
			}
			if req.Description == nil && req.Sponsor == nil && req.Suspended == nil {
				return fmt.Errorf("name what to change: --description, --sponsor, --suspend or --resume")
			}
			si, err := client().UpdateServiceIdentity(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			return printServiceIdentity(cmd, si, asJSON)
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "what it is for, at most 200 characters")
	cmd.Flags().StringVar(&sponsor, "sponsor", "", "the new sponsor; revokes every token of the identity")
	cmd.Flags().BoolVar(&suspend, "suspend", false, "refuse its tokens until resumed")
	cmd.Flags().BoolVar(&resume, "resume", false, "accept its tokens again")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func serviceIdentityRetireCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "retire <name>", Short: "Retire a service identity for good, revoking its tokens", Args: cobra.ExactArgs(1),
		Long: `Retire a service identity: its tokens are revoked and its stored credentials erased. It cannot be undone.
Running it again finishes an erase that failed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			si, err := client().RetireServiceIdentity(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printServiceIdentity(cmd, si, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

// serviceIdentityTokenCmd is the sponsor's read and revoke of an identity's tokens. Minting stays in the
// console: it needs the sponsor's signed-in session, so the SDK method has no verb here.
func serviceIdentityTokenCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "List or revoke the tokens of a service identity you sponsor",
		Long:  "List or revoke the tokens of a service identity you sponsor. Minting a service token happens in the console, signed in as the sponsor.",
	}
	var limit, offset int
	var asJSON bool
	list := &cobra.Command{
		Use: "list <name>", Short: "List an identity's tokens (never their values)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, truncated, err := client().ListServiceTokensPage(cmd.Context(), args[0], listPageOptsAt(limit, offset)...)
			if err != nil {
				return err
			}
			warnListTruncated(cmd, truncated, "token", len(rows), offset)
			if asJSON {
				if rows == nil {
					rows = []sdk.APIToken{}
				}
				return emitJSON(cmd.OutOrStdout(), rows)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tNAME\tCREATED\tEXPIRES\tREVOKED")
			for _, t := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.CreatedAt.Format(time.RFC3339), tokenDay(t.ExpiresAt, "never"), tokenDay(t.RevokedAt, "-"))
			}
			return tw.Flush()
		},
	}
	list.Flags().IntVar(&limit, "limit", 0, "max rows (0 = server default page)")
	list.Flags().IntVar(&offset, "offset", 0, "skip this many rows")
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	revoke := &cobra.Command{
		Use: "revoke <name> <token-id>", Short: "Revoke one token of an identity", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("token", args[1])
			if err != nil {
				return err
			}
			if err := client().RevokeServiceToken(cmd.Context(), args[0], id); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked token %s of %s\n", id, args[0])
			return err
		},
	}
	cmd.AddCommand(list, revoke)
	return subcommandGroup(cmd)
}

func tokenDay(t *time.Time, none string) string {
	if t == nil {
		return none
	}
	return t.Format(time.RFC3339)
}

// tokenCmd returns `wardyn token`: acts on an admin-minted API token by id.
func tokenCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Act on an API token an admin created (security tier)"}
	var asJSON bool
	move := &cobra.Command{
		Use: "move <id> <service-identity>", Short: "Link a token to the service identity that replaces it",
		Long: `Link a live token an admin created for someone else to the service identity that replaces it.
The token keeps working: this records the choice and revokes nothing. Revoke it yourself once the job uses
the service identity's own token.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("invalid token id %q: %w", args[0], err)
			}
			moved, err := client().MoveToken(cmd.Context(), id, args[1])
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), moved)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "token %s is %s (service identity %s)\n", moved.TokenID, moved.Disposition, moved.ServiceIdentity)
			return err
		},
	}
	move.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	cmd.AddCommand(move)
	return subcommandGroup(cmd)
}
