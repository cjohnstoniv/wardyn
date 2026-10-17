// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Dry run: POST /runs/preflight reproduces launch's gates, and a gate that
// refuses writes its authz.denied row from inside the shared path. Those rows
// are real but bound no run, so each carries data.dry_run: true. The marker is
// set by DryRunRecorder from a mark on the request context, never by a writer,
// so no door's detail can set or clear it.

type dryRunKey struct{}

// WithDryRun marks ctx as serving a dry run.
func WithDryRun(ctx context.Context) context.Context {
	return context.WithValue(ctx, dryRunKey{}, true)
}

// DryRunFrom reports whether ctx serves a dry run.
func DryRunFrom(ctx context.Context) bool {
	ok, _ := ctx.Value(dryRunKey{}).(bool)
	return ok
}

// DryRunDatum is the data key the marker rides under.
const DryRunDatum = "dry_run"

// StampDryRun returns data with dry_run set true when ctx serves a dry run, and
// with any dry_run key removed otherwise, so no writer's data can carry the
// marker outside preflight. A marked row whose data is not a JSON object keeps
// it whole under "data", the way StampDelegation does.
func StampDryRun(ctx context.Context, data json.RawMessage) json.RawMessage {
	marked := DryRunFrom(ctx)
	if !marked && !bytes.Contains(data, []byte(`"`+DryRunDatum+`"`)) {
		return data
	}
	m := map[string]json.RawMessage{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &m); err != nil {
			if !marked {
				return data
			}
			m = map[string]json.RawMessage{"data": data}
		}
	}
	if marked {
		m[DryRunDatum] = json.RawMessage("true")
	} else {
		delete(m, DryRunDatum)
	}
	// A map of RawMessage cannot fail to marshal.
	out, _ := json.Marshal(m)
	return out
}

// DryRunRecorder stamps data.dry_run on every event recorded under a dry-run
// context, whoever wrote it, and strips the key from every other. cmd/wardynd
// puts it at the head of the shared chain beside DelegationRecorder.
type DryRunRecorder struct{ Inner Recorder }

// Record stamps ev and hands it on.
func (r DryRunRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	ev.Data = StampDryRun(ctx, ev.Data)
	return r.Inner.Record(ctx, ev)
}
