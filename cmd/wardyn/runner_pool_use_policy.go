// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool_use_policy.go — who may use an organization pool, which a security admin or admin sets.
//
//	wardyn runner pool use-policy get <pool>
//	wardyn runner pool use-policy set <pool> [--group G]... [--role R]... [--user U]...
//	wardyn runner pool use-policy clear <pool>
//
// With no policy, everyone who may launch runs on organization pools may use the pool; a policy narrows
// that to the groups, roles and people it lists. A set replaces the whole list. Where a second person
// has to approve, a set or a clear is held: it is said so, nothing has changed, and the command still
// exits 0. A user pool has no use policy (only its owner uses it), which is said before anything is sent.
func runnerPoolUsePolicyCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "use-policy", Short: "Get, set or clear who may use an organization pool (security admin or admin)"}
	get := &cobra.Command{Use: "get <pool>", Short: "Print who may use an organization pool", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := organizationPoolArg(cmd, client, args[0])
		if err != nil {
			return err
		}
		pol, err := client().GetRunnerPoolUsePolicy(cmd.Context(), p.ID)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if pol.UpdatedAt.IsZero() {
			_, err = fmt.Fprintf(out, "no use policy: everyone who may launch runs on organization pools may use %s\n", p.Name)
			return err
		}
		if _, err := fmt.Fprintf(out, "revision %d, updated %s\n", pol.Revision, pol.UpdatedAt.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		tw := newTab(out)
		fmt.Fprintln(tw, "SUBJECT TYPE\tSUBJECT")
		for _, s := range pol.Subjects {
			fmt.Fprintf(tw, "%s\t%s\n", s.SubjectType, s.Subject)
		}
		return tw.Flush()
	}}
	var groups, roles, users []string
	set := &cobra.Command{Use: "set <pool> [--group G]... [--role R]... [--user U]...", Short: "Replace who may use an organization pool", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := organizationPoolArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			// An empty list is the server's to refuse, in its own words.
			subjects := []sdk.RunnerPoolSubject{}
			for _, by := range []struct {
				kind  types.CapabilitySubjectType
				names []string
			}{{types.CapabilitySubjectGroup, groups}, {types.CapabilitySubjectUserType, roles}, {types.CapabilitySubjectUser, users}} {
				for _, n := range by.names {
					subjects = append(subjects, sdk.RunnerPoolSubject{SubjectType: by.kind, Subject: n})
				}
			}
			pol, err := client().SetRunnerPoolUsePolicy(cmd.Context(), p.ID, sdk.RunnerPoolUsePolicyRequest{Revision: &p.Revision, Subjects: subjects})
			if held, err := heldForApproval(cmd, err); held || err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "use policy set on %s, revision %d\n", p.Name, pol.Revision)
			return err
		}}
	set.Flags().StringArrayVar(&groups, "group", nil, "a group whose members may use the pool (repeatable)")
	set.Flags().StringArrayVar(&roles, "role", nil, "a role (user type id) that may use the pool (repeatable)")
	set.Flags().StringArrayVar(&users, "user", nil, "a person who may use the pool (repeatable)")
	clear := &cobra.Command{Use: "clear <pool>", Short: "Let everyone who may launch on organization pools use the pool again", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := organizationPoolArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			if held, err := heldForApproval(cmd, client().ClearRunnerPoolUsePolicy(cmd.Context(), p.ID)); held || err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "use policy cleared on %s: everyone who may launch runs on organization pools may use it\n", p.Name)
			return err
		}}
	cmd.AddCommand(get, set, clear)
	return subcommandGroup(cmd)
}

// organizationPoolArg reads <pool> for a use policy, which only an organization pool has.
func organizationPoolArg(cmd *cobra.Command, client clientFn, arg string) (sdk.RunnerPool, error) {
	p, err := readPoolArg(cmd, client, arg)
	if err != nil {
		return p, err
	}
	if p.Scope != sdk.RunnerPoolOrganization {
		return p, fmt.Errorf("%s is a user pool. Only its owner can use it; a use policy narrows organization pools only", p.Name)
	}
	return p, nil
}

// subjectWord is how `show` names a use-policy subject's kind.
var subjectWord = map[types.CapabilitySubjectType]string{
	types.CapabilitySubjectGroup: "group", types.CapabilitySubjectUserType: "role", types.CapabilitySubjectUser: "person",
}

// whoMayUse is an organization pool's WHO MAY USE row, and whether there is one: a caller who may use
// the pool but may not read its policy (403) gets no row rather than a guess.
func whoMayUse(cmd *cobra.Command, c *sdk.Client, p sdk.RunnerPool) (string, bool, error) {
	pol, err := c.GetRunnerPoolUsePolicy(cmd.Context(), p.ID)
	var api *sdk.APIError
	if errors.As(err, &api) && api.Status == http.StatusForbidden {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if pol.UpdatedAt.IsZero() {
		return "everyone who may launch runs on organization pools", true, nil
	}
	note := fmt.Sprintf("   (use policy, revision %d)", pol.Revision)
	if len(pol.Subjects) == 0 {
		return "Nobody can use this pool. Everyone it listed has left." + note, true, nil
	}
	parts := make([]string, 0, len(pol.Subjects))
	for _, s := range pol.Subjects {
		parts = append(parts, subjectWord[s.SubjectType]+" "+s.Subject)
	}
	return "only: " + strings.Join(parts, ", ") + note, true, nil
}
