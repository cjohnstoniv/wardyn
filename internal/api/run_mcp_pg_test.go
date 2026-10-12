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
	"testing"

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

	// [github built-in, Alpha, Beta], as the gate admitted them: the built-in
	// writes no row, so Alpha and Beta are rows 0 and 1. Dispatch turned Beta
	// on for both harnesses; its row then records Claude Code alone.
	mcp := func(host string) types.ComponentDefinition {
		return types.ComponentDefinition{Hosts: []string{host}, MCP: &types.ComponentMCP{URL: "https://" + host + "/mcp", Transport: types.ComponentMCPTransportHTTP}}
	}
	comps := runComponents{attached: []attachedComponent{
		{builtin: &builtinGit{}},
		{snapshot: types.RunComponent{SelfDefined: true, Owner: owner, Name: "Alpha", Definition: mcp("mcp.alpha.test"), MCPHarnesses: []string{"claude-code"}}},
		{snapshot: types.RunComponent{SelfDefined: true, Owner: "sub-pg-mcp-other", Name: "Beta", Definition: mcp("mcp.beta.test"),
			MCPHarnesses: []string{"claude-code", "codex-cli"}}},
	}}
	out := dispatchMCP(comps.dispatch().MCP, &[]runner.InjectionGrant{}, "http://wardyn-proxy:3128", false, map[string]string{})
	cfgJSON, err := runner.BuildProxyConfig(run.ID, runner.ProxyConfig{RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081",
		MCPServers: out.servers, MCPBindings: out.bindings}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.storeRunProxyConfig(ctx, run.ID, cfgJSON); err != nil {
		t.Fatal(err)
	}
	comps.attached[2].snapshot.MCPHarnesses = []string{"claude-code"}
	if err := h.srv.persistRunComponents(ctx, run.ID, comps); err != nil {
		t.Fatal(err)
	}
	got, err := pg.ListRunComponents(ctx, run.ID)
	if err != nil || len(got) != 2 || !slices.Equal(got[0].MCPHarnesses, []string{"claude-code"}) || !slices.Equal(got[1].MCPHarnesses, []string{"claude-code"}) {
		t.Fatalf("read back %+v, %v", got, err)
	}

	// Alpha's author is erased: the row's content goes, its harnesses stay.
	if _, err := pg.EraseRunComponentsOfRun(ctx, run.ID, owner); err != nil {
		t.Fatal(err)
	}
	got, err = pg.ListRunComponents(ctx, run.ID)
	if err != nil || !got[0].Erased || got[1].Erased || !slices.Equal(got[0].MCPHarnesses, []string{"claude-code"}) {
		t.Fatalf("after erasure %+v, %v; want Alpha erased, keeping its harnesses, and Beta intact", got, err)
	}

	// The revive reads the rows: erased Alpha is dropped, Beta is kept, and
	// Codex CLI loses Beta, which its row no longer turns on for it.
	if code, reason := reviveReason(t, h, run, true); code != http.StatusOK || rn.replaced != 1 {
		t.Fatalf("revive = %d %s (replaced %d)", code, reason, rn.replaced)
	}
	cfg, err := proxy.LoadConfigBytes(rn.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Key != "beta" {
		t.Fatalf("revived servers %+v: want Beta alone", cfg.MCPServers)
	}
	for _, b := range cfg.MCPBindings {
		want := map[string][]string{"claude-code": {"beta"}, "codex-cli": {}}[b.Harness]
		if !slices.Equal(b.Servers, want) {
			t.Fatalf("revived binding %s = %v, want %v", b.Harness, b.Servers, want)
		}
	}
}
