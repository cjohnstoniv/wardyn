// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	// Aliased: every command constructor here takes a `client clientFn`
	// parameter that would otherwise shadow the package name.
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// clientFn lazily builds the SDK client after persistent flags are parsed.
type clientFn func() *sdk.Client

// parseID parses an id-like positional arg into a UUID, failing fast with a
// clear client-side message rather than posting a malformed path the server can
// only answer with an opaque 400/404. `what` names the noun (run/policy/approval).
func parseID(what, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid %s id %q: %w", what, s, err)
	}
	return id, nil
}

// setOptionalID parses an optional UUID-valued flag into a wire *uuid.UUID
// field. The server-side fields are pointers, so a malformed value could only
// ever come back as an opaque "invalid JSON body" 400 — parse client-side to
// fail fast with the flag's own name in the error. Empty (flag unset) is a
// no-op.
func setOptionalID(flag, s string, dst **uuid.UUID) error {
	if s == "" {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return fmt.Errorf("parse %s %q: %w", flag, s, err)
	}
	*dst = &id
	return nil
}

// normalizeConfinement maps a friendly tier alias (fence/wall/vault, case-
// insensitive — see types.ConfinementClassNames) onto its CC wire code, so
// CLI/CI callers can script the names the UI shows instead of memorizing CC
// codes. Anything else (including "", "CC1"...) passes through unchanged,
// leaving the server as the single validator of unknown values.
func normalizeConfinement(s string) string {
	for cc, name := range types.ConfinementClassNames {
		if strings.EqualFold(s, name) {
			return string(cc)
		}
	}
	return s
}

// runCmd is the single "run" noun: a bare invocation creates a run, and the
// subcommands inspect, stop and export runs. "runs" stays as an alias so
// `wardyn runs list` keeps working.
func runCmd(client clientFn) *cobra.Command {
	var repo, agent, task, policyID, confinement, policyFile, image, taskMode, workspaceID string
	var title, description string
	var devcontainerRepo, devcontainerRef string
	var interactive, wait, createJSON, dryRun bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:     "run",
		Aliases: []string{"runs"},
		Short:   "Create a governed agent run (subcommands list/get/grants/recording/kill inspect and stop runs)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --repo is optional: a run with no repo comes up in an ephemeral
			// scratch dir. --agent is required EXCEPT for --task-mode exec with an
			// --image (or a workspace), where there is no agent harness to name;
			// the server is the authority and returns a 400 naming what is missing.
			if wait && interactive {
				return fmt.Errorf("--wait and --interactive are mutually exclusive (an interactive run never finishes on its own)")
			}
			if dryRun && wait {
				return fmt.Errorf("--dry-run and --wait are mutually exclusive (a dry run launches nothing to wait for)")
			}
			body := sdk.CreateRunRequest{
				Agent: agent, Repo: repo, Task: task,
				Title: title, Description: description,
				// normalizeConfinement resolves a fence/wall/vault alias to its CC
				// code; anything else (including already-CC1/2/3 or "") is unchanged.
				ConfinementClass: normalizeConfinement(confinement), Interactive: interactive,
				Image: image, TaskMode: taskMode,
				DevcontainerRepo: devcontainerRepo, DevcontainerRef: devcontainerRef,
			}
			// --policy is a policy UUID; --workspace attaches an onboarded
			// workspace by id (see setOptionalID for the parse-here rationale).
			if err := setOptionalID("--policy", policyID, &body.PolicyID); err != nil {
				return err
			}
			if err := setOptionalID("--workspace", workspaceID, &body.WorkspaceID); err != nil {
				return err
			}
			// --policy-file supplies a JSON RunPolicySpec applied inline. It is
			// mutually exclusive with --policy; the server enforces that XOR — we
			// only surface a clear parse error client-side.
			if policyFile != "" {
				data, err := os.ReadFile(policyFile)
				if err != nil {
					return fmt.Errorf("read --policy-file: %w", err)
				}
				// Accept JSON or YAML: bridge to canonical JSON, then unmarshal the
				// same json-tagged spec the server validates.
				if data, err = policyToJSON(data); err != nil {
					return fmt.Errorf("parse --policy-file %s: %w", policyFile, err)
				}
				// Strict decode (DisallowUnknownFields, shared with `policy create`/
				// `policy render`): a misspelled spec field fails here, not as a
				// silently-dropped setting the server never sees.
				spec, err := decodeSpecStrict(data)
				if err != nil {
					return fmt.Errorf("parse --policy-file %s: %w", policyFile, err)
				}
				body.InlinePolicy = &spec
			}
			// --dry-run posts the SAME body to the preflight endpoint instead of
			// launching: it resolves the policy through the launch chokepoint (so
			// an XOR violation / unknown secret / non-onboarded workspace fails
			// here exactly as it would at create) and mints nothing.
			if dryRun {
				return printPreflight(cmd.Context(), client(), body, createJSON)
			}
			run, err := client().CreateRun(cmd.Context(), body)
			if err != nil {
				return err
			}
			if createJSON {
				// waitForRun prints and warnings go to stderr, so stdout stays
				// exactly one JSON object (the created run) for scripts to parse.
				if err := emitJSON(run.AgentRun); err != nil {
					return err
				}
			} else {
				fmt.Printf("created run %s (state %s, confinement %s)\n", run.ID, run.State, run.ConfinementClass)
				fmt.Printf("  spiffe id: %s\n", run.SPIFFEID)
				// The resolved image is the only signal that a --devcontainer-repo
				// build actually happened: with no image builder wired the server
				// silently falls back to the convention image (a 201 either way).
				if run.Image != "" {
					fmt.Printf("  image: %s\n", run.Image)
				}
				if interactive {
					fmt.Printf("  interactive: sandbox is idle; attach with `wardyn attach %s`\n", run.ID)
				}
			}
			// Advisory server warnings (workspace collision, dropped ssh grant):
			// the run is live either way, so silence here is a degraded run no
			// CI artifact records.
			for _, w := range run.Warnings {
				fmt.Fprintf(os.Stderr, "  warning: %s\n", w)
			}
			if wait {
				return waitForRun(cmd.Context(), client(), run.ID, timeout)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repository (org/name; optional — omit for an ephemeral scratch run)")
	cmd.Flags().StringVar(&agent, "agent", "", "agent name (e.g. claude-code)")
	cmd.Flags().StringVar(&task, "task", "", "human task description")
	cmd.Flags().StringVar(&title, "title", "", "short name for this run; runs sharing a title are grouped in the console (optional here, required in the console)")
	cmd.Flags().StringVar(&description, "description", "", "optional free-text note: why this run exists")
	cmd.Flags().StringVar(&policyID, "policy", "", "policy id (optional; uses the default policy if unset)")
	cmd.Flags().StringVar(&workspaceID, "workspace", "", "onboarded workspace id to launch against (optional; seeds its source, egress, image and bound model creds — composes with --policy/--policy-file)")
	cmd.Flags().StringVar(&policyFile, "policy-file", "", "path to a JSON or YAML RunPolicySpec applied inline (optional; mutually exclusive with --policy, enforced server-side)")
	cmd.Flags().StringVar(&confinement, "confinement", "", "confinement class (CC1|CC2|CC3, or fence|wall|vault; optional, inherits the policy minimum if unset)")
	cmd.Flags().BoolVar(&interactive, "interactive", false, "interactive run: come up idle for 'wardyn attach'; --task, if set, seeds the session's startup shell command at boot instead (empty --task stays idle, today's default); use a never-reap policy (auto_stop_after_sec < 0)")
	cmd.Flags().StringVar(&image, "image", "", "user-supplied base image (Bring Your Own Image; requires the server's image builder, mutually exclusive with devcontainer builds — enforced server-side; wraps the image only — nothing runs until inside the run's confinement tier, unlike --devcontainer-repo, which builds unconfined on the host)")
	cmd.Flags().StringVar(&devcontainerRepo, "devcontainer-repo", "", "git repo whose .devcontainer is built into the sandbox image (requires the server's image builder — WITHOUT it the run silently uses the convention image, so check the printed image; mutually exclusive with --image; builds/runs on the host, unconfined — trust the repo)")
	cmd.Flags().StringVar(&devcontainerRef, "devcontainer-ref", "", "git ref (branch/tag/sha) to build for --devcontainer-repo")
	cmd.Flags().StringVar(&taskMode, "task-mode", "", "how the sandbox executes --task: harness (default; runs the agent) or exec (runs the task as a plain shell command — no agent, and the operator's model access is not auto-injected; an explicit policy grant or a workspace's declared secret still applies)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve and check the run without launching it: prints the setup checklist and the confinement class that would be enforced")
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the run reaches a terminal state and exit with the run's outcome (COMPLETED=0, FAILED=agent exit code, KILLED/STOPPED=2, timeout=124)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "give up waiting after this long (with --wait; exit 124)")
	cmd.Flags().BoolVar(&createJSON, "json", false, "emit the created run (or the --dry-run checklist) as JSON (progress goes to stderr)")

	cmd.AddCommand(runListCmd(client), runGetCmd(client), runKillCmd(client),
		runGrantsCmd(client), runRecordingCmd(client), runWaitReadyCmd(client))
	return cmd
}

// runRecordingCmd downloads a run's terminal recording. It lives under the
// `run` noun, NOT under `wardyn record` — that noun is Recording MODE (learning
// a least-privilege policy from a run's activity), an unrelated concept the
// name would fuse with this one.
func runRecordingCmd(client clientFn) *cobra.Command {
	var outPath, session string
	rec := &cobra.Command{
		Use:   "recording <run-id>",
		Short: "Download a run's terminal recording as an asciicast (stdout unless -o)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			// W21-S1-6: defaults to the run's own (bare-id) cast when --session is
			// unset — an interactive run's OTHER recordings (one per attach
			// session, keyed "<run-id>~<session>") are otherwise unreachable from
			// the CLI/SDK even though the server has always served them.
			rc, err := client().GetRecording(cmd.Context(), id, session)
			if err != nil {
				return err
			}
			defer rc.Close()
			if outPath == "" {
				_, err = io.Copy(os.Stdout, rc)
				return err
			}
			f, err := os.Create(outPath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, rc); err != nil {
				f.Close()
				return err
			}
			// Close is checked: a swallowed flush error writes a truncated cast
			// that only fails much later, in a player.
			if err := f.Close(); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s\n", outPath)
			return nil
		},
	}
	rec.Flags().StringVarP(&outPath, "output", "o", "", "write the .cast here instead of stdout")
	rec.Flags().StringVar(&session, "session", "", "attach-session id, for an interactive run's OTHER recordings (default: the run's own recording)")
	return rec
}

// printPreflight renders the --dry-run checklist: one row per setup item plus
// the confinement class the run would actually enforce.
func printPreflight(ctx context.Context, c *sdk.Client, body sdk.CreateRunRequest, asJSON bool) error {
	pf, err := c.Preflight(ctx, body)
	if err != nil {
		return err
	}
	if asJSON {
		return emitJSON(pf)
	}
	fmt.Printf("dry run: not launched (enforced confinement %s)\n", pf.EnforcedConfinementClass)
	for _, warn := range pf.Warnings {
		fmt.Printf("warning: %s\n", warn)
	}
	tw := newTab()
	fmt.Fprintln(tw, "STATUS\tLABEL\tKIND\tREQUIRED_BY\tDETAIL")
	for _, it := range pf.SetupItems {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.Status, orDash(it.Label), it.Kind, orDash(it.RequiredBy), orDash(it.Detail))
	}
	return tw.Flush()
}

func runListCmd(client clientFn) *cobra.Command {
	var listJSON bool
	var listLimit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List all runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, err := client().ListRuns(cmd.Context(), listPageOpts(listLimit)...)
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
					r.ID, r.Agent, r.Repo, r.ConfinementClass, r.State,
					r.CreatedBy, r.CreatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "emit raw JSON")
	list.Flags().IntVar(&listLimit, "limit", 0, "max rows to return (0 = server default page)")
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
				return emitJSON(run)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tAGENT\tREPO\tCC\tSTATE\tIMAGE\tCREATED")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				run.ID, run.Agent, run.Repo, run.ConfinementClass, run.State,
				run.Image, run.CreatedAt.Format(time.RFC3339))
			if err := tw.Flush(); err != nil {
				return err
			}
			// A FAILED run must not hide its reason on the default surface. Surface
			// the failure reason from audit inline; fall back to a pointer so the
			// user is never left with a bare "FAILED".
			if run.State == types.RunFailed {
				if reason := runFailureReason(cmd.Context(), client(), run.ID); reason != "" {
					fmt.Printf("\nfailed: %s\n", reason)
				} else {
					fmt.Printf("\nfailed — full detail: wardyn audit %s --json\n", run.ID)
				}
			}
			return nil
		},
	}
	get.Flags().BoolVar(&getJSON, "json", false, "emit raw JSON")
	return get
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
			fmt.Printf("kill requested for run %s\n", args[0])
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
				return emitJSON(gs)
			}
			tw := newTab()
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

// waitPollInterval is how often --wait polls the run state (var for tests).
var waitPollInterval = 2 * time.Second

// waitForRun polls the run until it is terminal and maps the outcome to the
// CLI's exit code: COMPLETED→0, FAILED→the agent's real exit code from the
// run.complete audit event (fallback 1), KILLED/STOPPED/ARCHIVED→2, timeout→124.
func waitForRun(ctx context.Context, c *sdk.Client, runID uuid.UUID, timeout time.Duration) error {
	// Progress goes to stderr so `run --json` keeps stdout to a single object.
	fmt.Fprintf(os.Stderr, "waiting for run %s (timeout %s)\n", runID, timeout)
	deadline := time.Now().Add(timeout)
	consecutiveErrs := 0
	var lastState types.RunState
	for {
		run, err := c.GetRun(ctx, runID)
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
			if run.State.IsTerminal() {
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
					// Surface WHY, not just the exit code — a dispatch/image-pull
					// failure has no agent exit and would otherwise read as an
					// opaque "FAILED (agent exit code 1)".
					if reason := runFailureReason(ctx, c, runID); reason != "" {
						fmt.Fprintf(os.Stderr, "  reason: %s\n", reason)
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
func agentExitCode(ctx context.Context, c *sdk.Client, runID uuid.UUID) int {
	events, err := c.AuditEvents(ctx, runID)
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

// runFailureReason surfaces WHY a FAILED run failed. The dispatch path records
// failures as audit events with outcome "failure" and a human string in
// data.error / data.reason (internal/api/runs_dispatch.go) — a reason that is
// otherwise buried in `wardyn audit <id> --json` and invisible on run get/--wait.
// A governance product should not hide a FAILED run's own reason from its default
// surfaces (an unpullable image, an unknown --agent falling back to a remote ref,
// a proxy-resolve error all land here). Returns "" for a plain nonzero agent exit,
// where the exit code is the whole story. ponytail: first failure ≈ root cause;
// a later "failure" is usually a teardown cascade.
func runFailureReason(ctx context.Context, c *sdk.Client, runID uuid.UUID) string {
	events, err := c.AuditEvents(ctx, runID)
	if err != nil {
		return ""
	}
	for _, e := range events {
		if e.Outcome != "failure" || len(e.Data) == 0 {
			continue
		}
		var d map[string]any
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		for _, k := range []string{"error", "reason", "detail"} {
			if v, ok := d[k].(string); ok && v != "" {
				return fmt.Sprintf("%s: %s", e.Action, v)
			}
		}
	}
	return ""
}

// approvalScopePeek extracts the fields common to the several
// requested_scope JSON shapes (egressScope, apiKeyScope, gitPATScope,
// sshKeyScope all carry "host"; only the egress_domain shape carries "mode")
// without the CLI needing to know which kind it is decoding.
type approvalScopePeek struct {
	Host string `json:"host,omitempty"`
	Mode string `json:"mode,omitempty"`
}

// approvalHoldWindow mirrors internal/egress/proxy/approvals.go's
// defaultHoldTimeout: a wait_for_review egress approval blocks the sandbox
// for at most this long, so a still-PENDING row older than this has almost
// certainly already timed out on the proxy side even though nobody decided
// it yet.
const approvalHoldWindow = 30 * time.Second

// approvalHoldHint reports whether a is a live wait_for_review hold and, if
// so, how much of its window is left — the CLI decide loop otherwise has no
// way to tell a live 30s hold apart from an ordinary up-to-24h pendency
// (W19-S1-4 / W20-hold-fsm-7).
func approvalHoldHint(a types.ApprovalRequest) string {
	if a.State != types.ApprovalPending {
		return ""
	}
	var s approvalScopePeek
	if json.Unmarshal(a.RequestedScope, &s) != nil || s.Mode != "wait_for_review" {
		return ""
	}
	if left := approvalHoldWindow - time.Since(a.RequestedAt); left > 0 {
		return fmt.Sprintf("live hold, ~%ds left", int(left.Seconds()))
	}
	return "hold likely timed out"
}

// approvalHost extracts the "host" field from a's requested_scope, common to
// every scope kind except tool_call (which has none, and prints "").
func approvalHost(a types.ApprovalRequest) string {
	var s approvalScopePeek
	if json.Unmarshal(a.RequestedScope, &s) != nil {
		return ""
	}
	return s.Host
}

// approvalsCmd lists approval requests; approve/deny act on a single one.
func approvalsCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List approval requests (approve/deny decide a single one)",
	}
	var state string
	var runFilter string
	var asJSON bool
	var listLimit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List approval requests (optionally filtered by --state and/or --run)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runID, err := parseOptionalUUID(runFilter, "--run")
			if err != nil {
				return err
			}
			aps, err := client().ListApprovals(cmd.Context(), types.ApprovalState(state), runID, listPageOpts(listLimit)...)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(aps)
			}
			tw := newTab()
			// SCOPE and HOLD are appended last, not inserted, so a script scraping
			// the first N columns by position is unaffected. SCOPE prints "" for a
			// still-PENDING row or a credential/tool_call approval — neither has a
			// decision scope (DecisionScope's own doc: empty means nobody has
			// decided yet). HOLD prints "" for anything that isn't a live
			// wait_for_review egress hold — see approvalHoldHint.
			fmt.Fprintln(tw, "ID\tRUN\tKIND\tSTATE\tHOST\tREQUESTED\tSCOPE\tHOLD")
			for _, a := range aps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					a.ID, short(a.RunID.String()), a.Kind, a.State, approvalHost(a),
					a.RequestedAt.Format(time.RFC3339), a.DecisionScope, approvalHoldHint(a))
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&state, "state", "", "filter by state (e.g. PENDING)")
	list.Flags().StringVar(&runFilter, "run", "", "filter to approvals on one run (server-side)")
	list.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	list.Flags().IntVar(&listLimit, "limit", 0, "max rows to return (0 = server default page)")
	cmd.AddCommand(list)

	// approvalScanPage is the page size `approvals get` scans a run with. It
	// matches the server's own default page (defaultListLimit, well under
	// maxListLimit=1000), and asking for it EXPLICITLY is what makes the next
	// page reachable at all — offset only advances if the request carries one.
	const approvalScanPage = 200

	// get has no server-side counterpart (the human/admin API has no GET
	// /approvals/{id} — only the sandbox-internal lane does) so it scopes a
	// list-by-run call to one ID client-side. --run is required for exactly
	// that reason: without it there is no server filter to reuse and this
	// would have to fetch every approval in the deployment to find one.
	var getRun string
	var getJSON bool
	get := &cobra.Command{
		Use:   "get <approval-id> --run <run-id>",
		Short: "Show one approval request (looked up within --run's approvals)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apID, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("invalid approval id %q: %w", args[0], err)
			}
			if getRun == "" {
				return errors.New("--run is required: the API has no get-by-id lookup, so `approvals get` scans one run's approvals")
			}
			runID, err := uuid.Parse(getRun)
			if err != nil {
				return fmt.Errorf("invalid --run %q: %w", getRun, err)
			}
			// PAGE, don't peek: an unparameterised list is one server-default
			// page (200), so an approval past it read as "not found" — a
			// long-running run with a busy egress lane passes 200 easily.
			c := client()
			for offset := 0; ; offset += approvalScanPage {
				aps, err := c.ListApprovals(cmd.Context(), "", runID, sdk.ListOpts{Limit: approvalScanPage, Offset: offset})
				if err != nil {
					return err
				}
				for _, a := range aps {
					if a.ID != apID {
						continue
					}
					if getJSON {
						return emitJSON(a)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "ID:        %s\nRUN:       %s\nKIND:      %s\nSTATE:     %s\nHOST:      %s\nREQUESTED: %s\nSCOPE:     %s\nHOLD:      %s\n",
						a.ID, a.RunID, a.Kind, a.State, approvalHost(a),
						a.RequestedAt.Format(time.RFC3339), a.DecisionScope, approvalHoldHint(a))
					return nil
				}
				if len(aps) < approvalScanPage {
					break // a short page is the last page
				}
			}
			return fmt.Errorf("approval %s not found on run %s", apID, runID)
		},
	}
	get.Flags().StringVar(&getRun, "run", "", "run ID to search (required)")
	get.Flags().BoolVar(&getJSON, "json", false, "emit raw JSON")
	cmd.AddCommand(get)
	return cmd
}

// parseOptionalUUID parses raw as a uuid.UUID, returning uuid.Nil when raw is
// empty (an unset filter flag) instead of erroring.
func parseOptionalUUID(raw, flagName string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid %s %q: %w", flagName, raw, err)
	}
	return id, nil
}

// approvalDecisionCmd builds the approve/deny command. The two decisions are
// one operation with a different verb (the domain layer already models it that
// way: approval.Decide's ApprovalDecision.State is just APPROVED vs DENIED), so
// they share one body. decide is an unbound method expression — client() must
// resolve INSIDE RunE, after the persistent --url/--token flags are parsed.
// Its variadic sdk.DecisionOpts tail matches what (*sdk.Client).Approve and
// .Deny actually are as method expressions; RunE always passes exactly one.
func approvalDecisionCmd(client clientFn, verb, short string,
	decide func(*sdk.Client, context.Context, uuid.UUID, string, ...sdk.DecisionOpts) (types.ApprovalRequest, error),
) *cobra.Command {
	var reason, scope, until string
	cmd := &cobra.Command{
		Use:   verb + " <approval-id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("approval", args[0])
			if err != nil {
				return err
			}
			opts := sdk.DecisionOpts{Scope: types.ApprovalScope(scope)}
			if until != "" {
				t, err := parseDecisionUntil(until)
				if err != nil {
					return err
				}
				opts.Until = &t
			}
			ap, err := decide(client(), cmd.Context(), id, reason, opts)
			if err != nil {
				return err
			}
			fmt.Printf("approval %s -> %s\n", ap.ID, ap.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "reason recorded in the audit trail")
	cmd.Flags().StringVar(&scope, "scope", "",
		"decision scope: once|run|until|always on an egress_domain approval (default run — today's behavior), or run on a CREDENTIAL approval for a per-run git_pat lease (approve once, re-mintable for the run). Rejected otherwise")
	cmd.Flags().StringVar(&until, "until", "",
		"expiry for --scope=until: a duration (e.g. 2h) or an RFC3339 timestamp; requires --scope=until, rejected otherwise")
	return cmd
}

// parseDecisionUntil parses --until as either a duration relative to now
// (e.g. "2h") or an absolute RFC3339 timestamp, matching what
// decision_expires_at accepts on the wire either way.
func parseDecisionUntil(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--until %q is not a duration (e.g. 2h) or an RFC3339 timestamp", s)
	}
	return t, nil
}

// logTail dedupes a run's audit-event stream across repeated polls for
// logsCmd. The server's Since filter is RFC3339 (second resolution, see
// AuditFilter.Since), so re-polling with since=<last event's second> can
// legitimately re-return every event from that same second — logTail tracks
// which event IDs at the current boundary second were already emitted so
// those are skipped, while a genuinely new event at (or after) that second
// is not.
type logTail struct {
	since time.Time
	seen  map[uuid.UUID]bool
}

// filter returns the events in page (server-guaranteed time-ascending, i.e.
// oldest-first) that are new since the last call, plus the tail's updated
// state. A value receiver/return (not a pointer method) so a unit test can
// assert the before/after state directly with no I/O.
func (t logTail) filter(page []types.AuditEvent) ([]types.AuditEvent, logTail) {
	next := t
	if next.seen == nil {
		next.seen = map[uuid.UUID]bool{}
	}
	var newEvents []types.AuditEvent
	for _, e := range page {
		if e.Time.Before(next.since) {
			continue // stale event from a filter granularity mismatch; already emitted
		}
		if e.Time.Equal(next.since) {
			if next.seen[e.ID] {
				continue
			}
			next.seen[e.ID] = true
		} else {
			next.since = e.Time
			next.seen = map[uuid.UUID]bool{e.ID: true}
		}
		newEvents = append(newEvents, e)
	}
	return newEvents, next
}

// logLine renders one audit event as a single human-readable log line.
func logLine(e types.AuditEvent) string {
	line := fmt.Sprintf("%s %-24s %s", e.Time.Format(time.RFC3339), e.Action, e.Outcome)
	if len(e.Data) > 0 && string(e.Data) != "null" {
		line += " " + string(e.Data)
	}
	return line
}

// logsCmd tails a run's audit-event trail as a live, human-readable log
// stream — the CLI's answer to W22-S1-4: a batch/headless run's live
// progress was otherwise unreachable from the CLI (`attach` opens a
// SEPARATE interactive exec, not a tail of the agent — see Runner.Attach's
// doc; `run --wait` only polls terminal state, printing nothing in between).
//
// Honesty note: this reuses the existing audit-event pipeline rather than
// adding a new server-side stdout/stderr capture (no such capture exists for
// exec-mode runs anywhere in the current architecture — see
// internal/runner/runner.go's Exec/ExecStream docs). Every line is a real
// audited action, not raw process bytes; it is the closest live signal the
// CLI has today without new server plumbing.
func logsCmd(client clientFn) *cobra.Command {
	var follow bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Tail a run's audit-event trail (progress, not raw agent stdout — see --help)",
		Long: `Tail a run's audit-event trail: dispatch, egress decisions, credential mints,
and completion, printed as they happen.

This is NOT the agent's raw stdout/stderr — no such capture exists for a
headless/exec-mode run today. It is the live-progress signal the CLI has: a
batch run's own audit trail, which is exactly what --wait's terminal
"reason:" line already reads from, just streamed as it's written instead of
only at the end.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			c := client()
			// GetRun FIRST, in BOTH modes: an unknown/typo'd id 404s here and an
			// unauthorized caller 401/403s, while the audit endpoint happily
			// answers 200 [] for either — so --follow=false used to print
			// nothing and exit 0 for a run that does not exist.
			if _, err := c.GetRun(cmd.Context(), id); err != nil {
				return err
			}
			var tail logTail
			// Polls taken AFTER the run was first seen terminal. The completion
			// watcher flips the state BEFORE finalizeRunTail writes run.complete,
			// and the revoke/teardown audits land after that again — so stopping
			// on the first terminal read drops the completion line this command
			// promises. Bounded: two extra polls, and it stops as soon as one
			// comes back with nothing new.
			drains := 0
			for {
				var f sdk.AuditFilter
				if !tail.since.IsZero() {
					f.Since = tail.since.UTC().Format(time.RFC3339)
				}
				page, truncated, err := c.AuditEventsPage(cmd.Context(), id, f)
				if err != nil {
					return err
				}
				var newEvents []types.AuditEvent
				newEvents, tail = tail.filter(page)
				for _, e := range newEvents {
					fmt.Fprintln(cmd.OutOrStdout(), logLine(e))
				}
				if !follow {
					// A truncated page is the server's per-run cap (1000
					// events), not the end of the trail. --follow recovers from
					// it for free on its next poll, since `since` has advanced;
					// one-shot mode has to take that next page itself or it
					// silently prints a cut-off log.
					if truncated && len(newEvents) > 0 {
						continue
					}
					return nil
				}
				run, err := c.GetRun(cmd.Context(), id)
				if err != nil {
					return err
				}
				if run.State.IsTerminal() {
					if drains >= 2 || (drains > 0 && len(newEvents) == 0) {
						return nil
					}
					drains++
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", true, "keep polling until the run reaches a terminal state (like tail -f)")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "poll interval while following")
	return cmd
}

// auditCmd shows the audit trail for a run. The per-run trail caps at 1000
// events server-side (internal/api/audit.go's auditPerRunDefaultLimit),
// oldest-first — a run with more events than that silently dropped its
// newest ones, including run.complete, with no way to page further or even
// detect the drop (W16-S1-2). --limit/--offset close the paging gap; a
// truncated page (server sets X-Wardyn-Truncated, surfaced via
// sdk.AuditEventsPage) prints a warning naming the next --offset instead of
// looking identical to a complete trail. The filter flags mirror the
// server-side predicates docs/sdk.md documents on the raw HTTP API.
func auditCmd(client clientFn) *cobra.Command {
	var runID string
	var asJSON bool
	var limit, offset int
	var filter sdk.AuditFilter
	cmd := &cobra.Command{
		Use:   "audit <run-id>",
		Short: "Show the audit trail for a run",
		// A positional run id matches the sibling commands (run get, approve,
		// attach, record synthesize). The deprecated --run flag is still accepted
		// as an alias for backward compat, so at most one arg is allowed.
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Positional id wins; fall back to the deprecated --run flag.
			if len(args) == 1 {
				runID = args[0]
			}
			if runID == "" {
				return fmt.Errorf("run id is required (pass it positionally: wardyn audit <run-id>)")
			}
			id, err := parseID("run", runID)
			if err != nil {
				return err
			}
			var opts []sdk.ListOpts
			if limit > 0 || offset > 0 {
				opts = []sdk.ListOpts{{Limit: limit, Offset: offset}}
			}
			events, truncated, err := client().AuditEventsPage(cmd.Context(), id, filter, opts...)
			if err != nil {
				return err
			}
			if truncated {
				// Stderr, not stdout: --json output stays the plain array shape
				// existing callers (e.g. scripts/ci-run.sh) already parse.
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: audit trail truncated at %d event(s); page forward with --offset=%d (or a larger --limit) to reach the rest, including the newest events\n",
					len(events), offset+len(events))
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
	cmd.Flags().StringVar(&runID, "run", "", "run id (DEPRECATED: pass the run id positionally instead)")
	_ = cmd.Flags().MarkDeprecated("run", "pass the run id positionally: wardyn audit <run-id>")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	cmd.Flags().IntVar(&limit, "limit", 0, "max events to return (0 = server default page, currently up to 1000)")
	cmd.Flags().IntVar(&offset, "offset", 0, "page offset; page forward by offset += the previous page's event count")
	cmd.Flags().StringVar(&filter.Since, "since", "", "only events at/after this RFC3339 timestamp")
	cmd.Flags().StringVar(&filter.Until, "until", "", "only events before this RFC3339 timestamp")
	cmd.Flags().StringVar(&filter.ActionPrefix, "action-prefix", "", "only events whose action has this prefix (e.g. egress.)")
	cmd.Flags().StringVar(&filter.Actor, "actor", "", "only events by this principal (e.g. alice@corp.example)")
	cmd.Flags().StringVar(&filter.ActorType, "actor-type", "", "only events from this actor type (human|agent|system)")
	cmd.Flags().StringVar(&filter.Outcome, "outcome", "", "only events with this outcome (success|denied|failure)")
	return cmd
}

// listPageOpts turns a --limit flag into the SDK's variadic ListOpts: limit<=0
// sends nothing (the server applies its default page, preserving the prior
// unparameterised output), a positive limit is passed through.
func listPageOpts(limit int) []sdk.ListOpts {
	if limit <= 0 {
		return nil
	}
	return []sdk.ListOpts{{Limit: limit}}
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
//
// Use it ONLY for columns that are context, never for the id a subcommand will
// ask for back. Truncating an ACTIONABLE id breaks the obvious `list` → copy →
// `kill`/`approve` flow: the commands parse a full UUID and reject 8 chars with
// "invalid UUID length: 8". So the ID column of runs/approvals/policies prints in
// full, and short() is left for the likes of an approval's RUN column or an audit
// target, which nothing takes as an argument.
func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
