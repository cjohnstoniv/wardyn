// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// TestMetricsLockHoldsOverLimit: /metrics carries the count of holds of a run's lock that ran past their limit, as
// the db package counts them.
func TestMetricsLockHoldsOverLimit(t *testing.T) {
	h := newHarness(t)
	w := do(t, New(baseTestConfig(h, nil)), http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"# TYPE wardyn_lock_holds_over_limit_total counter\n",
		fmt.Sprintf("wardyn_lock_holds_over_limit_total %d\n", db.LongLockHolds()),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q:\n%s", want, body)
		}
	}
}
