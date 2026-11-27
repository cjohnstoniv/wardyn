// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
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

// stdioAccessComps is one server that starts from a command, Files, in the given sandbox policy (nil: same), on
// for claude-code, as the access door's comps and facts.
func stdioAccessComps(policy *types.ComponentMCPSandboxPolicy) (runComponents, []componentFact) {
	def := *localInline("Files").Inline
	if policy != nil {
		def.MCP.Sandbox = &types.ComponentMCPSandbox{Policy: *policy}
	}
	comps := runComponents{attached: []attachedComponent{{source: componentSourceInline,
		snapshot: types.RunComponent{SelfDefined: true, Name: "Files", Definition: def, MCPHarnesses: []string{"claude-code"}}}}}
	return comps, []componentFact{{ID: "inline:0", Name: "Files", Status: componentReady, Hosts: def.Hosts}}
}

// TestAccessMCPHoldsRefused, on accessFor, the producer of both dry doors: one server that starts from a
// command per mode, with a hold host rule in the run's network settings (same, narrower) or in the ceiling
// (own), carries holds_refused: true and its policy_mode; with no hold rule anywhere, no holds_refused key.
func TestAccessMCPHoldsRefused(t *testing.T) {
	hold := []types.HostRuleSet{{Rules: []types.HostRule{{Host: "api.files.test", Paths: []string{"/admin"}, Effect: types.HostRuleHold}}}}
	for _, c := range []struct {
		mode   string
		policy *types.ComponentMCPSandboxPolicy
	}{
		{types.ComponentMCPSandboxSame, nil},
		{types.ComponentMCPSandboxNarrower, &types.ComponentMCPSandboxPolicy{Mode: types.ComponentMCPSandboxNarrower, Overlay: &types.CeilingOverlay{}}},
		{types.ComponentMCPSandboxOwn, &types.ComponentMCPSandboxPolicy{Mode: types.ComponentMCPSandboxOwn, Spec: &types.RunPolicySpec{}}},
	} {
		for _, withHold := range []bool{true, false} {
			h := newHarness(t)
			cfg := baseTestConfig(h, nil)
			req := createRunRequest{}
			req.Tools = harnessTools("claude-code")
			in := accessInputs{req: &req, spec: types.RunPolicySpec{AllowedDomains: []string{"api.files.test"}}}
			if withHold && c.mode == types.ComponentMCPSandboxOwn {
				cfg.DefaultPolicy = types.RunPolicySpec{HostRules: hold} // the ceiling a member's own policy is clamped to
			} else if withHold {
				in.spec.HostRules = hold
			}
			in.comps, in.facts = stdioAccessComps(c.policy)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/policy-preview", nil).WithContext(memberCtx(nil))
			block, err := New(cfg).accessFor(r, in)
			if err != nil {
				t.Fatalf("%s: %v", c.mode, err)
			}
			i := slices.IndexFunc(block.Rows, func(row client.AccessRow) bool { return row.MCP != nil })
			if i < 0 {
				t.Fatalf("%s: no MCP row in %+v", c.mode, block.Rows)
			}
			raw, _ := json.Marshal(block.Rows[i].MCP)
			wantMode := `"policy_mode":"` + c.mode + `"`
			if withHold != strings.Contains(string(raw), `"holds_refused":true`) || strings.Contains(string(raw), `"holds_refused":false`) || !strings.Contains(string(raw), wantMode) {
				t.Errorf("%s, hold %v: %s", c.mode, withHold, raw)
			}
		}
	}
}

// TestMCPBrokeredServerFor: the broker sits in front of a remote server and of one that starts from a command,
// each only for a harness it is on for; a component with no server is not brokered. Codex CLI's MCP rules are
// then the broker's to enforce.
func TestMCPBrokeredServerFor(t *testing.T) {
	on := func(def types.ComponentDefinition) runComponents {
		return runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{Definition: def, MCPHarnesses: []string{codexHarness}}}}}
	}
	remote := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.remote.test/mcp"}}
	for _, c := range []struct {
		name    string
		comps   runComponents
		harness string
		want    bool
	}{
		{"no server", on(types.ComponentDefinition{Hosts: []string{"a.test"}}), codexHarness, false},
		{"remote", on(remote), codexHarness, true},
		{"stdio", on(*localInline("Files").Inline), codexHarness, true},
		{"stdio, another harness", on(*localInline("Files").Inline), "claude-code", false},
		{"no components", runComponents{}, codexHarness, false},
	} {
		if got := mcpBrokeredServerFor(c.comps, c.harness); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	req := createRunRequest{}
	req.Tools = harnessTools(codexHarness)
	b := &accessBuilder{in: accessInputs{req: &req, comps: on(*localInline("Files").Inline)}, hosts: map[string]*client.AccessHost{}}
	if got := b.toolPolicyFact(codexHarness).Enforcer; got != toolEnforcerMCPBroker {
		t.Errorf("Codex CLI with a server that starts from a command: enforcer %q, want %q", got, toolEnforcerMCPBroker)
	}
}
