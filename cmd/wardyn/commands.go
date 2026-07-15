// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// clientFn lazily builds the API client after persistent flags are parsed.
type clientFn func() *apiClient

func runCmd(client clientFn) *cobra.Command {
	var repo, agent, task, policyID, confinement, policyFile string
	var interactive bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Create a new governed agent run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --repo is optional now: a run with no repo comes up in an ephemeral
			// scratch dir (matches the wizard's workspace-optional ephemeral runs).
			if agent == "" {
				return fmt.Errorf("--agent is required")
			}
			body := createRunBody{
				Agent: agent, Repo: repo, Task: task, PolicyID: policyID,
				ConfinementClass: confinement, Interactive: interactive,
			}
			// --policy-file supplies a JSON RunPolicySpec applied inline. It is
			// mutually exclusive with --policy; the server enforces that XOR — we
			// only surface a clear parse error client-side.
			if policyFile != "" {
				data, err := os.ReadFile(policyFile)
				if err != nil {
					return fmt.Errorf("read --policy-file: %w", err)
				}
				var spec types.RunPolicySpec
				if err := json.Unmarshal(data, &spec); err != nil {
					return fmt.Errorf("parse --policy-file %s: %w", policyFile, err)
				}
				body.InlinePolicy = &spec
			}
			run, err := client().createRun(cmd.Context(), body)
			if err != nil {
				return err
			}
			fmt.Printf("created run %s (state %s, confinement %s)\n", run.ID, run.State, run.ConfinementClass)
			fmt.Printf("  spiffe id: %s\n", run.SPIFFEID)
			if interactive {
				fmt.Printf("  interactive: sandbox is idle; attach with `wardyn attach %s`\n", run.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repository (org/name; optional — omit for an ephemeral scratch run)")
	cmd.Flags().StringVar(&agent, "agent", "", "agent name (e.g. claude-code)")
	cmd.Flags().StringVar(&task, "task", "", "human task description")
	cmd.Flags().StringVar(&policyID, "policy", "", "policy id (optional; uses the default policy if unset)")
	cmd.Flags().StringVar(&policyFile, "policy-file", "", "path to a JSON RunPolicySpec applied inline (optional; mutually exclusive with --policy, enforced server-side)")
	cmd.Flags().StringVar(&confinement, "confinement", "", "confinement class (CC1|CC2|CC3; optional, inherits the policy minimum if unset)")
	cmd.Flags().BoolVar(&interactive, "interactive", false, "interactive run: come up idle (no agent task) for `wardyn attach`; use a never-reap policy (auto_stop_after_sec < 0)")
	return cmd
}

func runsCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List and inspect runs",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List all runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, err := client().listRuns(cmd.Context())
			if err != nil {
				return err
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tAGENT\tREPO\tCC\tSTATE\tCREATED_BY\tCREATED")
			for _, r := range runs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					short(r.ID.String()), r.Agent, r.Repo, r.ConfinementClass, r.State,
					r.CreatedBy, r.CreatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	cmd.AddCommand(list)
	return cmd
}

func approveCmd(client clientFn) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "approve <approval-id>",
		Short: "Approve a pending approval request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ap, err := client().decideApproval(cmd.Context(), args[0], true, reason)
			if err != nil {
				return err
			}
			fmt.Printf("approval %s -> %s\n", ap.ID, ap.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "reason recorded in the audit trail")
	return cmd
}

func denyCmd(client clientFn) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "deny <approval-id>",
		Short: "Deny a pending approval request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ap, err := client().decideApproval(cmd.Context(), args[0], false, reason)
			if err != nil {
				return err
			}
			fmt.Printf("approval %s -> %s\n", ap.ID, ap.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "reason recorded in the audit trail")
	return cmd
}

func auditCmd(client clientFn) *cobra.Command {
	var runID string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show the audit trail for a run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runID == "" {
				return fmt.Errorf("--run is required")
			}
			events, err := client().audit(cmd.Context(), runID)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(events)
			}
			tw := newTab()
			fmt.Fprintln(tw, "TIME\tACTOR_TYPE\tACTOR\tACTION\tTARGET\tOUTCOME")
			for _, e := range events {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					e.Time.Format(time.RFC3339), e.ActorType, e.Actor, e.Action,
					short(e.Target), e.Outcome)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "run id")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

func killCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kill <run-id>",
		Short: "Kill a run (tears down sandbox, revokes identity + credentials)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client().killRun(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("kill requested for run %s\n", args[0])
			return nil
		},
	}
	return cmd
}

func newTab() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

// short truncates an id-like string to its first segment for table density.
func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
