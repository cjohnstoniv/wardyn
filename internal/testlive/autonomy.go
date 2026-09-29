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

// AutonomyL0RefusalOK reports a descriptive error unless body is the refusal
// autonomyLadder writes when a governance profile caps a member at AutonomyL0
// and the request is an unattended run.
//
// It is PURE — no HTTP, no client — so a live test and a hermetic fixture
// prove the SAME check: a 403 alone isn't proof of L0 enforcement, since a
// profile could refuse for an unrelated reason or resolve to a different rung
// and still return 403.
//
// It grades body text only, never the machine "reason" field, which the
// server always leaves empty on this refusal — checking APIError.Reason
// would fail red against a server enforcing L0 correctly.
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
// runner substrate this deployment uses.
//
// It does NOT also check for an absent run.exec row: an interactive run
// records run.interactive and returns before any Exec call exists, at EVERY
// autonomy level, so an absent row here proves nothing L0-specific. The real
// "no run.exec at L0" proof is AutonomyL0RefusalOK's refusal, which happens
// before any run exists at all.
//
// On an exec-less (krun) runner substrate, interactive dispatch never writes
// this row at all, so this check reports "never even attempted" rather than
// a real delivery failure there.
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
