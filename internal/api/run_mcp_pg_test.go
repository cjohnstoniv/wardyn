// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An MCP component's harnesses over a real Postgres: written with the run's
// component rows, kept by an erasure, and read back by a revive, which narrows
// the proxy's harness bindings to them. Guarded by WARDYN_TEST_PG; skipped
// cleanly when unset.

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestRunComponents_MCPHarnessesPersistAndRevive(t *testing.T) {
	const owner = "sub-pg-mcp-harnesses"
	h, _ := newRunOwnerPGHarness(t)
	ctx := t.Context()
	pg := h.srv.cfg.Store.(store.PG)
	run, rn := lostComponentRun(t, h, owner, "api.linear.test", `{"host":"api.linear.test","require_tls":true,"secret_name":"person-key"}`)

	// The stored config, as dispatch wrote it: both servers on for Claude Code,
	// linear on for Codex CLI too.
	cfgJSON, err := runner.BuildProxyConfig(run.ID, runner.ProxyConfig{RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081",
		MCPServers: []proxy.MCPServerConfig{{Key: "linear", URL: "https://mcp.linear.test/mcp", Ordinal: 0}, {Key: "notes", URL: "https://mcp.notes.test/mcp", Ordinal: 1}},
		MCPBindings: []proxy.MCPBinding{
			{Harness: "claude-code", SHA256: strings.Repeat("a", 64), Servers: []string{"linear", "notes"}},
			{Harness: "codex-cli", SHA256: strings.Repeat("b", 64), Servers: []string{"linear"}},
		},
	}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.storeRunProxyConfig(ctx, run.ID, cfgJSON); err != nil {
		t.Fatal(err)
	}
	mcp := func(host string) types.ComponentDefinition {
		return types.ComponentDefinition{Hosts: []string{host}, MCP: &types.ComponentMCP{URL: "https://" + host + "/mcp", Transport: types.ComponentMCPTransportHTTP}}
	}
	rows := []types.RunComponent{
		{SelfDefined: true, Owner: owner, Name: "Linear", Definition: mcp("mcp.linear.test"), MCPHarnesses: []string{"claude-code"}},
		{SelfDefined: true, Owner: owner, Name: "Notes", Definition: mcp("mcp.notes.test"), MCPHarnesses: []string{"claude-code", "codex-cli"}},
	}
	if err := pg.PutRunComponents(ctx, run.ID, rows); err != nil {
		t.Fatal(err)
	}
	got, err := pg.ListRunComponents(ctx, run.ID)
	if err != nil || len(got) != 2 || !slices.Equal(got[0].MCPHarnesses, rows[0].MCPHarnesses) || !slices.Equal(got[1].MCPHarnesses, rows[1].MCPHarnesses) {
		t.Fatalf("read back %+v, %v", got, err)
	}

	// The revive reads the rows: Codex CLI loses linear, which its row does not
	// turn on for it; nothing is added (notes stays off for Codex, though its
	// row allows it, because the stored binding never had it).
	if code, reason := reviveReason(t, h, run, true); code != http.StatusOK || rn.replaced != 1 {
		t.Fatalf("revive = %d %s (replaced %d)", code, reason, rn.replaced)
	}
	cfg, err := proxy.LoadConfigBytes(rn.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 2 || !slices.Equal(cfg.MCPBindings[0].Servers, []string{"linear", "notes"}) || len(cfg.MCPBindings[1].Servers) != 0 {
		t.Fatalf("revived MCP config: servers %+v bindings %+v", cfg.MCPServers, cfg.MCPBindings)
	}

	// An erasure clears the rows' content and keeps their harnesses; the next
	// revive then drops both servers.
	if _, err := pg.EraseRunComponentsOfRun(ctx, run.ID, owner); err != nil {
		t.Fatal(err)
	}
	got, err = pg.ListRunComponents(ctx, run.ID)
	if err != nil || !got[0].Erased || !slices.Equal(got[1].MCPHarnesses, rows[1].MCPHarnesses) {
		t.Fatalf("after erasure %+v, %v; want erased rows that keep their harnesses", got, err)
	}
	if ok, err := pg.MarkRunLost(ctx, run.ID, types.LostOutage, time.Now(), 0); err != nil || !ok {
		t.Fatalf("MarkRunLost: %v %v", ok, err)
	}
	code, reason := reviveReason(t, h, run, true)
	if code == http.StatusOK {
		if cfg, err = proxy.LoadConfigBytes(rn.cfg); err != nil || len(cfg.MCPServers) != 0 {
			t.Fatalf("revive after erasure kept MCP servers %+v (%v)", cfg.MCPServers, err)
		}
	} else if reason == "" {
		t.Fatalf("revive after erasure = %d with no reason", code)
	}
}
