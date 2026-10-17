// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// LiveAWSSSOGrantLister lists the AWS SSO injection grants the background
// renewer keeps current (internal/api/awssso_renewer.go).
type LiveAWSSSOGrantLister interface {
	// ListLiveAWSSSOGrants returns every grant whose scope names secretName on
	// a run that is still live: in a slot-holding state, not paused, not lost
	// or past its end, and not revoked (the identity_revocations check the
	// broker's mint refuses on). Its approval is the broker's too: the grant's
	// latest credential approval, if it has one, is APPROVED for this run on
	// exactly this scope; one with none must not require one.
	ListLiveAWSSSOGrants(ctx context.Context, secretName string) ([]types.CredentialGrant, error)
}

var _ LiveAWSSSOGrantLister = PG{}

// ListLiveAWSSSOGrants implements LiveAWSSSOGrantLister.
func (s PG) ListLiveAWSSSOGrants(ctx context.Context, secretName string) ([]types.CredentialGrant, error) {
	const q = `
		SELECT g.id, g.run_id, g.created_at, g.spec, g.delivery
		  FROM credential_grants g
		  JOIN agent_runs r ON r.id = g.run_id
		 WHERE g.spec->'scope'->>'secret_name' = $1
		   AND r.state = ANY($2)
		   AND r.paused_at IS NULL AND r.lost_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM identity_revocations v WHERE v.run_id = g.run_id)
		   AND COALESCE((SELECT a.state = 'APPROVED' AND a.run_id = g.run_id AND a.requested_scope = g.spec->'scope'
		                   FROM approvals a WHERE a.grant_id = g.id AND a.kind = 'credential'
		                  ORDER BY a.requested_at DESC LIMIT 1),
		                NOT COALESCE((g.spec->>'requires_approval')::boolean, false))
		 ORDER BY g.created_at, g.id`
	states := make([]string, len(types.SlotHoldingRunStates))
	for i, st := range types.SlotHoldingRunStates {
		states[i] = string(st)
	}
	return collect(ctx, s.Pool, "list", "live aws sso grants", q, []any{secretName, states}, scanGrant)
}
