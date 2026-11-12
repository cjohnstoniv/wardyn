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
// the policy in the run's run.policy.resolve row, and data.policy_generation, 1 for that policy. A revive
// records the run's next policy as run.policy.generation; rows under it carry its digest and number, and a
// row from the run's proxy carries the generation of the run token it presented. It answers "which policy
// was this action under" from the row alone. Only PolicyStampRecorder sets the keys; a writer's own values
// are removed.
//
// Not stamped: rows with no run, rows written before dispatch recorded the policy (run.create, the policy
// apply rows, queue rows), and rows a device forwards, which never pass the recorder. An absent key means
// "before the run's policy was recorded, or not recorded". On by default: it adds one data key and refuses
// nothing.

// PolicyDigestDatum and PolicyGenerationDatum are the data keys the stamp rides under.
const (
	PolicyDigestDatum     = "policy_digest"
	PolicyGenerationDatum = "policy_generation"
)

// policyTokenKey carries the run token a request from a run's proxy was authenticated by.
type policyTokenKey struct{}

type policyToken struct {
	run uuid.UUID
	jti string
}

// WithPolicyToken marks ctx as a request from run's proxy, authenticated by the run token whose id is jti
// ("" when the token is not known, as for a decision a runner forwards from its spool). A row of that run
// recorded under ctx is stamped with the policy generation the token belongs to
// (types.RunPolicyState.Stamp); a row of any other run is stamped as the control plane's.
func WithPolicyToken(ctx context.Context, run uuid.UUID, jti string) context.Context {
	return context.WithValue(ctx, policyTokenKey{}, policyToken{run: run, jti: jti})
}

// PolicyStampFor returns the stamp of a row of runID recorded under ctx, from the run's state.
func PolicyStampFor(ctx context.Context, runID uuid.UUID, st types.RunPolicyState) types.PolicyStamp {
	tok, fromProxy := ctx.Value(policyTokenKey{}).(policyToken)
	if fromProxy && tok.run != runID {
		fromProxy = false
	}
	return st.Stamp(fromProxy, tok.jti)
}

// StampPolicy returns data with policy_digest and policy_generation set from s, or, for the zero stamp,
// with both keys removed, so no writer's data can carry them unless the recorder put them there. An
// unknown stamp carries policy_digest "unknown" and no generation. A stamped row whose data is not a
// JSON object keeps it whole under "data", the way StampDryRun does.
func StampPolicy(data json.RawMessage, s types.PolicyStamp) json.RawMessage {
	// An escaped spelling of a key ("policy\u005fdigest") reads as the key too, so the quick exit
	// needs neither literal key nor any backslash.
	if s.Digest == "" && !bytes.Contains(data, []byte(`"`+PolicyDigestDatum+`"`)) &&
		!bytes.Contains(data, []byte(`"`+PolicyGenerationDatum+`"`)) && bytes.IndexByte(data, '\\') < 0 {
		return data
	}
	m := map[string]json.RawMessage{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &m); err != nil {
			if s.Digest == "" {
				return data
			}
			m = map[string]json.RawMessage{"data": data}
		}
	}
	delete(m, PolicyDigestDatum)
	delete(m, PolicyGenerationDatum)
	// A string or an int cannot fail to marshal.
	if s.Digest != "" {
		m[PolicyDigestDatum], _ = json.Marshal(s.Digest)
	}
	if s.Generation > 0 {
		m[PolicyGenerationDatum], _ = json.Marshal(s.Generation)
	}
	// A map of RawMessage cannot fail to marshal.
	out, _ := json.Marshal(m)
	return out
}

// PolicyStampRecorder stamps data.policy_digest and data.policy_generation on every event that names a
// run, from Lookup and the run token ctx carries (WithPolicyToken), and removes both keys from every
// other. A Lookup that fails records the row without the keys and never drops it. A nil Lookup stamps
// nothing. cmd/wardynd puts it between the coalescer and masking, so pass-through rows and the
// coalescer's summaries are both stamped, and the keys are set before seal, spool and store.
type PolicyStampRecorder struct {
	Inner Recorder
	// Lookup returns the run's policy state; the zero state for a run with none recorded (including one
	// with no row yet).
	Lookup func(ctx context.Context, runID uuid.UUID) (types.RunPolicyState, error)
}

// lastPolicyStampWarn is when a failed lookup was last logged, in unix nanoseconds.
var lastPolicyStampWarn atomic.Int64

const policyStampWarnEvery = 30 * time.Second

// Record stamps ev and hands it on.
func (r PolicyStampRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	var stamp types.PolicyStamp
	if ev.RunID != nil && r.Lookup != nil {
		st, err := r.Lookup(ctx, *ev.RunID)
		if err != nil {
			if now := time.Now().UnixNano(); now-lastPolicyStampWarn.Load() > int64(policyStampWarnEvery) {
				lastPolicyStampWarn.Store(now)
				slog.Warn("audit: run policy digest unreadable, row recorded without policy_digest", slog.String("action", ev.Action), slog.Any("err", err))
			}
		} else {
			stamp = PolicyStampFor(ctx, *ev.RunID, st)
		}
	}
	ev.Data = StampPolicy(ev.Data, stamp)
	return r.Inner.Record(ctx, ev)
}
