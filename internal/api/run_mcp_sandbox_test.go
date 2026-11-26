// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// onlyAuditRow is the one row the harness recorded, with its data decoded.
func onlyAuditRow(t *testing.T, h *harness) (types.AuditEvent, map[string]any) {
	t.Helper()
	rows := h.audit.snapshot()
	if len(rows) != 1 {
		t.Fatalf("%d audit rows, want 1: %+v", len(rows), rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	return rows[0], data
}

// TestMCPSandboxAuditRows: each MCP server row names its action, run, actor, outcome and server, and its data
// is the ordinal, the key and the row's own facts, never a field the server's definition holds.
func TestMCPSandboxAuditRows(t *testing.T) {
	runID := uuid.New()
	row := store.RunMCPSandbox{RunID: runID, Ordinal: 2, ServerKey: "files", SandboxRef: "wardyn-mcp-1", Restarts: 3}
	for _, c := range []struct {
		action, actor, outcome string
		actorType              types.ActorType
		emit                   func(*Server)
		keys                   []string
	}{
		{"run.mcp_sandbox.create", "wardynd", "success", types.ActorSystem, func(s *Server) {
			s.auditMCPSandboxCreate(context.Background(), row, "success", "")
		}, []string{"mcp_sandbox", "ordinal", "sandbox_ref"}},
		{"run.mcp_sandbox.create", "wardynd", "failure", types.ActorSystem, func(s *Server) {
			r := row
			r.SandboxRef = ""
			s.auditMCPSandboxCreate(context.Background(), r, "failure", "readiness")
		}, []string{"cause", "mcp_sandbox", "ordinal"}},
		{"run.mcp_sandbox.restart", "alice@example.com", "success", types.ActorHuman, func(s *Server) {
			s.auditMCPSandboxRestart(context.Background(), types.ActorHuman, "alice@example.com", row, "success", "person")
		}, []string{"cause", "mcp_sandbox", "ordinal", "restarts"}},
		{"run.mcp_sandbox.end", "wardynd", "success", types.ActorSystem, func(s *Server) {
			s.auditMCPSandboxEnd(context.Background(), runID, types.ActorSystem, "wardynd", &row, "success", "run_end")
		}, []string{"cause", "mcp_sandbox", "ordinal"}},
		{"run.mcp_sandbox.end", "wardynd", "failure", types.ActorSystem, func(s *Server) {
			s.auditMCPSandboxEnd(context.Background(), runID, types.ActorSystem, "wardynd", nil, "failure", "rows_unread")
		}, []string{"cause"}},
	} {
		h := newHarness(t)
		c.emit(h.srv)
		ev, data := onlyAuditRow(t, h)
		if ev.Action != c.action || ev.RunID == nil || *ev.RunID != runID || ev.ActorType != c.actorType || ev.Actor != c.actor || ev.Outcome != c.outcome {
			t.Errorf("%s %s: row %+v", c.action, c.outcome, ev)
		}
		if got := slices.Sorted(maps.Keys(data)); !slices.Equal(got, c.keys) {
			t.Errorf("%s %s: data keys %v, want %v", c.action, c.outcome, got, c.keys)
		}
		if _, ok := data["mcp_sandbox"]; ok && (data["mcp_sandbox"] != "files" || data["ordinal"] != float64(2) || ev.Target != "files") {
			t.Errorf("%s %s: names %v / %v / %q, want files, 2", c.action, c.outcome, data["mcp_sandbox"], data["ordinal"], ev.Target)
		}
	}
}

// TestComponentAttachRecordsMCPPolicyMode: an MCP server that starts from a command records its sandbox's
// policy mode on run.component.attach, "same" when it names none; a remote server records none.
func TestComponentAttachRecordsMCPPolicyMode(t *testing.T) {
	comps := mcpPolicyComps(&types.ComponentMCPSandboxPolicy{Mode: types.ComponentMCPSandboxOwn, Spec: &types.RunPolicySpec{}})
	if got := comps.attached[0].auditEntry(0)["mcp_policy_mode"]; got != types.ComponentMCPSandboxOwn {
		t.Errorf("own server: mcp_policy_mode = %v", got)
	}
	comps = mcpPolicyComps(nil)
	if got := comps.attached[0].auditEntry(0)["mcp_policy_mode"]; got != types.ComponentMCPSandboxSame {
		t.Errorf("a server naming no policy: mcp_policy_mode = %v, want same", got)
	}
	if _, ok := comps.attached[1].auditEntry(1)["mcp_policy_mode"]; ok {
		t.Error("a remote server records mcp_policy_mode")
	}
}
