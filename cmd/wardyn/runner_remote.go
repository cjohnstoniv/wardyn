// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_remote.go — remote-hosted runners: the Docker host or Kubernetes cluster this server is set up
// with. An admin or security admin lists them; an admin renames one; who may add one to their own user
// pools ("Available to people's pools") is set by an admin or security admin, and may be held for a
// second person's approval.
//
//	wardyn runner remote list [--mine] [--no-limits] [--set-up-here] [--ids]
//	wardyn runner remote show <id>
//	wardyn runner remote propose <id> [--free-disk GB]
//	wardyn runner remote set <id> --name "Build host"
//	wardyn runner remote people set <id> --everyone | --user <subject> --group <group> --role <user-type>
//	wardyn runner remote people clear <id>
//
// `wardyn runner set <runner> --available-to-people on|off` is the everyone-or-nobody form of the same
// policy; `people set` is the only way to list people, groups and roles.
func runnerRemoteCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "remote", Short: "List and manage remote-hosted runners, the Docker host or Kubernetes cluster this server is set up with"}
	var mine, asJSON, noLimits, setUpHere, idsOnly bool
	list := &cobra.Command{Use: "list", Short: "List the remote-hosted runners (admin), or with --mine those you may add to your own user pools",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			list := client().ListRemoteRunners
			if mine {
				list = client().ListMyRemoteRunners
			}
			out, err := list(cmd.Context())
			if err != nil {
				return err
			}
			if noLimits {
				out = slices.DeleteFunc(out, func(rr sdk.RemoteRunnerView) bool { return rr.Limits != nil })
			}
			if setUpHere {
				out = slices.DeleteFunc(out, func(rr sdk.RemoteRunnerView) bool { return !rr.Configured })
			}
			if idsOnly {
				for _, rr := range out {
					fmt.Fprintln(cmd.OutOrStdout(), rr.ID)
				}
				return nil
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), out)
			}
			return printRemoteRunners(cmd.OutOrStdout(), out)
		}}
	list.Flags().BoolVar(&mine, "mine", false, "only the ones you may add to your own user pools")
	list.Flags().BoolVar(&noLimits, "no-limits", false, "only the ones with no limits set, which take no run until an admin sets them")
	list.Flags().BoolVar(&setUpHere, "set-up-here", false, "only the ones set up on this server (SET UP HERE: yes)")
	list.Flags().BoolVar(&idsOnly, "ids", false, "print only the ids, one per line, for a script (a name can hold spaces; an id cannot)")
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	var showJSON bool
	show := &cobra.Command{Use: "show <id>", Short: "Show one remote-hosted runner (admin)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rr, err := client().GetRemoteRunner(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if showJSON {
				return emitJSON(cmd.OutOrStdout(), rr)
			}
			return printRemoteRunners(cmd.OutOrStdout(), []sdk.RemoteRunnerView{rr})
		}}
	show.Flags().BoolVar(&showJSON, "json", false, "emit raw JSON")
	var freeDisk int64
	propose := &cobra.Command{Use: "propose <id> [--free-disk GB]", Args: cobra.ExactArgs(1),
		Short: "Print the limits setup proposes for a remote-hosted runner, half of what it reports, as `wardyn runner set` flags; sets nothing (admin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			rr, err := client().GetRemoteRunner(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			flags, err := proposedLimitFlags(rr.Usage.Reported, rr.Usage.ReportedWhy, freeDisk)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), flags)
			return err
		}}
	propose.Flags().Int64Var(&freeDisk, "free-disk", 0, "GB of disk free for runs on the runner's host, in place of what the runner reports (no amount is ever guessed)")
	var name string
	set := &cobra.Command{Use: "set <id> --name <name>", Short: "Rename a remote-hosted runner (admin)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("name") {
				return errors.New("name the change: --name")
			}
			c := client()
			cur, err := c.GetRemoteRunner(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			req := sdk.UpdateRemoteRunnerRequest{Revision: cur.Revision, Name: &name}
			if err := req.Validate(); err != nil {
				return err
			}
			out, err := c.UpdateRemoteRunner(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			return printRemoteRunners(cmd.OutOrStdout(), []sdk.RemoteRunnerView{out})
		}}
	set.Flags().StringVar(&name, "name", "", "the runner's new name, 1 to 64 characters")
	cmd.AddCommand(list, show, propose, set, runnerRemotePeopleCmd(client))
	return subcommandGroup(cmd)
}

// runnerRemotePeopleCmd sets or clears a remote-hosted runner's "Available to people's pools".
func runnerRemotePeopleCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "people", Short: "Set who may add a remote-hosted runner to their own user pools (admin or security admin)"}
	var everyone bool
	var users, groups, roles []string
	set := &cobra.Command{Use: "set <id> (--everyone | --user/--group/--role ...)", Short: "Make a remote-hosted runner available to everyone's pools, or to the people, groups and roles listed",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			req := sdk.RemoteRunnerPeoplePolicyRequest{PeoplePools: sdk.PeoplePoolsEveryone}
			for _, l := range []struct {
				kind sdk.CapabilitySubjectType
				subs []string
			}{{sdk.CapabilitySubjectUser, users}, {sdk.CapabilitySubjectGroup, groups}, {sdk.CapabilitySubjectUserType, roles}} {
				for _, sub := range l.subs {
					req.Subjects = append(req.Subjects, sdk.RunnerPoolSubject{SubjectType: l.kind, Subject: sub})
				}
			}
			switch {
			case everyone == (len(req.Subjects) > 0):
				return errors.New("pass --everyone, or list people with --user, --group and --role")
			case !everyone:
				req.PeoplePools = sdk.PeoplePoolsListed
			}
			out, err := client().SetRemoteRunnerPeoplePolicy(cmd.Context(), args[0], req)
			if held, err := heldForApproval(cmd, err); held || err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), out)
		}}
	set.Flags().BoolVar(&everyone, "everyone", false, "anyone may add it to their own user pools")
	set.Flags().StringArrayVar(&users, "user", nil, "a person who may add it (repeatable)")
	set.Flags().StringArrayVar(&groups, "group", nil, "a group whose members may add it (repeatable)")
	set.Flags().StringArrayVar(&roles, "role", nil, "a user type whose people may add it (repeatable)")
	clear := &cobra.Command{Use: "clear <id>", Short: "Make a remote-hosted runner available to nobody's pools", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := heldForApproval(cmd, client().ClearRemoteRunnerPeoplePolicy(cmd.Context(), args[0]))
			return err
		}}
	cmd.AddCommand(set, clear)
	return subcommandGroup(cmd)
}

func printRemoteRunners(w io.Writer, rrs []sdk.RemoteRunnerView) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "ID\tNAME\tSET UP HERE\tONLINE\tPEOPLE'S POOLS\tPOOLS")
	for _, rr := range rrs {
		pools := make([]string, 0, len(rr.Pools))
		for _, p := range rr.Pools {
			pools = append(pools, p.Name)
		}
		people := string(rr.PeoplePools)
		if rr.PeoplePools == sdk.PeoplePoolsListed {
			people = fmt.Sprintf("listed (%d)", len(rr.PeopleSubjects))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", rr.ID, rr.Name, yesNo(rr.Configured), yesNo(rr.Online), people, strings.Join(pools, ", "))
	}
	return tw.Flush()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
