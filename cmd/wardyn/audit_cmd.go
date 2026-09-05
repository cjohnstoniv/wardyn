// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The `wardyn audit` command, split out of commands.go at the file-size gate's
// seam (scripts/check-file-size.sh): one command, its flags and its two
// server-shape rules — the paging/truncation contract W16-S1-2 added, and the
// run-scope guard F266 added — read as one thing here and were 100 lines of a
// 1000-line file there.
package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

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
			c := client()
			// GetRun FIRST, in BOTH modes — the guard the sibling logsCmd
			// carries and explains: /api/v1/audit is member-scoped by FILTERING
			// ROWS, so an unknown or unowned id answers 200 [] exactly like a
			// real run with no events (audit_run_scope_test.go).
			if _, err := c.GetRun(cmd.Context(), id); err != nil {
				return err
			}
			events, truncated, err := c.AuditEventsPage(cmd.Context(), id, filter, opts...)
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
