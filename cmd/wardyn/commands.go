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

func runCmd(client clientFn) *cobra.Command {
	var repo, agent, task, policyID, confinement, policyFile, image, taskMode string
	var interactive, wait, asJSON bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Create a new governed agent run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --repo is optional now: a run with no repo comes up in an ephemeral
			// scratch dir (matches the wizard's workspace-optional ephemeral runs).
			if agent == "" {
				return fmt.Errorf("--agent is required")
			}
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
			// --json emits the raw run object for scripting (a pipeline can read
			// .ID/.State instead of scraping the human lines below). Printed before
			// the --wait blocking loop so the run identity is captured immediately.
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(run); err != nil {
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
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the created run as raw JSON (for scripting/CI instead of scraping human output)")
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
	fmt.Printf("waiting for run %s (timeout %s)\n", runID, timeout)
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
				code, found := agentExitCode(ctx, c, runID)
				if run.State == types.RunFailed && !found {
					// The terminal state commits just before the run.complete
					// audit write; one retry covers that tiny window.
					time.Sleep(waitPollInterval)
					code, found = agentExitCode(ctx, c, runID)
				}
				fmt.Printf("run %s finished: state %s, agent exit code %d\n", runID, run.State, code)
				switch run.State {
				case types.RunCompleted:
					return nil
				case types.RunFailed:
					if !found || code == 0 {
						code = 1 // completion event missing, or FAILED despite a 0 agent code: never exit 0 on FAILED
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

// agentExitCode reads the agent's real exit code from the last run.complete audit
// event. The bool reports whether such an event with an exit code was FOUND, so the
// caller can tell "agent genuinely exited 0" from "the completion event is missing/
// unreadable" — they otherwise both read as 0 (U088). Best-effort: (0,false) on any
// audit error.
func agentExitCode(ctx context.Context, c *apiClient, runID string) (int, bool) {
	events, err := c.audit(ctx, runID)
	if err != nil {
		return 0, false
	}
	code, found := 0, false
	for _, e := range events {
		if e.Action != "run.complete" || len(e.Data) == 0 {
			continue
		}
		var d struct {
			ExitCode *int `json:"exit_code"`
		}
		if json.Unmarshal(e.Data, &d) == nil && d.ExitCode != nil {
			code, found = *d.ExitCode, true
		}
	}
	return code, found
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
	var asJSON bool
	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show one run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := client().getRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(run)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tAGENT\tREPO\tCC\tSTATE\tIMAGE\tCREATED")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				run.ID, run.Agent, run.Repo, run.ConfinementClass, run.State,
				run.Image, run.CreatedAt.Format(time.RFC3339))
			return tw.Flush()
		},
	}
	get.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	cmd.AddCommand(list, get)
	return cmd
}

// approvalsCmd exposes the fully-implemented listApprovals client method as a
// command, so a CLI-only operator can discover a pending approval's id to
// approve/deny — previously the docs pointed only at "the Approvals UI" (U086).
func approvalsCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List pending and decided approval requests",
	}
	var state string
	list := &cobra.Command{
		Use:   "list",
		Short: "List approval requests (optionally filtered by --state)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			approvals, err := client().listApprovals(cmd.Context(), state)
			if err != nil {
				return err
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tRUN\tKIND\tSTATE\tREQUESTED\tDECIDED_BY\tREASON")
			for _, a := range approvals {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					short(a.ID.String()), short(a.RunID.String()), a.Kind, a.State,
					a.RequestedAt.Format(time.RFC3339), a.DecidedBy, a.Reason)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&state, "state", "", "filter by state (e.g. PENDING, APPROVED, DENIED)")
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
		Use:   "audit <run-id>",
		Short: "Show the audit trail for a run",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Accept the run id positionally like every sibling command (runs get,
			// approve, kill, …); keep --run as a still-supported alias. Previously
			// audit was the only target-resource command that ignored a positional
			// arg, silently discarding `wardyn audit <id>`.
			id := runID
			if id == "" && len(args) == 1 {
				id = args[0]
			}
			if id == "" {
				return fmt.Errorf("run id required: pass it positionally (wardyn audit <run-id>) or via --run")
			}
			events, err := client().audit(cmd.Context(), id)
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
	cmd.Flags().StringVar(&runID, "run", "", "run id (alias for the positional arg)")
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
