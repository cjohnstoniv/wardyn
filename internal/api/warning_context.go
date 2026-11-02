// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"regexp"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// warningContext is a drop warning's typed companion: which warnings[] entry it is about, and the
// policy whose ceiling the run is bound by, with the contact that policy publishes. The sentence is
// unchanged; the console names the policy and the way to ask under it. It is context, not
// attribution: the leaf (or the deployment), never the layer of the tree that set the allowlist.
type warningContext struct {
	WarningIndex int            `json:"warning_index"`
	Kind         string         `json:"kind"`
	Policy       *policyref.Ref `json:"policy"`
}

const warningKindEgressPolicyDrop = "egress_policy_drop"

// egressPolicyDrop matches the two warnings a ceiling's allowlist writes when it drops a host from a
// member's policy: composer.ClampMember's and dropUncoveredPrivateLiterals'. A workspace's dropped clone
// host or egress requirement carries its own remedy and gets no context.
var egressPolicyDrop = regexp.MustCompile(`^dropped ([0-9]+ egress domain\(s\) not in operator allowlist: |private address entry ".*": not in operator allowlist)`)

// warningContexts is the context for each policy drop in warnings, by its index in that same slice, so
// a caller that filters or reorders warnings afterwards must compute it again. Nil when there is none or
// the caller is not bound by a ceiling (an operator).
func (s *Server) warningContexts(ctx context.Context, ceiling governanceCeiling, warnings []string) []warningContext {
	var out []warningContext
	var ref *policyref.Ref
	for i, w := range warnings {
		if !egressPolicyDrop.MatchString(w) {
			continue
		}
		if ref == nil {
			if ref = s.ceilingPolicy(ctx, ceiling); ref == nil {
				return nil
			}
		}
		out = append(out, warningContext{WarningIndex: i, Kind: warningKindEgressPolicyDrop, Policy: ref})
	}
	return out
}
