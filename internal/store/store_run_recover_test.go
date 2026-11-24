// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// TestRecoveryHeldExpr_JudgeAndSweepsShareOnePredicate: the STATE judge (storeRecovering "false") and the sweeps'
// RunRecoveryCheck (a bound boolean) read one hold predicate that differs only in that term, and both read the
// runner's latest report of the run, so a report of the proxy stopped or containment pending ends the hold for both.
func TestRecoveryHeldExpr_JudgeAndSweepsShareOnePredicate(t *testing.T) {
	if n := strings.Count(recoveryHeldSQL, storeRecoveringTerm); n != 1 {
		t.Fatalf("the hold predicate has %d store-recovering terms, want 1", n)
	}
	judge, sweep := recoveryHeldExpr("false"), recoveryHeldExpr("$2::boolean")
	if strings.Replace(sweep, "$2::boolean", "false", 1) != judge {
		t.Fatal("the judge's hold predicate differs from the sweeps' by more than the store-recovering term")
	}
	for name, expr := range map[string]string{"judge": judge, "sweep": sweep} {
		if !strings.Contains(expr, "AND NOT "+runnerReportsProxyDownSQL) {
			t.Errorf("the %s's hold predicate does not end on the runner's report of its proxy down", name)
		}
	}
	for _, field := range []string{"'proxy_stopped_at'", "'containment_pending'"} {
		if !strings.Contains(runnerReportsProxyDownSQL, field) {
			t.Errorf("the runner report term does not read %s", field)
		}
	}
}
