// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Delegation (#1142): "no impersonation; delegation is recorded as
// delegation". A request a registered portal makes for a person carries the
// portal and its delegated token on its context, and every audit row written
// under that context names them as data.via beside the person who is the
// actor. The marker lives here rather than in internal/api because rows are
// written by more than the API — the identity provider's identity.mint during a
// run create, for one — and DelegationRecorder stamps them all.

type delegationKey struct{}

// WithDelegation marks ctx as a request made through via.
func WithDelegation(ctx context.Context, via types.DelegationVia) context.Context {
	return context.WithValue(ctx, delegationKey{}, via)
}

// DelegationFrom reports the portal and delegated token ctx's request came
// through, when it did.
func DelegationFrom(ctx context.Context) (types.DelegationVia, bool) {
	v, ok := ctx.Value(delegationKey{}).(types.DelegationVia)
	return v, ok
}

// StampDelegation returns data with via added under "via". Data that is not a
// JSON object is kept whole under "data", so no delegated row can lack the
// stamp.
func StampDelegation(data json.RawMessage, via types.DelegationVia) json.RawMessage {
	m := map[string]any{}
	if len(data) > 0 && string(data) != "null" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err == nil {
			for k, v := range obj {
				m[k] = v
			}
		} else {
			m["data"] = data
		}
	}
	m["via"] = via
	// A map of RawMessage and a struct of two UUIDs cannot fail to marshal.
	out, _ := json.Marshal(m)
	return out
}

// DelegationRecorder stamps data.via on every event recorded under a
// delegated context, whoever wrote it. cmd/wardynd puts it at the head of the
// shared chain.
type DelegationRecorder struct{ Inner Recorder }

// Record stamps ev when ctx is delegated and hands it on.
func (r DelegationRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if via, ok := DelegationFrom(ctx); ok {
		ev.Data = StampDelegation(ev.Data, via)
	}
	return r.Inner.Record(ctx, ev)
}
