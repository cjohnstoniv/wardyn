// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// viaActorJSON is a JSONB value for the agent a portal named: NULL for none, so a run or ticket with
// no agent writes exactly what it did before the column existed. Used by agent_runs.created_via_actor
// and attach_tickets.via_actor.
func viaActorJSON(a *types.DelegationActor) ([]byte, error) {
	if a == nil {
		return nil, nil
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("store: marshal delegation actor: %w", err)
	}
	return raw, nil
}

// scanViaActor reads one back: nil for NULL. A value that does not decode is an error, never "no agent",
// so a run is not read as unattributed when its row names one.
func scanViaActor(raw []byte) (*types.DelegationActor, error) {
	if raw == nil {
		return nil, nil
	}
	var a types.DelegationActor
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("store: unmarshal delegation actor: %w", err)
	}
	return &a, nil
}
