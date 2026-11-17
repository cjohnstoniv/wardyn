// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runFailureReason surfaces WHY a FAILED run failed. The cause is the run's own
// failure_hint, a sentence the platform wrote for the operator ("the sandbox could
// not be created: pull ...: not found"). The audit row only names the step that
// failed ("Create sandbox"), so the hint is read first and printed without an
// action prefix: it is already a whole sentence. A FAILED state commits just
// before the hint is stamped, so a run polled with no hint is re-read once.
// With still no hint (an older server, or a failure the platform wrote no
// sentence for) the audit rows are scanned as before: the dispatch path records
// failures as outcome "failure" with a string in
// data.error / data.reason (internal/api/runs_dispatch.go) — a reason that is
// otherwise buried in `wardyn audit <id> --json` and invisible on run get/--wait.
// A governance product should not hide a FAILED run's own reason from its default
// surfaces (an unpullable image, an unknown --agent falling back to a remote ref,
// a proxy-resolve error all land here). Returns "" for a plain nonzero agent exit,
// where the exit code is the whole story. ponytail: first failure ≈ root cause;
// a later "failure" is usually a teardown cascade.
//
// ?outcome=failure for agentExitCode's reason: an unfiltered per-run read is
// capped at 1000 events, so on a chatty run the dispatch error that explains an
// otherwise opaque "FAILED" was simply not on the page.
func runFailureReason(ctx context.Context, c *sdk.Client, run types.AgentRun) string {
	if run.FailureHint == "" {
		if again, err := c.GetRun(ctx, run.ID); err == nil {
			run = again
		}
	}
	if run.FailureHint != "" {
		return run.FailureHint
	}
	runID := run.ID
	events, _, err := c.AuditEventsPage(ctx, runID, sdk.AuditFilter{Outcome: "failure"})
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
