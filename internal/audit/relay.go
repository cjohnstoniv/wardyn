// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

type relayKey struct{}

// WithRelay marks a request with its authenticated runner, never a client header.
func WithRelay(ctx context.Context, runnerID uuid.UUID) context.Context {
	return context.WithValue(ctx, relayKey{}, runnerID)
}

// StampRelay adds the runner transport without changing the event's actor or via.
func StampRelay(ctx context.Context, data json.RawMessage) json.RawMessage {
	id, ok := ctx.Value(relayKey{}).(uuid.UUID)
	if !ok || id == uuid.Nil {
		return data
	}
	fields := map[string]json.RawMessage{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &fields); err != nil {
			fields = map[string]json.RawMessage{"data": data}
		}
	}
	fields["relay"], _ = json.Marshal("runner:" + id.String())
	out, _ := json.Marshal(fields)
	return out
}
