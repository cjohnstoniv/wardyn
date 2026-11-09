// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// The read-only `wardyn run` subcommands that inspect runs: list, get, kill and
// grants. Split from commands.go by seam; the create command and the wait logic stay there.

func runListCmd(client clientFn) *cobra.Command {
	var listJSON bool
	var listLimit, listOffset int
	list := &cobra.Command{
		Use:   "list",
		Short: "List all runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, truncated, err := client().ListRunsPage(cmd.Context(), listPageOptsAt(listLimit, listOffset)...)
			if err != nil {
				return err
			}
			warnListTruncated(cmd, truncated, "run", len(runs), listOffset)
			if listJSON {
				return emitJSON(cmd.OutOrStdout(), runs)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tAGENT\tREPO\tCC\tSTATE\tCREATED_BY\tCREATED")
			for _, r := range runs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.ID, r.Agent, r.Repo, r.ConfinementClass, r.State,
					r.CreatedBy, r.CreatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "emit raw JSON")
	list.Flags().IntVar(&listLimit, "limit", 0, "max rows to return (0 = server default page)")
	list.Flags().IntVar(&listOffset, "offset", 0, "skip this many rows (page forward past a truncated list)")
	return list
}

func runGetCmd(client clientFn) *cobra.Command {
	var getJSON bool
	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show one run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			run, err := client().GetRun(cmd.Context(), id)
			if err != nil {
				return err
			}
			if getJSON {
				return emitJSON(cmd.OutOrStdout(), run)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tAGENT\tREPO\tCC\tSTATE\tIMAGE\tCREATED")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				run.ID, run.Agent, run.Repo, run.ConfinementClass, run.State,
				run.Image, run.CreatedAt.Format(time.RFC3339))
			if err := tw.Flush(); err != nil {
				return err
			}
			if q := run.Queue; q != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "\nwaiting: %d ahead in pool %s; queued %s, gives up %s (%s)\n",
					q.Position, q.PoolID, q.QueuedAt.Format(time.RFC3339), q.DeadlineAt.Format(time.RFC3339), q.Reason)
			}
			if line := serviceLine(run, time.Now()); line != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", line)
			}
			// A run the per-person Azure DevOps lane declined says why (run.ado_lane.skip).
			if detail := adoLaneDeclineDetail(cmd.Context(), client(), run.ID); detail != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "\nazure devops: %s\n", detail)
			}
			// Credentials the run's policy did not allow exactly were withheld (run.ceiling.reassert, run.revive).
			if line := withheldCredentialDetail(cmd.Context(), client(), run.ID); line != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", line)
			}
			// A FAILED run must not hide its reason on the default surface. Surface
			// the failure reason from audit inline; fall back to a pointer so the
			// user is never left with a bare "FAILED".
			if run.State == types.RunFailed {
				if reason := runFailureReason(cmd.Context(), client(), run.ID); reason != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "\nfailed: %s\n", reason)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "\nfailed — full detail: wardyn audit %s --json\n", run.ID)
				}
			}
			return nil
		},
	}
	get.Flags().BoolVar(&getJSON, "json", false, "emit raw JSON")
	return get
}

// serviceLine is a service run's health as one line, "service: healthy · 3 restarts · checked 40s ago", and ""
// for a run that is not a service. A service wardynd has not yet observed says so. A service that opted in to
// Wardyn's own check gets that reading on a second line (probeLine).
func serviceLine(run types.AgentRun, now time.Time) string {
	if run.Service == nil {
		return ""
	}
	st := run.ServiceStatus
	if st == nil {
		if run.Service.ControlPlaneProbe() {
			return "service: not checked yet\n" + probeLine(nil, now)
		}
		return "service: not checked yet"
	}
	health := string(st.Health)
	if st.Reason != "" {
		health += " (" + st.Reason + ")"
	}
	restarts := fmt.Sprintf("%d restarts", st.Restarts)
	if st.Restarts == 1 {
		restarts = "1 restart"
	}
	line := fmt.Sprintf("service: %s \u00b7 %s \u00b7 checked %s ago", health, restarts, agoText(now.Sub(st.ObservedAt)))
	if run.Service.ControlPlaneProbe() {
		line += "\n" + probeLine(st.Probe, now)
	}
	return line
}

// probeLine is Wardyn's own check of a service that opted in to it, as the line under serviceLine's:
// "wardyn's check: healthy (bad_status, HTTP 503) · 1 failed in a row · checked 40s ago".
func probeLine(p *types.RunServiceProbe, now time.Time) string {
	if p == nil {
		return "wardyn's check: not checked yet"
	}
	health := string(p.Health)
	switch {
	case p.Reason != "" && p.HTTPStatus != 0:
		health += fmt.Sprintf(" (%s, HTTP %d)", p.Reason, p.HTTPStatus)
	case p.Reason != "":
		health += " (" + p.Reason + ")"
	}
	if p.Failures > 0 {
		health += fmt.Sprintf(" \u00b7 %d failed in a row", p.Failures)
	}
	return fmt.Sprintf("wardyn's check: %s \u00b7 checked %s ago", health, agoText(now.Sub(p.ObservedAt)))
}

// agoText is an elapsed time as the terminal says it: seconds, minutes, hours or days, rounded down.
func agoText(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", max(int(d/time.Second), 0))
}

// adoLaneDeclineDetail is the sentence of the run's run.ado_lane.skip row, "" when
// it has none or the trail could not be read.
func adoLaneDeclineDetail(ctx context.Context, c *sdk.Client, runID uuid.UUID) string {
	events, _, err := c.AuditEventsPage(ctx, runID, sdk.AuditFilter{ActionPrefix: "run.ado_lane.skip"})
	if err != nil {
		return ""
	}
	for _, e := range events {
		var d struct {
			Detail string `json:"detail"`
		}
		if e.Action == "run.ado_lane.skip" && json.Unmarshal(e.Data, &d) == nil && d.Detail != "" {
			return d.Detail
		}
	}
	return ""
}

// withheldCredentialDetail is the line telling that credentials were withheld from the run because its
// policy does not allow their hosts exactly: the hosts named by the dropped_injection_hosts of its
// run.ceiling.reassert and run.revive rows, in the order first seen. "" when no row names a host or the
// trail could not be read. Host names only; no credential value or grant name is read.
func withheldCredentialDetail(ctx context.Context, c *sdk.Client, runID uuid.UUID) string {
	var hosts []string
	for _, action := range []string{"run.ceiling.reassert", "run.revive"} {
		events, _, err := c.AuditEventsPage(ctx, runID, sdk.AuditFilter{ActionPrefix: action})
		if err != nil {
			continue
		}
		for _, e := range events {
			var d struct {
				Hosts []string `json:"dropped_injection_hosts"`
			}
			if e.Action != action || json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			for _, h := range d.Hosts {
				if h != "" && !slices.Contains(hosts, h) {
					hosts = append(hosts, h)
				}
			}
		}
	}
	if len(hosts) == 0 {
		return ""
	}
	return "credentials withheld for " + strings.Join(hosts, ", ") +
		": the run's policy does not allow the host exactly; name it in allowed_domains to deliver it"
}

func runKillCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "kill <run-id>",
		Short: "Kill a run (tears down sandbox, revokes identity + credentials)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			if _, err := client().KillRun(cmd.Context(), id); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "kill requested for run %s\n", args[0])
			return nil
		},
	}
}

func runGrantsCmd(client clientFn) *cobra.Command {
	var grantsJSON bool
	grants := &cobra.Command{
		Use:   "grants <run-id>",
		Short: "List a run's credential-grant eligibility records (what it MAY request, not what was minted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			gs, err := client().ListGrants(cmd.Context(), id)
			if err != nil {
				return err
			}
			if grantsJSON {
				return emitJSON(cmd.OutOrStdout(), gs)
			}
			tw := newTab(cmd.OutOrStdout())
			// Full grant id, not short(): this is the row's own identity, not a
			// context column. APPROVAL is the load-bearing column — these are
			// ELIGIBILITY records, and a requires-approval grant mints nothing
			// until a human decides it.
			fmt.Fprintln(tw, "ID\tKIND\tAPPROVAL\tTTL\tSCOPE")
			for _, g := range gs {
				approval := "auto"
				if g.Spec.RequiresApproval {
					approval = "required"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%ds\t%s\n",
					g.ID, g.Spec.Kind, approval, g.Spec.TTLSeconds, orDash(string(g.Spec.Scope)))
			}
			return tw.Flush()
		},
	}
	grants.Flags().BoolVar(&grantsJSON, "json", false, "emit raw JSON")
	return grants
}
