// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// governanceAdminsCmd grants and revokes unit admins: a person or an IdP group that governs a unit
// and every unit below it.
//
//	wardyn governance admins add Platform --group platform-leads
//	wardyn governance admins remove Platform --user alice@example.com
//
// A grant the server holds for a second approver (always, for a unit admin's) prints the pending
// change and exits 0, as `governance set` does.
func governanceAdminsCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admins",
		Short: "Make a person or an IdP group an admin of a unit and the units below it, or take it back",
	}
	cmd.AddCommand(governanceAdminCmd(client, true), governanceAdminCmd(client, false))
	return subcommandGroup(cmd)
}

func governanceAdminCmd(client clientFn, add bool) *cobra.Command {
	var user, group string
	use, short := "remove <unit>", "Revoke a unit admin (the unit by name or id)"
	if add {
		use, short = "add <unit>", "Grant a unit admin (the unit by name or id)"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			subjectType, subject := types.CapabilitySubjectUser, user
			if group != "" {
				subjectType, subject = types.CapabilitySubjectGroup, group
			}
			c := client()
			doc, err := c.GetGovernance(cmd.Context())
			if err != nil {
				return err
			}
			unit, err := governanceUnitByArg(doc, args[0])
			if err != nil {
				return err
			}
			var pending *sdk.GovernanceChange
			if add {
				var saved sdk.GovernanceUnitAdmin
				if saved, pending, err = c.GrantUnitAdmin(cmd.Context(), unit.ID, subjectType, subject); err == nil && pending == nil {
					return emitJSON(cmd.OutOrStdout(), saved)
				}
			} else {
				grant, ok := grantOf(doc, unit.ID, subjectType, subject)
				if !ok {
					return fmt.Errorf("%s %q is not an admin of unit %q", subjectType, subject, unit.Name)
				}
				if pending, err = c.RevokeUnitAdmin(cmd.Context(), unit.ID, grant.ID); err == nil && pending == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "revoked: %s %q is no longer an admin of %q\n", subjectType, subject, unit.Name)
					return nil
				}
			}
			if err != nil {
				return err
			}
			printGovernancePending(cmd.ErrOrStderr(), sdk.GovernanceApplyResult{Pending: []sdk.GovernanceChange{*pending}})
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "the person's sign-in subject or email")
	cmd.Flags().StringVar(&group, "group", "", "the IdP group")
	cmd.MarkFlagsMutuallyExclusive("user", "group")
	cmd.MarkFlagsOneRequired("user", "group")
	return cmd
}

// governanceUnitByArg is the unit arg names in doc: by exact name, else by id.
func governanceUnitByArg(doc sdk.GovernanceDocument, arg string) (sdk.GovernanceUnit, error) {
	for _, u := range doc.Units {
		if u.Name == arg {
			return u, nil
		}
	}
	if id, err := uuid.Parse(arg); err == nil {
		for _, u := range doc.Units {
			if u.ID == id {
				return u, nil
			}
		}
	}
	return sdk.GovernanceUnit{}, fmt.Errorf("no governance unit named %q", arg)
}

// grantOf is the grant naming subject on unit in doc, matched as the server stores the subject.
func grantOf(doc sdk.GovernanceDocument, unit uuid.UUID, subjectType types.CapabilitySubjectType, subject string) (sdk.GovernanceUnitAdmin, bool) {
	want := subject
	switch subjectType {
	case types.CapabilitySubjectUser:
		want = types.CanonicalUserSubject(subject)
	case types.CapabilitySubjectGroup:
		if canon, ok := types.CanonicalGroupSubject(subject); ok {
			want = canon
		}
	}
	for _, g := range doc.UnitAdminGrants() {
		if g.UnitID == unit && g.SubjectType == subjectType && g.Subject == want {
			return g, true
		}
	}
	return sdk.GovernanceUnitAdmin{}, false
}
