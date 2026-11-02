// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A macOS VM whose guest could not trust the run's proxy CA gets one denied run.dispatch row with the
// fixed codes and the run goes on; a trusted CA and an absent answer (not attempted) write none, and a
// code the host does not know is written as unavailable, never as the runner sent it.
func TestAuditKeychainTrust(t *testing.T) {
	for _, tc := range []struct {
		name       string
		k          *runner.KeychainTrust
		wantReason string // "" = no row
	}{
		{"ok", &runner.KeychainTrust{Status: runner.KeychainTrusted}, ""},
		{"failed", &runner.KeychainTrust{Status: runner.KeychainNotTrusted, Reason: runner.KeychainRefused}, runner.KeychainRefused},
		{"absent", nil, ""},
		{"unknown", &runner.KeychainTrust{Status: runner.KeychainNotTrusted, Reason: "guest text"}, runner.KeychainUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVMSignInFixture(t, "run_disk")
			mac := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineMacOSVM}
			f.srv.auditKeychainTrust(context.Background(), mac, runner.Sandbox{Ref: "vm-1", KeychainTrust: tc.k})
			var rows []types.AuditEvent
			for _, ev := range f.h.audit.snapshot() {
				if ev.Action == "run.dispatch" && ev.RunID != nil && *ev.RunID == mac.ID {
					rows = append(rows, ev)
				}
			}
			if tc.wantReason == "" {
				if len(rows) != 0 {
					t.Fatalf("rows = %+v, want none", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Outcome != "denied" {
				t.Fatalf("rows = %+v, want one denied row", rows)
			}
			var d map[string]any
			if err := json.Unmarshal(rows[0].Data, &d); err != nil {
				t.Fatal(err)
			}
			if d["keychain_trust"] != runner.KeychainNotTrusted || d["keychain_trust_reason"] != tc.wantReason || d["note"] != keychainTrustNote || len(d) != 3 {
				t.Errorf("data = %v", d)
			}
		})
	}
}
