// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ReasonPolicyDigest is the failure for a run.policy.resolve or run.policy.generation row whose stamped
// policy_digest is not the digest of the policy the same row records, on a row whose content verified and was
// not masked.
const ReasonPolicyDigest = "policy_digest_mismatch"

// Policy verdicts. A policy row (resolve or generation) is verified (its recomputed digest is its stamp), masked (it differs, and
// the row shows the masking recorder rewrote it after the stamp was taken, so the difference is not
// evidence of anything) or mismatch. An action row carrying a stamp is linked to the policy row in the
// bundle that carries the same digest, or unlinked when the bundle holds none or its stamp is unknown.
const (
	PolicyVerified = "verified"
	PolicyMasked   = "masked"
	PolicyMismatch = "mismatch"
	PolicyLinked   = "linked"
	PolicyUnlinked = "unlinked"
)

// PolicyResult is the verdict on one stamped row.
type PolicyResult struct {
	Checkpoint int       `json:"checkpoint"`
	Seq        int64     `json:"seq"`
	ID         uuid.UUID `json:"id"`
	Action     string    `json:"action"`
	Digest     string    `json:"policy_digest"`
	Status     string    `json:"status"`
	// Resolve is, for a linked action row, the seq of a run.policy.resolve or run.policy.generation row with
	// matching policy content (the same digest); it does not show that this row ran under that policy.
	Resolve int64 `json:"resolve_seq,omitempty"`
}

// policyRows are the actions whose row records a run's policy, each with the data key that holds it: dispatch's
// resolve row carries the policy flat (""), a revive's generation row under "policy".
var policyRows = map[string]string{"run.policy.resolve": "", "run.policy.generation": "policy"}

// maskedMarkers are what the audit masking recorder leaves in a row it rewrote: the placeholder it writes in
// place of a registered secret (raw, and as Go's JSON escapes it) and the scope label it adds when its corpus
// for the run was partial. The stamp is taken before masking, so either explains a recomputed digest that
// differs from it.
var maskedMarkers = []string{"<secret-hidden>", `<secret-hidden>`, `"mask_scope"`}

// checkPolicies recomputes each verified policy row's digest (run.policy.resolve or run.policy.generation)
// and links the stamped rows of the bundle to the first policy row with matching policy content (an equal digest). Only a row whose content
// re-hashed is read: a withheld or failed row's data is whatever the bundle says, so it carries no verdict and
// links nothing.
func checkPolicies(b Bundle, rep *Report) {
	var results []PolicyResult
	linkable := map[string]int64{} // digest -> seq of the (verified or masked) policy row carrying it
	n := 0
	for ci, c := range b.Checkpoints {
		for _, ev := range c.Events {
			if n >= len(rep.Events) {
				return
			}
			er := rep.Events[n]
			n++
			if !er.OK || ev.Row.Data == nil {
				continue
			}
			stamp := stampOf(*ev.Row.Data)
			if stamp == "" {
				continue
			}
			res := PolicyResult{Checkpoint: ci, Seq: ev.Row.Seq, ID: ev.Row.ID, Action: ev.Row.Action, Digest: stamp}
			key, isPolicy := policyRows[ev.Row.Action]
			// An unknown stamp names no policy, so it neither links nor is linked to.
			if !isPolicy || stamp == types.PolicyDigestUnknown {
				res.Status = PolicyUnlinked
				results = append(results, res)
				continue
			}
			spec, err := recordedPolicy(*ev.Row.Data, key)
			switch {
			case err == nil && spec.Digest() == stamp:
				res.Status = PolicyVerified
			case isMasked(*ev.Row.Data):
				res.Status = PolicyMasked
			default:
				res.Status = PolicyMismatch
				rep.Failures = append(rep.Failures, Failure{Checkpoint: ci, Seq: ev.Row.Seq, Reason: ReasonPolicyDigest,
					Detail: "the policy this row records does not digest to its policy_digest (a verifier from a different Wardyn version can compute another digest)"})
			}
			if res.Status != PolicyMismatch {
				if _, seen := linkable[stamp]; !seen {
					linkable[stamp] = ev.Row.Seq
				}
			}
			results = append(results, res)
		}
	}
	for i := range results {
		if results[i].Status == PolicyUnlinked && results[i].Digest != types.PolicyDigestUnknown {
			if seq, ok := linkable[results[i].Digest]; ok {
				results[i].Status, results[i].Resolve = PolicyLinked, seq
			}
		}
	}
	rep.Policies = results
}

// recordedPolicy decodes the policy a policy row records: the whole data when key is "", else data[key].
func recordedPolicy(data, key string) (types.RunPolicySpec, error) {
	var spec types.RunPolicySpec
	raw := json.RawMessage(data)
	if key != "" {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return spec, err
		}
		raw = m[key]
	}
	return spec, json.Unmarshal(raw, &spec)
}

// stampOf is the policy_digest a row's data carries, "" when it carries none or is not an object.
func stampOf(data string) string {
	var m struct {
		PolicyDigest string `json:"policy_digest"`
	}
	if json.Unmarshal([]byte(data), &m) != nil {
		return ""
	}
	return m.PolicyDigest
}

func isMasked(data string) bool {
	for _, m := range maskedMarkers {
		if strings.Contains(data, m) {
			return true
		}
	}
	return false
}
