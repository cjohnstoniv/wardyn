// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for agent rules (migration 0400_agent_rules). Guarded by WARDYN_TEST_PG; each test works
// in its own throwaway database.
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func agentRulesFixture(harness, by string, denied ...string) types.AgentRules {
	return types.AgentRules{Harness: harness, UpdatedBy: by, Spec: types.PersonalWallsSpec{DeniedDomains: &denied},
		InnerWalls: &types.HarnessInnerWalls{SandboxMode: types.InnerSandboxReadOnly}}
}

// TestAgentRules_CreateUpdateDelete_PG: a create is version 1; an update at the stored version writes the
// next and replaces the spec and the inner walls; the list is sorted by harness; a delete answers the row it
// removed.
func TestAgentRules_CreateUpdateDelete_PG(t *testing.T) {
	st, _ := templatesPG(t)
	ctx := context.Background()
	cx, err := st.PutAgentRules(ctx, agentRulesFixture("codex-cli", "root", "a.example.com"), 0)
	if err != nil || cx.Version != 1 || cx.UpdatedAt.IsZero() || cx.InnerWalls == nil || cx.InnerWalls.SandboxMode != types.InnerSandboxReadOnly {
		t.Fatalf("create = %+v, %v", cx, err)
	}
	if _, err := st.PutAgentRules(ctx, agentRulesFixture("claude-code", "root", "b.example.com"), 0); err != nil {
		t.Fatal(err)
	}
	next := agentRulesFixture("codex-cli", "alice", "c.example.com")
	next.InnerWalls = nil
	upd, err := st.PutAgentRules(ctx, next, 1)
	if err != nil || upd.Version != 2 || upd.UpdatedBy != "alice" || upd.InnerWalls != nil ||
		upd.Spec.DeniedDomains == nil || (*upd.Spec.DeniedDomains)[0] != "c.example.com" {
		t.Fatalf("update = %+v, %v", upd, err)
	}
	all, err := st.ListAgentRules(ctx)
	if err != nil || len(all) != 2 || all[0].Harness != "claude-code" || all[1].Harness != "codex-cli" {
		t.Fatalf("list = %+v, %v", all, err)
	}
	got, err := store.GetAgentRulesQ(ctx, st.Pool, "codex-cli")
	if err != nil || got.Version != 2 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	gone, err := st.DeleteAgentRules(ctx, "codex-cli", 2)
	if err != nil || gone.Version != 2 || gone.Harness != "codex-cli" {
		t.Fatalf("delete = %+v, %v", gone, err)
	}
	if _, err := store.GetAgentRulesQ(ctx, st.Pool, "codex-cli"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: err = %v", err)
	}
	if _, err := st.DeleteAgentRules(ctx, "codex-cli", 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete of an absent harness: err = %v", err)
	}
}

// TestAgentRules_VersionMismatch_PG: a create over an existing harness, an update at a stale version and a
// delete at a stale version each answer ErrAgentRulesVersion and change nothing.
func TestAgentRules_VersionMismatch_PG(t *testing.T) {
	st, _ := templatesPG(t)
	ctx := context.Background()
	if _, err := st.PutAgentRules(ctx, agentRulesFixture("codex-cli", "root", "a.example.com"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutAgentRules(ctx, agentRulesFixture("codex-cli", "root", "b.example.com"), 0); !errors.Is(err, store.ErrAgentRulesVersion) {
		t.Fatalf("create over an existing harness: err = %v", err)
	}
	if _, err := st.PutAgentRules(ctx, agentRulesFixture("codex-cli", "root", "b.example.com"), 7); !errors.Is(err, store.ErrAgentRulesVersion) {
		t.Fatalf("update at a stale version: err = %v", err)
	}
	if _, err := st.PutAgentRules(ctx, agentRulesFixture("claude-code", "root", "b.example.com"), 1); !errors.Is(err, store.ErrAgentRulesVersion) {
		t.Fatalf("update of an absent harness: err = %v", err)
	}
	if _, err := st.DeleteAgentRules(ctx, "codex-cli", 7); !errors.Is(err, store.ErrAgentRulesVersion) {
		t.Fatalf("delete at a stale version: err = %v", err)
	}
	got, err := store.GetAgentRulesQ(ctx, st.Pool, "codex-cli")
	if err != nil || got.Version != 1 || (*got.Spec.DeniedDomains)[0] != "a.example.com" {
		t.Fatalf("the row changed: %+v, %v", got, err)
	}
}

// TestAgentRules_CheckRefusesNone_PG: an insert of harness "none" (Your own tools) or an empty harness
// fails the table's CHECK.
func TestAgentRules_CheckRefusesNone_PG(t *testing.T) {
	st, pool := templatesPG(t)
	ctx := context.Background()
	for _, h := range []string{"none", ""} {
		if _, err := pool.Exec(ctx, `INSERT INTO agent_rules (harness, spec, version, updated_by) VALUES ($1, '{}', 1, 'root')`, h); err == nil {
			t.Fatalf("an insert of harness %q was accepted", h)
		}
	}
	if all, err := st.ListAgentRules(ctx); err != nil || len(all) != 0 {
		t.Fatalf("list = %+v, %v", all, err)
	}
}
