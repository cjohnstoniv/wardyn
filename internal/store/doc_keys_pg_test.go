// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Version skew on the two whole-document writes: an older wardynd saving a
// document a newer one wrote must not drop the keys it does not know. Guarded
// by WARDYN_TEST_PG (see store_pg_test.go).
package store_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSiteConfigRoundTripPreservesUnknownKeys: the stored document carries a
// key this binary's types.SiteConfig does not declare (as 0.8's model_providers
// is to 0.7, written by a newer wardynd). A save through this binary keeps it, and still clears a
// declared key the save leaves out.
func TestSiteConfigRoundTripPreservesUnknownKeys(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	clear := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM site_config`); err != nil {
			t.Fatalf("clear site_config: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	const newer = `{"scm_hosts":["github.com"],"internal_hosts":[{"host_suffix":"git.internal"}],` +
		`"from_a_newer_wardynd":[{"id":"bedrock-org","kind":"bedrock_sso"}]}`
	if _, err := pool.Exec(ctx, `INSERT INTO site_config (singleton, config) VALUES (true, $1::jsonb)`, newer); err != nil {
		t.Fatalf("seed a newer binary's site config: %v", err)
	}

	st := store.NewPG(pool)
	cfg, err := st.GetSiteConfig(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	cfg.ScmHosts = []string{"dev.azure.com"}
	cfg.InternalHosts = nil // a declared key this save clears
	if _, err := st.PutSiteConfig(ctx, cfg); err != nil {
		t.Fatalf("put: %v", err)
	}

	got := storedDoc(t, pool.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan)
	if !jsonEq(got["from_a_newer_wardynd"], `[{"id":"bedrock-org","kind":"bedrock_sso"}]`) {
		t.Errorf("from_a_newer_wardynd after an older binary's save = %s, want the newer binary's value kept", got["from_a_newer_wardynd"])
	}
	if !jsonEq(got["scm_hosts"], `["dev.azure.com"]`) {
		t.Errorf("scm_hosts = %s, want this save's value", got["scm_hosts"])
	}
	if v, ok := got["internal_hosts"]; ok {
		t.Errorf("internal_hosts = %s, want it cleared — a key this binary declares must still clear", v)
	}
}

// TestGovernanceLimitsRoundTripPreservesUnknownKeys: the same rule for a
// profile's limits, where the drop is a widening — an absent limit is no limit.
func TestGovernanceLimitsRoundTripPreservesUnknownKeys(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	p := seedGovernanceProfile(t, st, "test-skew-"+uuid.NewString())
	if _, err := pool.Exec(ctx,
		`UPDATE governance_profiles SET limits = limits || '{"limit_from_a_newer_wardynd": 3600}'::jsonb WHERE id = $1`, p.ID); err != nil {
		t.Fatalf("seed a newer binary's limit: %v", err)
	}

	p.Limits = types.GovernanceLimits{DenyTaskModeExec: true} // drops the declared DenyInteractive
	if _, err := st.UpsertGovernanceProfile(ctx, p); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := storedDoc(t, pool.QueryRow(ctx, `SELECT limits FROM governance_profiles WHERE id = $1`, p.ID).Scan)
	if !jsonEq(got["limit_from_a_newer_wardynd"], `3600`) {
		t.Errorf("limits after an older binary's edit = %s, want limit_from_a_newer_wardynd kept — dropping it removes the limit", got)
	}
	if !jsonEq(got["deny_task_mode_exec"], `true`) {
		t.Errorf("deny_task_mode_exec = %s, want this edit's value", got["deny_task_mode_exec"])
	}
	if v, ok := got["deny_interactive"]; ok {
		t.Errorf("deny_interactive = %s, want it cleared — a key this binary declares must still clear", v)
	}
}

// TestGovernanceCeilingRoundTripPreservesUnknownKeys is
// TestGovernanceLimitsRoundTripPreservesUnknownKeys's sibling for ceiling
// (#675): a full RunPolicySpec document has the same forward-compat problem —
// a field a newer wardynd added must survive an older binary's edit to an
// unrelated field — and until this fix ceiling = EXCLUDED.ceiling did a whole-
// document replace instead of the key-preserving merge limits already used.
func TestGovernanceCeilingRoundTripPreservesUnknownKeys(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	p := seedGovernanceProfile(t, st, "test-skew-ceiling-"+uuid.NewString())
	if _, err := pool.Exec(ctx,
		`UPDATE governance_profiles SET ceiling = ceiling || '{"ceiling_from_a_newer_wardynd": "bedrock_sso"}'::jsonb WHERE id = $1`, p.ID); err != nil {
		t.Fatalf("seed a newer binary's ceiling field: %v", err)
	}

	p.Ceiling.AllowAllEgress = true // a declared field this edit sets
	if _, err := st.UpsertGovernanceProfile(ctx, p); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := storedDoc(t, pool.QueryRow(ctx, `SELECT ceiling FROM governance_profiles WHERE id = $1`, p.ID).Scan)
	if !jsonEq(got["ceiling_from_a_newer_wardynd"], `"bedrock_sso"`) {
		t.Errorf("ceiling after an older binary's edit = %s, want ceiling_from_a_newer_wardynd kept", got)
	}
	if !jsonEq(got["allow_all_egress"], `true`) {
		t.Errorf("allow_all_egress = %s, want this edit's value", got["allow_all_egress"])
	}

	p.Ceiling.AllowAllEgress = false // a declared field this edit clears back to its omitempty zero
	if _, err := st.UpsertGovernanceProfile(ctx, p); err != nil {
		t.Fatalf("update: %v", err)
	}
	got = storedDoc(t, pool.QueryRow(ctx, `SELECT ceiling FROM governance_profiles WHERE id = $1`, p.ID).Scan)
	if v, ok := got["allow_all_egress"]; ok {
		t.Errorf("allow_all_egress = %s, want it cleared — a key this binary declares must still clear", v)
	}
	if !jsonEq(got["ceiling_from_a_newer_wardynd"], `"bedrock_sso"`) {
		t.Errorf("ceiling_from_a_newer_wardynd = %s, want it still kept after the clearing edit", got["ceiling_from_a_newer_wardynd"])
	}
}

func storedDoc(t *testing.T, scan func(...any) error) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := scan(&raw); err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the stored document %s: %v", raw, err)
	}
	return doc
}

// jsonEq compares by value: jsonb re-renders whitespace and key order.
func jsonEq(got json.RawMessage, want string) bool {
	var g, w any
	return json.Unmarshal(got, &g) == nil && json.Unmarshal([]byte(want), &w) == nil && reflect.DeepEqual(g, w)
}
