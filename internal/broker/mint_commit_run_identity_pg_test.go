// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAudit_MintRowCarriesRunIdentity_PG: the credential.mint row is written on the mint's own transaction,
// past the recorders, so the mint stamps the run identity itself, from the context a verified run token made.
// With no run identity on the context its data is as it was.
func TestAudit_MintRowCarriesRunIdentity_PG(t *testing.T) {
	for name, tc := range map[string]struct {
		id      *audit.RunIdentity
		want    map[string]string
		wantNot []string
	}{
		"person-owned run": {&audit.RunIdentity{Owner: "alice@example.com", Sponsor: "ignored", Workload: "spiffe://wardyn.test/agent-run/x"},
			map[string]string{"run_owner": "alice@example.com", "run_workload": "spiffe://wardyn.test/agent-run/x"}, []string{"sponsor"}},
		"service-owned run": {&audit.RunIdentity{Owner: "service:ci", Sponsor: "boss@example.com", Workload: "spiffe://wardyn.test/agent-run/y"},
			map[string]string{"run_owner": "service:ci", "sponsor": "boss@example.com", "run_workload": "spiffe://wardyn.test/agent-run/y"}, nil},
		"no run identity": {nil, nil, []string{"run_owner", "run_workload", "sponsor"}},
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgPool(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			runID := uuid.New()
			seedRun(ctx, t, pool, runID)
			scope := pgGithubScope(t)
			grantID := pgSeedGrant(ctx, t, pool, runID, types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600})
			pgSeedApproval(ctx, t, pool, runID, grantID, scope)
			gh := &FakeGitHubMinter{Token: "ghs_run_identity_token"}
			b := New(NewPgxStore(pool), nil, poolAudit{pool}, nil, gh)
			if tc.id != nil {
				ctx = audit.WithRunIdentity(ctx, tc.id.Owner, tc.id.Sponsor, tc.id.Workload)
			}
			if _, err := b.MintForGrant(ctx, callerFor(runID), grantID); err != nil {
				t.Fatalf("mint: %v", err)
			}
			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT data FROM audit_events WHERE run_id = $1 AND action = 'credential.mint' AND outcome = 'success'`, runID).Scan(&raw); err != nil {
				t.Fatalf("read the mint row: %v", err)
			}
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.want {
				if data[k] != v {
					t.Errorf("data[%q] = %v, want %q (data %s)", k, data[k], v, raw)
				}
			}
			for _, k := range tc.wantNot {
				if _, ok := data[k]; ok {
					t.Errorf("data has %q, want it absent (data %s)", k, raw)
				}
			}
		})
	}
}
