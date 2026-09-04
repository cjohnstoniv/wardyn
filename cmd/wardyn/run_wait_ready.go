// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
// answers. The run state is re-read on EVERY tick, including after RUNNING: a
// run that dies while its clone is landing exits 1/2 at once, never 124 later.
// 409 from files (no sandbox yet) and transient 5xx keep waiting; any other
// 4xx and 501 are permanent and fail immediately. With wantGit it additionally
// waits for vcs:"git". Exit codes mirror waitForRun: FAILED→1, other terminal
// states→2, timeout→124.
func waitForRunReady(ctx context.Context, c *sdk.Client, runID uuid.UUID, timeout time.Duration, wantGit bool) (waitReadyResult, error) {
	fmt.Fprintf(os.Stderr, "waiting for run %s to be ready (timeout %s)\n", runID, timeout)
	deadline := time.Now().Add(timeout)
	var res waitReadyResult
	res.ID = runID
	consecutiveErrs := 0
	var lastFilesErr error
	for {
		run, err := c.GetRun(ctx, runID)
		if err != nil {
			consecutiveErrs++
			if consecutiveErrs >= 5 {
				return res, fmt.Errorf("polling run %s failed %d times in a row: %w", runID, consecutiveErrs, err)
			}
		} else {
			consecutiveErrs = 0
			res.State = run.State
			if run.State.IsTerminal() {
				if run.State == types.RunFailed {
					msg := fmt.Sprintf("run %s FAILED before it was ready", runID)
					if reason := runFailureReason(ctx, c, runID); reason != "" {
						msg += ": " + reason
					}
					return res, &exitError{code: 1, err: errors.New(msg)}
				}
				return res, &exitError{code: 2, err: fmt.Errorf("run %s terminated before it was ready: %s", runID, run.State)}
			}
			if run.State == types.RunRunning {
				if run.Repo != "" {
					wantGit = true
				}
				files, ferr := c.RunFiles(ctx, runID)
				switch {
				case ferr == nil:
					lastFilesErr = nil
					res.Workspace.VCS = files.VCS
					res.Workspace.Path = files.Path
					// vcs:"unknown" is not "not yet": the exec RAN (so the
					// sandbox is up) and Path names the directory it settled
					// on, which is the whole of what a caller who did not ask
					// for git needs. Waiting cannot improve it, so treating it
					// as un-ready spent the full --timeout and exited 124 on a
					// sandbox an editor could already open. With wantGit it
					// still waits: "unknown" is git confirming a work tree and
					// a later git command failing (runFilesScript exits 3 —
					// vcs:"none" — when git is missing outright), the shape a
					// clone still landing has.
					if files.VCS == "git" || !wantGit {
						fmt.Fprintf(os.Stderr, "run %s ready: workspace %s (%s)\n", runID, files.Path, files.VCS)
						return res, nil
					}
				case filesErrIsPermanent(ferr):
					return res, fmt.Errorf("run %s: cannot read its workspace: %w", runID, ferr)
				default:
					lastFilesErr = ferr
				}
			}
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("timed out after %s waiting for run %s to be ready (last state %s, workspace vcs %q)", timeout, runID, res.State, res.Workspace.VCS)
			if lastFilesErr != nil {
				msg += ": last files error: " + lastFilesErr.Error()
			}
			return res, &exitError{code: 124, err: errors.New(msg)}
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// filesErrIsPermanent reports whether a RunFiles error cannot be fixed by
// waiting: 501 (the runner cannot exec into a sandbox at all), a 3xx (an
// interposed proxy or a misrouted --url — never a sandbox that is still coming
// up), and any 4xx except the four that mean "ask again later". Everything
// else — network blips, 5xx — is treated as transient.
//
// Those four are 409 (no sandbox YET, handleRunFiles' empty-SandboxRef arm)
// plus 408, 425 and 429. 429 is one wardynd itself sends (internal/api/audit.go)
// and the one any ingress rate limiter in front of it sends; 408 and 425 are
// the other two an ingress emits for a request it never refused on its merits.
// Classed permanent, a single one of them aborted `run wait-ready` outright on
// a sandbox that was coming up fine.
func filesErrIsPermanent(err error) bool {
	var apiErr *sdk.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Status {
	case http.StatusConflict, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	case http.StatusNotImplemented:
		return true
	}
	return apiErr.Status >= 300 && apiErr.Status < 500
}
