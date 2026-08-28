// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// waitReadyResult is `run wait-ready --json`'s output: the moment a sandbox is
// usable by something that wants to work INSIDE it (an editor or agent tool
// over the SSH gateway), which is later than RUNNING — the workspace clone
// lands after the sandbox is up.
type waitReadyResult struct {
	ID    uuid.UUID      `json:"id"`
	State types.RunState `json:"state"`
	// Workspace is the sandbox directory an external tool should open: Path is
	// the in-sandbox absolute path, VCS "git" when it is a git work tree.
	Workspace struct {
		VCS  string `json:"vcs"`
		Path string `json:"path,omitempty"`
	} `json:"workspace"`
}

// runWaitReadyCmd returns `wardyn run wait-ready <run-id>`. Distinct from
// `run --wait` (which waits for a TERMINAL state and is refused for an
// interactive run, since one never finishes on its own): this waits for the
// run to become USABLE — RUNNING, and its workspace inspectable through the
// same exec channel the console's files widget uses.
func runWaitReadyCmd(client clientFn) *cobra.Command {
	var timeout time.Duration
	var asJSON, expectGit bool
	cmd := &cobra.Command{
		Use:   "wait-ready <run-id>",
		Short: "Block until a run is RUNNING and its workspace is inspectable, then print where it is",
		Long: `Wait for a run to be usable from outside — RUNNING, with its workspace
directory readable inside the sandbox — and print that directory.

A run with a repo waits until the clone has landed (a git work tree exists);
pass --expect-git to require that for a workspace-sourced run too. A terminal
state (COMPLETED/FAILED/KILLED/STOPPED) before then exits non-zero at once;
--timeout exits 124. Pair with 'wardyn ssh <run-id> --json' to hand the
sandbox to an external tool over the SSH gateway.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			res, err := waitForRunReady(cmd.Context(), client(), id, timeout, expectGit)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run %s is %s; workspace %s (%s)\n", res.ID, res.State, res.Workspace.Path, res.Workspace.VCS)
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "give up after this long (exit 124)")
	cmd.Flags().BoolVar(&expectGit, "expect-git", false, "also require a git work tree in the workspace (automatic when the run names a repo)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit {id, state, workspace:{vcs, path}} as JSON (progress goes to stderr)")
	return cmd
}

// waitForRunReady polls until the run is RUNNING and GET /runs/{id}/files
// answers (409 = no sandbox yet, keep waiting). With wantGit it additionally
// waits for vcs:"git". Exit codes mirror waitForRun: FAILED→1, other terminal
// states→2, timeout→124.
func waitForRunReady(ctx context.Context, c *sdk.Client, runID uuid.UUID, timeout time.Duration, wantGit bool) (waitReadyResult, error) {
	fmt.Fprintf(os.Stderr, "waiting for run %s to be ready (timeout %s)\n", runID, timeout)
	deadline := time.Now().Add(timeout)
	var res waitReadyResult
	res.ID = runID
	consecutiveErrs := 0
	running := false
	for {
		if !running {
			run, err := c.GetRun(ctx, runID)
			switch {
			case err != nil:
				consecutiveErrs++
				if consecutiveErrs >= 5 {
					return res, fmt.Errorf("polling run %s failed %d times in a row: %w", runID, consecutiveErrs, err)
				}
			case run.State.IsTerminal():
				res.State = run.State
				if run.State == types.RunFailed {
					msg := fmt.Sprintf("run %s FAILED before it was ready", runID)
					if reason := runFailureReason(ctx, c, runID); reason != "" {
						msg += ": " + reason
					}
					return res, &exitError{code: 1, err: errors.New(msg)}
				}
				return res, &exitError{code: 2, err: fmt.Errorf("run %s terminated before it was ready: %s", runID, run.State)}
			case run.State == types.RunRunning:
				consecutiveErrs = 0
				running = true
				res.State = run.State
				if run.Repo != "" {
					wantGit = true
				}
			default:
				consecutiveErrs = 0
				res.State = run.State
			}
		}
		if running {
			files, err := c.RunFiles(ctx, runID)
			if err == nil {
				res.Workspace.VCS = files.VCS
				res.Workspace.Path = files.Path
				if files.VCS == "git" || (!wantGit && files.VCS != "unknown") {
					fmt.Fprintf(os.Stderr, "run %s ready: workspace %s (%s)\n", runID, files.Path, files.VCS)
					return res, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return res, &exitError{code: 124, err: fmt.Errorf("timed out after %s waiting for run %s to be ready (last state %s, workspace vcs %q)", timeout, runID, res.State, res.Workspace.VCS)}
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}
