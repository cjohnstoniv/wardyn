// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A sandbox that reports where it runs gets one run.sandbox.start row with the values that pass the name check;
// no row for a nil answer or when nothing passes, and a value that fails is never written as sent.
func TestAuditSandboxStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		where    *runner.SandboxWhere
		wantKeys []string // nil = no row
		notIn    string   // must not appear in the row's data
	}{
		{"full", &runner.SandboxWhere{Node: "node-1", RuntimeClass: "kata", RuntimeHandler: "kata-qemu"},
			[]string{"sandbox_ref", "node", "runtime_class", "runtime_handler"}, ""},
		{"nil", nil, nil, ""},
		{"node only", &runner.SandboxWhere{Node: "node-1"}, []string{"sandbox_ref", "node"}, ""},
		{"invalid node", &runner.SandboxWhere{Node: "Node 1; rm", RuntimeHandler: "runsc"}, []string{"sandbox_ref", "runtime_handler"}, "rm"},
		{"invalid only", &runner.SandboxWhere{Node: "UPPER"}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			run := types.AgentRun{ID: uuid.New()}
			h.srv.auditSandboxStart(context.Background(), run, runner.Sandbox{Ref: "wardyn-agent-x", Where: tc.where})
			var rows []types.AuditEvent
			for _, ev := range h.audit.snapshot() {
				if ev.Action == "run.sandbox.start" && ev.RunID != nil && *ev.RunID == run.ID {
					rows = append(rows, ev)
				}
			}
			if tc.wantKeys == nil {
				if len(rows) != 0 {
					t.Fatalf("rows = %+v, want none", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Outcome != "success" {
				t.Fatalf("rows = %+v, want one success row", rows)
			}
			var d map[string]any
			if err := json.Unmarshal(rows[0].Data, &d); err != nil {
				t.Fatal(err)
			}
			if len(d) != len(tc.wantKeys) {
				t.Errorf("data = %v, want keys %v", d, tc.wantKeys)
			}
			for _, k := range tc.wantKeys {
				if _, ok := d[k]; !ok {
					t.Errorf("data = %v, missing %q", d, k)
				}
			}
			if tc.notIn != "" && strings.Contains(string(rows[0].Data), tc.notIn) {
				t.Errorf("data %s holds %q", rows[0].Data, tc.notIn)
			}
		})
	}
}
