// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Rows written under a verified run token name who the run belongs to. The identity comes from the token's own
// claims (or, for a row a runner spooled, from the stored run), never from a request body, and it is stored
// under keys no other audit writer uses, so the "owner" and "workload" keys that secret reads, key erasure and
// the gateway's first mint row already carry keep their values and meanings.

const (
	// RunOwnerKey is the data key of the principal the run acts for.
	RunOwnerKey = "run_owner"
	// RunWorkloadKey is the data key of the run's workload identity.
	RunWorkloadKey = "run_workload"
	// runSponsorKey is a service-owned run's sponsor, the key StampSponsor writes.
	runSponsorKey = "sponsor"
)

// RunIdentity is the identity a run token carries. Sponsor is kept only for an owner that is a service
// identity.
type RunIdentity struct {
	Owner    string
	Sponsor  string
	Workload string
}

type runIdentityKey struct{}

// WithRunIdentity marks ctx as a request made under a verified run token owned by owner, with the given
// workload identity. The sponsor is kept only when the owner is a service identity ("service:" prefix).
func WithRunIdentity(ctx context.Context, owner, sponsor, workload string) context.Context {
	if !strings.HasPrefix(owner, "service:") {
		sponsor = ""
	}
	return context.WithValue(ctx, runIdentityKey{}, RunIdentity{Owner: owner, Sponsor: sponsor, Workload: workload})
}

// RunIdentityFrom reports the run identity ctx's request was made under, when it was.
func RunIdentityFrom(ctx context.Context) (RunIdentity, bool) {
	id, ok := ctx.Value(runIdentityKey{}).(RunIdentity)
	return id, ok
}

// StampRunIdentity returns data with run_owner and run_workload set from id and, for a service-owned run, its
// sponsor. It overwrites those keys, and deletes a sponsor key when the owner is not a service identity or has
// none, so nothing posted by a caller survives. It reads, writes and deletes no other key. Data that is not a
// JSON object is kept whole under "data", as StampSponsor does.
func StampRunIdentity(data json.RawMessage, id RunIdentity) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &m); err != nil || m == nil {
			m = map[string]json.RawMessage{"data": data}
		}
	}
	set := func(key, value string) {
		if value == "" {
			delete(m, key)
			return
		}
		m[key], _ = json.Marshal(value) // a string cannot fail to marshal
	}
	set(RunOwnerKey, id.Owner)
	set(RunWorkloadKey, id.Workload)
	if strings.HasPrefix(id.Owner, "service:") {
		set(runSponsorKey, id.Sponsor)
	} else {
		delete(m, runSponsorKey)
	}
	out, _ := json.Marshal(m) // a map of RawMessage cannot fail to marshal
	return out
}

// RunIdentityRecorder stamps the run identity ctx carries on every event it records, and leaves an event
// recorded under no run identity as it is. cmd/wardynd wraps the head of the shared chain with it, so the
// stamped bytes are the ones the store hashes.
type RunIdentityRecorder struct{ Inner Recorder }

// Record stamps ev from ctx and hands it on.
func (r RunIdentityRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	ev.Data = r.stamp(ctx, ev.Data)
	return r.Inner.Record(ctx, ev)
}

// RecordBatch stamps each event as Record does and hands the batch on.
func (r RunIdentityRecorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	out := make([]types.AuditEvent, len(evs))
	for i, ev := range evs {
		ev.Data = r.stamp(ctx, ev.Data)
		out[i] = ev
	}
	return RecordBatch(ctx, r.Inner, out)
}

func (RunIdentityRecorder) stamp(ctx context.Context, data json.RawMessage) json.RawMessage {
	id, ok := RunIdentityFrom(ctx)
	if !ok {
		return data
	}
	return StampRunIdentity(data, id)
}

var _ BatchRecorder = RunIdentityRecorder{}
