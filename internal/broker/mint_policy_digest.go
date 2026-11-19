// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stampPolicyDigestTx returns ev's data with the run's policy stamp (data.policy_digest and
// data.policy_generation) set, read on the mint's own transaction. The mint row is written there, past
// the audit recorder chain that stamps every other row (audit.PolicyStampRecorder), so it stamps itself,
// by the same rule: a mint the run's proxy asked for is under the generation of the run token it
// presented. A run with no digest, or no row, carries no keys, and keys the row came with are removed
// either way.
func stampPolicyDigestTx(ctx context.Context, tx Querier, ev types.AuditEvent) (json.RawMessage, error) {
	var st types.RunPolicyState
	if ev.RunID != nil {
		var gens []byte
		var jti, prev string
		err := tx.QueryRow(ctx, `SELECT policy_digest, policy_generations, COALESCE(token_jti, ''), COALESCE(token_prev_jti, '')
			FROM agent_runs WHERE id = $1`, *ev.RunID).Scan(&st.Digest, &gens, &jti, &prev)
		switch {
		case errors.Is(err, errNoRow):
		case err != nil:
			return nil, fmt.Errorf("broker: read run policy digest: %w", err)
		default:
			if err := json.Unmarshal(gens, &st.Generations); err != nil {
				return nil, fmt.Errorf("broker: read run policy generations: %w", err)
			}
			for _, id := range []string{jti, prev} {
				if id != "" {
					st.LiveTokenIDs = append(st.LiveTokenIDs, id)
				}
			}
		}
		return audit.StampPolicy(ev.Data, audit.PolicyStampFor(ctx, *ev.RunID, st)), nil
	}
	return audit.StampPolicy(ev.Data, types.PolicyStamp{}), nil
}
