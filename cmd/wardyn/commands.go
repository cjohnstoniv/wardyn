// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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

// runCmd is the single "run" noun: a bare invocation creates a run, and the
// list/get/kill subcommands inspect and stop runs. "runs" stays as an alias so
// `wardyn runs list` keeps working.
func runCmd(client clientFn) *cobra.Command {
	var repo, agent, task, policyID, confinement, policyFile, image, taskMode string
	var interactive, wait, createJSON bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:     "run",
		Aliases: []string{"runs"},
		Short:   "Create a governed agent run (subcommands list/get/kill inspect and stop runs)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --repo is optional: a run with no repo comes up in an ephemeral
			// scratch dir. --agent is required (enforced via MarkFlagRequired).
			if wait && interactive {
				return fmt.Errorf("--wait and --interactive are mutually exclusive (an interactive run never finishes on its own)")
			}
			body := createRunBody{
				Agent: agent, Repo: repo, Task: task, PolicyID: policyID,
				ConfinementClass: confinement, Interactive: interactive,
				Image: image, TaskMode: taskMode,
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
			if createJSON {
				// waitForRun prints go to stderr, so stdout stays exactly one
				// JSON object (the created run) for scripts to parse.
				if err := emitJSON(run); err != nil {
					return err
				}
			} else {
				fmt.Printf("created run %s (state %s, confinement %s)\n", run.ID, run.State, run.ConfinementClass)
				fmt.Printf("  spiffe id: %s\n", run.SPIFFEID)
				if interactive {
					fmt.Printf("  interactive: sandbox is idle; attach with `wardyn attach %s`\n", run.ID)
				}
			}
			if wait {
				return waitForRun(cmd.Context(), client(), run.ID.String(), timeout)
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
	cmd.Flags().StringVar(&image, "image", "", "user-supplied base image (Bring Your Own Image; requires the server's image builder, mutually exclusive with devcontainer builds — enforced server-side)")
	cmd.Flags().StringVar(&taskMode, "task-mode", "", "how the sandbox executes --task: harness (default; runs the agent) or exec (runs the task as a plain shell command — no agent, no LLM credentials)")
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the run reaches a terminal state and exit with the run's outcome (COMPLETED=0, FAILED=agent exit code, KILLED/STOPPED=2, timeout=124)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "give up waiting after this long (with --wait; exit 124)")
	cmd.Flags().BoolVar(&createJSON, "json", false, "emit the created run as JSON (progress goes to stderr)")
	_ = cmd.MarkFlagRequired("agent")

	var listJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List all runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, err := client().listRuns(cmd.Context())
			if err != nil {
				return err
			}
			if listJSON {
				return emitJSON(runs)
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
	list.Flags().BoolVar(&listJSON, "json", false, "emit raw JSON")

	var getJSON bool
	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show one run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := client().getRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if getJSON {
				return emitJSON(run)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tAGENT\tREPO\tCC\tSTATE\tIMAGE\tCREATED")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				run.ID, run.Agent, run.Repo, run.ConfinementClass, run.State,
				run.Image, run.CreatedAt.Format(time.RFC3339))
			return tw.Flush()
		},
	}
	get.Flags().BoolVar(&getJSON, "json", false, "emit raw JSON")

	kill := &cobra.Command{
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

	cmd.AddCommand(list, get, kill)
	return cmd
}

// waitPollInterval is how often --wait polls the run state (var for tests).
var waitPollInterval = 2 * time.Second

// terminalRunState mirrors internal/api.isTerminalRunState.
// ponytail: 5 constants, not worth exporting the server's helper for.
func terminalRunState(s types.RunState) bool {
	switch s {
	case types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived:
		return true
	}
	return false
}

// waitForRun polls the run until it is terminal and maps the outcome to the
// CLI's exit code: COMPLETED→0, FAILED→the agent's real exit code from the
// run.complete audit event (fallback 1), KILLED/STOPPED/ARCHIVED→2, timeout→124.
func waitForRun(ctx context.Context, c *apiClient, runID string, timeout time.Duration) error {
	// Progress goes to stderr so `run --json` keeps stdout to a single object.
	fmt.Fprintf(os.Stderr, "waiting for run %s (timeout %s)\n", runID, timeout)
	deadline := time.Now().Add(timeout)
	consecutiveErrs := 0
	var lastState types.RunState
	for {
		run, err := c.getRun(ctx, runID)
		if err != nil {
			// Tolerate transient poll blips (a CI stack mid-restart shouldn't
			// fail the pipeline); a persistent error still aborts fast.
			consecutiveErrs++
			if consecutiveErrs >= 5 {
				return fmt.Errorf("polling run %s failed %d times in a row: %w", runID, consecutiveErrs, err)
			}
		} else {
			consecutiveErrs = 0
			lastState = run.State
			if terminalRunState(run.State) {
				code := agentExitCode(ctx, c, runID)
				if run.State == types.RunFailed && code == 0 {
					// The terminal state commits just before the run.complete
					// audit write; one retry covers that tiny window.
					time.Sleep(waitPollInterval)
					code = agentExitCode(ctx, c, runID)
				}
				fmt.Fprintf(os.Stderr, "run %s finished: state %s, agent exit code %d\n", runID, run.State, code)
				switch run.State {
				case types.RunCompleted:
					return nil
				case types.RunFailed:
					if code == 0 {
						code = 1 // run.complete event missing/unparseable: still fail
					}
					return &exitError{code: code, err: fmt.Errorf("run %s FAILED (agent exit code %d)", runID, code)}
				default: // KILLED / STOPPED / ARCHIVED: lifecycle termination, not an agent result
					return &exitError{code: 2, err: fmt.Errorf("run %s terminated: %s", runID, run.State)}
				}
			}
		}
		if time.Now().After(deadline) {
			return &exitError{code: 124, err: fmt.Errorf("timed out after %s waiting for run %s (last state %s)", timeout, runID, lastState)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// agentExitCode reads the agent's real exit code from the last run.complete
// audit event. Best-effort: 0 when the event is missing or unparseable.
func agentExitCode(ctx context.Context, c *apiClient, runID string) int {
	events, err := c.audit(ctx, runID)
	if err != nil {
		return 0
	}
	code := 0
	for _, e := range events {
		if e.Action != "run.complete" || len(e.Data) == 0 {
			continue
		}
		var d struct {
			ExitCode *int `json:"exit_code"`
		}
		if json.Unmarshal(e.Data, &d) == nil && d.ExitCode != nil {
			code = *d.ExitCode
		}
	}
	return code
}

// approvalsCmd lists approval requests; approve/deny act on a single one.
func approvalsCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List approval requests (approve/deny decide a single one)",
	}
	var state string
	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List approval requests (optionally filtered by --state)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			aps, err := client().listApprovals(cmd.Context(), state)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(aps)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tRUN\tKIND\tSTATE\tREQUESTED")
			for _, a := range aps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					short(a.ID.String()), short(a.RunID.String()), a.Kind, a.State,
					a.RequestedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&state, "state", "", "filter by state (e.g. PENDING)")
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
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
				return emitJSON(events)
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

// emitJSON writes v to stdout as indented JSON (the CLI's --json output shape).
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
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
