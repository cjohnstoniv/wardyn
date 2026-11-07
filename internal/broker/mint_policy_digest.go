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

// stampPolicyDigestTx returns ev's data with the run's policy digest (data.policy_digest) set, read on the
// mint's own transaction. The mint row is written there, past the audit recorder chain that stamps every
// other row (audit.PolicyStampRecorder), so it stamps itself. A run with no digest, or no row, carries
// no key, and a key the row came with is removed either way.
func stampPolicyDigestTx(ctx context.Context, tx Querier, ev types.AuditEvent) (json.RawMessage, error) {
	var digest string
	if ev.RunID != nil {
		if err := tx.QueryRow(ctx, `SELECT policy_digest FROM agent_runs WHERE id = $1`, *ev.RunID).Scan(&digest); err != nil && !errors.Is(err, errNoRow) {
			return nil, fmt.Errorf("broker: read run policy digest: %w", err)
		}
	}
	return audit.StampPolicyDigest(ev.Data, digest), nil
}
