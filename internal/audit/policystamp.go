// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Policy stamp: every audit row of a run written after dispatch recorded the run's policy carries
// data.policy_digest, the SHA-256 of that policy (types.RunPolicySpec.Digest), which is also the digest of
// the policy in the run's run.policy.resolve row. It answers "which policy was this action under" from the
// row alone. Only PolicyStampRecorder sets the key; a writer's own value is removed.
//
// Not stamped: rows with no run, rows written before dispatch recorded the policy (run.create, the policy
// apply rows, queue rows), and rows a device forwards, which never pass the recorder. An absent key means
// "before the run's policy was recorded, or not recorded". On by default: it adds one data key and refuses
// nothing.

// PolicyDigestDatum is the data key the stamp rides under.
const PolicyDigestDatum = "policy_digest"

// StampPolicyDigest returns data with policy_digest set to digest, or, when digest is empty, with any
// policy_digest key removed, so no writer's data can carry the key unless the recorder put it there. A
// stamped row whose data is not a JSON object keeps it whole under "data", the way StampDryRun does.
func StampPolicyDigest(data json.RawMessage, digest string) json.RawMessage {
	// An escaped spelling of the key ("policy\u005fdigest") reads as the key too, so the quick exit
	// needs neither the literal key nor any backslash.
	if digest == "" && !bytes.Contains(data, []byte(`"`+PolicyDigestDatum+`"`)) && bytes.IndexByte(data, '\\') < 0 {
		return data
	}
	m := map[string]json.RawMessage{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &m); err != nil {
			if digest == "" {
				return data
			}
			m = map[string]json.RawMessage{"data": data}
		}
	}
	if digest == "" {
		delete(m, PolicyDigestDatum)
	} else {
		// A string cannot fail to marshal.
		m[PolicyDigestDatum], _ = json.Marshal(digest)
	}
	// A map of RawMessage cannot fail to marshal.
	out, _ := json.Marshal(m)
	return out
}

// PolicyStampRecorder stamps data.policy_digest on every event that names a run, from Lookup, and
// removes the key from every other. A Lookup that fails records the row without the key and never drops
// it. A nil Lookup stamps nothing. cmd/wardynd puts it between the coalescer and masking, so pass-through
// rows and the coalescer's summaries are both stamped, and the key is set before seal, spool and store.
type PolicyStampRecorder struct {
	Inner Recorder
	// Lookup returns the run's digest, "" for a run with none recorded (including one with no row yet).
	Lookup func(ctx context.Context, runID uuid.UUID) (string, error)
}

// lastPolicyStampWarn is when a failed lookup was last logged, in unix nanoseconds.
var lastPolicyStampWarn atomic.Int64

const policyStampWarnEvery = 30 * time.Second

// Record stamps ev and hands it on.
func (r PolicyStampRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	var digest string
	if ev.RunID != nil && r.Lookup != nil {
		d, err := r.Lookup(ctx, *ev.RunID)
		if err != nil {
			if now := time.Now().UnixNano(); now-lastPolicyStampWarn.Load() > int64(policyStampWarnEvery) {
				lastPolicyStampWarn.Store(now)
				slog.Warn("audit: run policy digest unreadable, row recorded without policy_digest", slog.String("action", ev.Action), slog.Any("err", err))
			}
		} else {
			digest = d
		}
	}
	ev.Data = StampPolicyDigest(ev.Data, digest)
	return r.Inner.Record(ctx, ev)
}
