// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// supportDoors are the doors a grant may name, in the order the console lists them.
var supportDoors = []string{sdk.SupportDoorTerminal, sdk.SupportDoorCommands, sdk.SupportDoorFiles, sdk.SupportDoorForward, sdk.SupportDoorWebApps, sdk.SupportDoorDesktop}

// supportMinutes are the lifetimes an owner may pick when accepting.
var supportMinutes = []int{15, 30, 60}

// The consent words an owner reads before accepting, as the console shows them. {recording} is "recorded" or
// "not recorded".
const (
	supportConsentRecording = "Terminal: %s on this run. Commands, Files, Forward and Web apps are never recorded. " +
		"The audit trail keeps each command's text (a very long one is cut, with its length and checksum) and exit code. " +
		"For files it keeps direction and size, and in the Files panel a checksum, never a file's name or content; " +
		"a sync also keeps the folder it started in, and a download sent for review keeps the file's name on its approval request. " +
		"For a forward it keeps the port and a byte count. What the helper does inside the run appears in the audit trail as " +
		"this run's activity. No credential is paused: the helper can use every connection this run has, as you. " +
		"The helper cannot approve or deny any of this run's holds raised after their request, even once help ends."
	supportConsentReach = "Terminal and Commands each run anything the agent can, so either one reads every file and reaches the run's network. " +
		"Forward reaches every port the run listens on, including its web apps and desktop, unrecorded."
)

// runSupportCmd is `wardyn run support`: a super admin asks to help inside someone's running run, the run's
// owner accepts or declines with the doors and minutes they choose, and either of them (or a security admin)
// ends it. request, accept and decline need a CLI sign-in as a person; the owner's status and end also work
// with a personal token (support_grant.go: supportPerson, supportOwnerRead).
func runSupportCmd(client clientFn) *cobra.Command {
	cmd := subcommandGroup(&cobra.Command{
		Use:   "support",
		Short: "Ask to help inside a run, or answer or end a help request (support sessions)",
		Long: `Support sessions let a super admin help inside another person's running run.
The helper asks with 'request', naming the doors they want and why. The run's
owner reads that with 'status' and answers with 'accept' (choosing which doors
open and for 15, 30 or 60 minutes) or 'decline'. Either of them, or a security
admin, ends it with 'end'. Nothing opens until the owner accepts, and help cannot
be extended: more time needs a new request.

Doors: ` + strings.Join(supportDoors, ", ") + `.`,
	})
	cmd.AddCommand(runSupportRequestCmd(client), runSupportAcceptCmd(client), runSupportDeclineCmd(client),
		runSupportEndCmd(client), runSupportStatusCmd(client))
	return cmd
}

// parseSupportDoors reads a comma-separated door list: non-empty, each name one of the six, none twice.
func parseSupportDoors(list string) ([]string, error) {
	var doors []string
	for _, d := range strings.Split(list, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if !slices.Contains(supportDoors, d) {
			return nil, fmt.Errorf("--doors: %q is not a door; the doors are %s", d, strings.Join(supportDoors, ", "))
		}
		if slices.Contains(doors, d) {
			return nil, fmt.Errorf("--doors: %q is named twice", d)
		}
		doors = append(doors, d)
	}
	if len(doors) == 0 {
		return nil, fmt.Errorf("--doors is required: name at least one of %s", strings.Join(supportDoors, ", "))
	}
	return doors, nil
}

func runSupportRequestCmd(client clientFn) *cobra.Command {
	var doorList, reason string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "request <run-id> --doors terminal,commands --reason \"...\"",
		Short: "Ask the run's owner to let you help inside the run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			doors, err := parseSupportDoors(doorList)
			if err != nil {
				return err
			}
			if n := len([]rune(strings.TrimSpace(reason))); n < 1 || n > 500 {
				return errors.New("--reason must be 1 to 500 characters")
			}
			g, err := client().RequestSupport(cmd.Context(), run, sdk.SupportRequest{Doors: doors, Reason: reason})
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), g)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "help requested: grant %s\nasked for: %s\nnothing opens until the run's owner accepts; an unanswered request lapses after 15 minutes\n",
				g.Grant, strings.Join(g.DoorsAsked, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&doorList, "doors", "", "comma-separated doors to ask for: "+strings.Join(supportDoors, ", "))
	cmd.Flags().StringVar(&reason, "reason", "", "why you want to help; the owner reads this (1 to 500 characters)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the request as JSON")
	return cmd
}

func runSupportAcceptCmd(client clientFn) *cobra.Command {
	var doorList string
	var minutes int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "accept <run-id> <grant-id> --doors terminal --minutes 15|30|60",
		Short: "Let the helper in through the doors you name, for 15, 30 or 60 minutes",
		Long: `Accept a help request. Every door stays closed unless you name it in --doors, and
the doors must be among those the helper asked for. Read 'wardyn run support
status <run-id>' first: it shows who asked, their reason and what is recorded.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			grant, err := parseID("support grant", args[1])
			if err != nil {
				return err
			}
			doors, err := parseSupportDoors(doorList)
			if err != nil {
				return err
			}
			if !slices.Contains(supportMinutes, minutes) {
				return errors.New("--minutes must be 15, 30 or 60")
			}
			g, err := client().AcceptSupport(cmd.Context(), run, grant, sdk.SupportAccept{Doors: doors, Minutes: minutes})
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), g)
			}
			until := ""
			if g.ExpiresAt != nil {
				until = g.ExpiresAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "accepted: %s may use %s until %s\n", termLine(g.Helper), strings.Join(g.Doors, ", "), until)
			return nil
		},
	}
	cmd.Flags().StringVar(&doorList, "doors", "", "comma-separated doors to open (a subset of those asked)")
	cmd.Flags().IntVar(&minutes, "minutes", 0, "how long the help lasts: 15, 30 or 60")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the grant as JSON")
	return cmd
}

func runSupportDeclineCmd(client clientFn) *cobra.Command {
	return supportAnswerCmd(client, "decline <run-id> <grant-id>", "Decline a help request", "declined: nothing was opened",
		func(cmd *cobra.Command, c *sdk.Client, run, grant uuid.UUID) (sdk.SupportGrant, error) {
			return c.DeclineSupport(cmd.Context(), run, grant)
		})
}

func runSupportEndCmd(client clientFn) *cobra.Command {
	return supportAnswerCmd(client, "end <run-id> <grant-id>", "End a help request or session now",
		"ended: the helper can open nothing new; an open terminal, SSH or web session closes within about 35 seconds",
		func(cmd *cobra.Command, c *sdk.Client, run, grant uuid.UUID) (sdk.SupportGrant, error) {
			return c.EndSupport(cmd.Context(), run, grant)
		})
}

// supportAnswerCmd is decline and end: one run id, one grant id, one POST, one line.
func supportAnswerCmd(client clientFn, use, short, done string, call func(*cobra.Command, *sdk.Client, uuid.UUID, uuid.UUID) (sdk.SupportGrant, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			grant, err := parseID("support grant", args[1])
			if err != nil {
				return err
			}
			g, err := call(cmd, client(), run, grant)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), g)
			}
			fmt.Fprintln(cmd.OutOrStdout(), done)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the grant as JSON")
	return cmd
}

func runSupportStatusCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status <run-id>",
		Short: "Show a run's help request or session, and the last one that ended",
		Long: `Show the open help request or session on a run and the last one that ended. The
run's owner sees the helper's reason here, with what is recorded on the run, before
answering with accept or decline.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			st, err := client().SupportStatus(cmd.Context(), run)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), st)
			}
			return printSupportStatus(cmd.OutOrStdout(), st)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the status as JSON")
	return cmd
}

func printSupportStatus(w io.Writer, st sdk.SupportStatus) error {
	if st.Open == nil && st.Ended == nil {
		fmt.Fprintln(w, "no help request or session on this run")
	}
	if g := st.Open; g != nil {
		printSupportGrant(w, "open", *g)
		if g.State == "requested" {
			recorded := "not recorded"
			if slices.Contains(g.RecordedDoors, sdk.SupportDoorTerminal) {
				recorded = "recorded"
			}
			fmt.Fprintf(w, "\n%s\n\n%s\n", fmt.Sprintf(supportConsentRecording, recorded), supportConsentReach)
			if slices.Contains(g.DoorsAsked, sdk.SupportDoorDesktop) {
				desktop := "not recorded"
				if slices.Contains(g.RecordedDoors, sdk.SupportDoorDesktop) {
					desktop = "recorded"
				}
				fmt.Fprintf(w, "Desktop: %s on this run.\n", desktop)
			}
		}
	}
	if g := st.Ended; g != nil {
		if st.Open != nil {
			fmt.Fprintln(w)
		}
		printSupportGrant(w, "last ended", *g)
	}
	return nil
}

func printSupportGrant(w io.Writer, label string, g sdk.SupportGrant) {
	fmt.Fprintf(w, "%s: grant %s (%s)\n", label, g.Grant, g.State)
	fmt.Fprintf(w, "  helper:     %s\n", termLine(g.Helper))
	if g.Reason != "" {
		fmt.Fprintf(w, "  reason:     %s\n", termLine(g.Reason))
	}
	fmt.Fprintf(w, "  asked:      %s\n", strings.Join(g.DoorsAsked, ", "))
	fmt.Fprintf(w, "  requested:  %s\n", g.RequestedAt.UTC().Format(time.RFC3339))
	if len(g.Doors) > 0 {
		fmt.Fprintf(w, "  doors:      %s\n", strings.Join(g.Doors, ", "))
	}
	if g.ExpiresAt != nil {
		fmt.Fprintf(w, "  until:      %s\n", g.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if g.LastSeenAt != nil {
		fmt.Fprintf(w, "  last seen:  %s\n", g.LastSeenAt.UTC().Format(time.RFC3339))
	}
	if g.EndReason != "" {
		fmt.Fprintf(w, "  end reason: %s\n", g.EndReason)
	}
}
