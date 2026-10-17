// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration0180_RunnerOrgTrust applies the org-runner and trust-class migration over a runner and a
// token from before it: both become person, the runner personal with no writer, the token a human's
// without break-glass. Afterwards the schema refuses an owner kind outside the set, an org row with an
// owner, a trust class outside the set, org_cluster on a person's runner, any other class on an org
// runner, and break-glass on a human's token, and it admits an org runner at org_cluster.
func TestMigration0180_RunnerOrgTrust(t *testing.T) {
	pool, _ := partialSchemaPool(t, "0180")
	runner, token := uuid.New(), uuid.New()
	pgExec(t, pool, `INSERT INTO runners (id, owner, public_key, key_fingerprint, state, max_runs) VALUES ($1, 'sub-a', $2, $3, 'claimed', 2)`,
		runner, append(runner[:], runner[:]...), "fp-"+runner.String())
	pgExec(t, pool, `INSERT INTO runner_registration_tokens (id, owner, minted_by, token_sha256, org_url_sha256, created_at, expires_at)
		VALUES ($1, 'sub-a', 'sub-b', 'sha', 'org', now(), now() + interval '1 hour')`, token)

	migrateBefore(t, pool, "0181")

	if got := pgScalar[string](t, pool, `SELECT owner_kind || '/' || trust_class || '/' || trust_set_by || '/' || (trust_set_at IS NULL)::text
		|| '/' || (registration_token_id IS NULL)::text FROM runners WHERE id = $1`, runner); got != "person/personal//true/true" {
		t.Errorf("an existing runner became %q, want person/personal with no writer and no token", got)
	}
	if got := pgScalar[string](t, pool, `SELECT owner_kind || '/' || minted_by_kind || '/' || minted_by_email || '/' || break_glass::text
		FROM runner_registration_tokens WHERE id = $1`, token); got != "person/human//false" {
		t.Errorf("an existing token became %q, want person/human without break-glass", got)
	}
	rid, tid := "'"+runner.String()+"'", "'"+token.String()+"'"
	refused := map[string]string{
		"an unknown owner kind":            `UPDATE runners SET owner_kind = 'team' WHERE id = ` + rid,
		"an org runner with an owner":      `UPDATE runners SET owner_kind = 'org', trust_class = 'org_cluster' WHERE id = ` + rid,
		"an unknown trust class":           `UPDATE runners SET trust_class = 'trusted' WHERE id = ` + rid,
		"org_cluster on a person's":        `UPDATE runners SET trust_class = 'org_cluster' WHERE id = ` + rid,
		"an org runner not org_cluster":    `UPDATE runners SET owner = '', owner_kind = 'org', trust_class = 'managed_device' WHERE id = ` + rid,
		"an org token with an owner":       `UPDATE runner_registration_tokens SET owner_kind = 'org' WHERE id = ` + tid,
		"an unknown minter kind":           `UPDATE runner_registration_tokens SET minted_by_kind = 'robot' WHERE id = ` + tid,
		"break-glass on a human's token":   `UPDATE runner_registration_tokens SET break_glass = true WHERE id = ` + tid,
		"an unknown token owner kind":      `UPDATE runner_registration_tokens SET owner_kind = 'team' WHERE id = ` + tid,
		"a person's runner as org_cluster": `UPDATE runners SET trust_class = 'org_cluster', owner_kind = 'person' WHERE id = ` + rid,
	}
	for what, stmt := range refused {
		if _, err := pool.Exec(t.Context(), stmt); err == nil || !strings.Contains(err.Error(), "check") {
			t.Errorf("the schema accepts %s: %v", what, err)
		}
	}
	pgExec(t, pool, `UPDATE runners SET trust_class = 'managed_device' WHERE id = `+rid)
	pgExec(t, pool, `UPDATE runners SET owner = '', owner_kind = 'org', trust_class = 'org_cluster' WHERE id = `+rid)
	pgExec(t, pool, `UPDATE runner_registration_tokens SET owner = '', owner_kind = 'org', minted_by_kind = 'admin_token', break_glass = true WHERE id = `+tid)
}
