// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// template_admin.go — the administrator's half of run templates:
//
//	wardyn template group-admin list
//	wardyn template group-admin grant <group> <person> [--label TEXT]
//	wardyn template group-admin revoke <group> <person>
//	wardyn template from-preset <name> [--everyone]
//
// Grants are CLI and API only; there is no console page for them.

func templateGroupAdminCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group-admin",
		Short: "List, grant and revoke who manages a group's templates (administrators)",
		Long: "A group grant lets one person publish and edit the templates of one group, and\n" +
			"nothing else: it is not an administrator role and group membership does not\n" +
			"imply it. The person is a name or email the identity directory knows; an email\n" +
			"that matches nobody, or more than one person, is refused. The group is written\n" +
			"as your sign-in provider names it, and the same group name can be reused later\n" +
			"by someone else, so revoke a grant when its holder no longer manages the group.",
	}
	cmd.AddCommand(templateGroupAdminListCmd(client), templateGroupAdminGrantCmd(client), templateGroupAdminRevokeCmd(client))
	return subcommandGroup(cmd)
}

func templateGroupAdminListCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every group template grant",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			grants, err := client().ListTemplateGroupAdmins(cmd.Context())
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), grants)
		},
	}
}

func templateGroupAdminGrantCmd(client clientFn) *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "grant <group> <person>",
		Short: "Let a person manage a group's templates (--label names the group)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			grant, err := client().GrantTemplateGroupAdmin(cmd.Context(), sdk.TemplateGroupAdmin{Group: args[0], Person: args[1], Label: label})
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), grant)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "the group's display name in the console (display only: the grant follows the group, not the label)")
	return cmd
}

func templateGroupAdminRevokeCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <group> <person>",
		Short: "Take back a group template grant (revoking one that is not there succeeds)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client().RevokeTemplateGroupAdmin(cmd.Context(), sdk.TemplateGroupAdmin{Group: args[0], Person: args[1]}); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "revoked %s from %s\n", args[1], args[0])
			return err
		},
	}
}

func templateFromPresetCmd(client clientFn) *cobra.Command {
	var everyone bool
	cmd := &cobra.Command{
		Use:   "from-preset <name>",
		Short: "Publish a launch preset to the organisation as a template (administrators)",
		Long: "Convert a launch preset into a run template and save it to the organisation,\n" +
			"under the preset's name. The preset stays as it is. The template is checked like\n" +
			"any other document, so a preset holding something one person owns (a workspace\n" +
			"or a drive) is refused with the path. A preset's inline_policy replaced your\n" +
			"default policy; in a template it is laid onto it, so a key the preset left out\n" +
			"takes the default's value.\n\n" +
			"A preset limited to some user types is offered to those types only, and a\n" +
			"template is offered to everyone. Such a preset is refused unless you pass\n" +
			"--everyone, which says you know the limit is lost.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client()
			preset, err := c.GetPreset(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			doc, err := sdk.PresetDocument(preset, everyone)
			if err != nil {
				return err
			}
			t, err := c.SaveTemplate(cmd.Context(), uuid.Nil, sdk.TemplateSaveRequest{
				Scope: sdk.TemplateScopeOrg, Name: doc.Name, Description: doc.Description, Document: doc,
			})
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), t)
		},
	}
	cmd.Flags().BoolVar(&everyone, "everyone", false, "publish a preset that is limited to some user types to everyone")
	return cmd
}
