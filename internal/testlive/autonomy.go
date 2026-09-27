// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AutonomyL0RefusalOK reports a descriptive error unless body is the shape
// internal/api/runs_autonomy.go's autonomyLadder writes when a governance
// profile caps a member's posture at AutonomyL0 and the request is an
// unattended (non-interactive) run: "unattended runs are not allowed by your
// governance profile %q at this run's posture: it permits autonomy level %s
// (bound by %s), which requires a human at the pane. Launch with
// `--interactive`, or narrow the run's egress, secrets or confinement."
//
// It is PURE — no HTTP, no client — so #705's live test and a hermetic fixture
// both prove the SAME check: a 403 alone is not proof of L0 enforcement (a
// governance profile could refuse for an unrelated reason, or resolve to a
// DIFFERENT rung, and the test would still see "403" and call it proven).
//
// It grades body text only, never a machine "reason" field: refuse() calls
// writeError, which is writeErrorReason(w, status, "", msg) — the reason is
// always "" on this refusal (internal/api/http.go's errorBody.Reason is
// omitempty, and refuse() never sets one here). A check against
// APIError.Reason would fail red against a server enforcing L0 correctly.
func AutonomyL0RefusalOK(body string) error {
	if !strings.Contains(body, "unattended runs are not allowed") {
		return fmt.Errorf("refusal does not name the unattended-run gate (runs.interactive): %q", body)
	}
	if !strings.Contains(body, "autonomy level L0") {
		return fmt.Errorf("refusal does not resolve to autonomy level L0: %q", body)
	}
	return nil
}

// agentPolicyWriteData is the shape internal/api/runs_dispatch_agentpolicy.go's
// auditAgentPolicy marshals onto a run.agent_policy.write row's Data.
type agentPolicyWriteData struct {
	Level     string `json:"level"`
	Delivered bool   `json:"delivered"`
	Reason    string `json:"reason,omitempty"`
}

// AgentPolicyDeliveredOK reports a descriptive error unless events (an
// interactive run's own audit trail) carries a run.agent_policy.write row for
// wantLevel with delivered=true — proof the managed-settings file that closes
// claude-code's own bypass route actually reached the sandbox, on whichever
// runner substrate this deployment uses (internal/runner/k8s/driver.go
// advertises Capabilities.ManagedFiles unconditionally; the Docker driver
// does too).
//
// It does NOT also check for an absent run.exec row: on an interactive run,
// startAgentOrIdle records run.interactive and returns before any Exec call
// exists to write one, at EVERY autonomy level — an absent run.exec row here
// would be true of any interactive run and would prove nothing about L0
// specifically. The real "no run.exec at L0" proof is
// AutonomyL0RefusalOK's refusal, which happens before any run exists at all.
//
// On an exec-less (krun) runner substrate, run.agent_policy.write is written
// from onAgentStarted, which interactive dispatch never calls — an
// interactive run on that substrate never gets the row at all, and this
// check reports "never even attempted" rather than a real delivery failure.
func AgentPolicyDeliveredOK(events []types.AuditEvent, wantLevel string) error {
	var found *agentPolicyWriteData
	for _, e := range events {
		if e.Action != "run.agent_policy.write" {
			continue
		}
		var d agentPolicyWriteData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("run.agent_policy.write row %s: unreadable data: %w", e.ID, err)
		}
		found = &d
	}
	if found == nil {
		return errors.New("no run.agent_policy.write audit row: the managed-settings file was never even attempted " +
			"(an exec-less/krun runner substrate never writes this row for an interactive run — see doc comment)")
	}
	if found.Level != wantLevel {
		return fmt.Errorf("run.agent_policy.write level=%q, want %q", found.Level, wantLevel)
	}
	if !found.Delivered {
		return fmt.Errorf("run.agent_policy.write delivered=false (reason=%q)", found.Reason)
	}
	return nil
}
