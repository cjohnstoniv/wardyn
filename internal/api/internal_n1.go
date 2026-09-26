// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// normalizeN1Decision maps an N-1 (0.7.12) proxy's wire vocabulary onto the
// current names, in place, before handlePostDecision's first read of dl: a
// scan error is a failure, "blind" is coverage-only, skipped is skip, a
// dropped-decisions summary is not a policy deny, and a hold is egress.hold —
// the meaning each value carried on the wire in 0.7 (owner ruling 2026-09-25,
// issue #1063: a 0.7.12 proxy behind an 0.8 daemon is supported). New rows are
// always written under the 0.8 names, regardless of which proxy sent them;
// persisted rows already written under the old names are untouched.
//
// 0.7.12 wire vocabulary (N−1 window, long-holds rev 4 §4 row 5); delete when
// the fixture's release moves to 0.8.x.
func normalizeN1Decision(dl *egress.DecisionLog) {
	if dl.Decision == "pending" {
		dl.Decision = egress.Pending
	}
	if dl.Scan != nil {
		switch dl.Scan.Action {
		case "error":
			dl.Scan.Action = "fail"
		case "blind":
			dl.Scan.Action = "bypass"
		case "skipped":
			dl.Scan.Action = "skip"
		}
	}
	if suffix, ok := strings.CutPrefix(dl.RuleSource, "egress.decisions.dropped:"); ok {
		dl.RuleSource = "egress:dropped-decisions-" + suffix
	}
}
