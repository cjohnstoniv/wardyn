// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const approvalRuleBody = `{"mode":"enforce","kinds":["tool_call","egress_domain"],"match":{"repos":["Org/Repo"]},` +
	`"approve":{"tools":["mcp__github__create_pull_request_review"],"hosts":["registry.npmjs.org"],"egress_scope":"run"}}`

// TestApprovalRuleWritesAreFourEyesExceptDelete (L1): with WARDYN_GOVERNANCE_SECOND_HUMAN on, a create and every edit,
// enforce -> dry_run included, are held for a second human; only a delete applies at once.
func TestApprovalRuleWritesAreFourEyesExceptDelete(t *testing.T) {
	e := newGovEnv(t)
	ctx := context.Background()
	const path = "/api/v1/governance/approval-rules/ci-review"

	ch := e.pending(e.call(e.alice, http.MethodPut, path, approvalRuleBody))
	if _, err := e.pg.GetApprovalRule(ctx, "ci-review"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a held create was written: %v", err)
	}
	e.approve(ch.ID)
	got, err := e.pg.GetApprovalRule(ctx, "ci-review")
	if err != nil || got.Version != 1 || got.Mode != types.ApprovalRuleEnforce || got.Match.Repos[0] != "org/repo" {
		t.Fatalf("approved create = %+v, %v; want version 1, enforce, the repo case-folded", got, err)
	}
	if rows := e.audits(approvalRuleWriteAction); len(rows) != 1 || !jsonHas(rows[0].Data, "spec") {
		t.Fatalf("%s rows = %+v; want one carrying the spec", approvalRuleWriteAction, rows)
	}

	narrowing := `{"mode":"dry_run","kinds":["tool_call"],"match":{"repos":["org/repo"]},"approve":{"tools":["mcp__github__create_pull_request_review"]}}`
	edit := e.pending(e.call(e.alice, http.MethodPut, path, narrowing))
	if cur, _ := e.pg.GetApprovalRule(ctx, "ci-review"); cur.Version != 1 || cur.Mode != types.ApprovalRuleEnforce {
		t.Fatalf("a held enforce -> dry_run edit was written: %+v", cur)
	}
	e.approve(edit.ID)
	if cur, _ := e.pg.GetApprovalRule(ctx, "ci-review"); cur.Version != 2 || cur.Mode != types.ApprovalRuleDryRun {
		t.Fatalf("approved edit = %+v; want version 2, dry_run", cur)
	}

	if w := e.call(e.alice, http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204 applied at once", w.Code, w.Body)
	}
	if _, err := e.pg.GetApprovalRule(ctx, "ci-review"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete left the rule: %v", err)
	}
	if rows := e.audits(approvalRuleDeleteAction); len(rows) != 1 || !jsonHas(rows[0].Data, "spec") {
		t.Fatalf("%s rows = %+v; want one carrying the removed spec", approvalRuleDeleteAction, rows)
	}
	if w := e.call(e.user, http.MethodPut, path, approvalRuleBody); w.Code != http.StatusForbidden {
		t.Fatalf("a member's PUT = %d, want 403", w.Code)
	}
}

// TestApprovalRuleWriteRefusals: a host the hard_limits boundary does not allow, and a scope a rule may never use, are
// refused at write.
func TestApprovalRuleWriteRefusals(t *testing.T) {
	e := newGovEnv(t)
	const path = "/api/v1/governance/approval-rules/ci-review"
	for name, body := range map[string]string{
		"always":  `{"kinds":["egress_domain"],"match":{"repos":["o/r"]},"approve":{"hosts":["registry.npmjs.org"],"egress_scope":"always"}}`,
		"until":   `{"kinds":["egress_domain"],"match":{"repos":["o/r"]},"approve":{"hosts":["registry.npmjs.org"],"egress_scope":"until"}}`,
		"glob":    `{"kinds":["tool_call"],"match":{"repos":["o/r"]},"approve":{"tools":["mcp__github__*"]}}`,
		"kind":    `{"kinds":["credential"],"match":{"repos":["o/r"]},"approve":{}}`,
		"wild":    `{"kinds":["egress_domain"],"match":{"repos":["o/r"]},"approve":{"hosts":["*.npmjs.org"]}}`,
		"nomatch": `{"kinds":["tool_call"],"match":{},"approve":{"tools":["t"]}}`,
	} {
		if w := e.call(e.alice, http.MethodPut, path, body); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonApprovalRuleInvalid {
			t.Errorf("%s: %d %s, want 400 %s", name, w.Code, w.Body, reasonApprovalRuleInvalid)
		}
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO governance_boundary (singleton, mode, spec, limits)
		VALUES (true, 'hard_limits', '{"allowed_domains":["other.example"]}', '{}')`); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.alice, http.MethodPut, path, approvalRuleBody); w.Code != http.StatusConflict || wireReason(t, w) != reasonApprovalRuleHost {
		t.Fatalf("a host outside the boundary: %d %s, want 409 %s", w.Code, w.Body, reasonApprovalRuleHost)
	}
}

func jsonHas(raw json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && len(m[key]) > 0
}
