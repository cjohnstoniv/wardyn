// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"fmt"
	"maps"
	"slices"
)

// failedStepNames names the cascade step behind each per-step failure key a run.revoke row carries.
// pat_revoke_pending is absent on purpose: a PAT revoke still pending is not a failure of the cascade.
var failedStepNames = map[string]string{
	"runner_error":           "sandbox_teardown",
	"identity_error":         "identity_revoke",
	"broker_error":           "broker_revoke",
	"proxy_config_error":     "proxy_config_drop",
	"pat_revoke_check_error": "pat_revoke_check",
	"approval_cancel_error":  "approval_cancel",
}

// WithFailedSteps is data plus a failed_steps list, one {step, reason} per failure key in key order,
// so a run.revoke row names each failing step and its reason without the reader knowing the key
// vocabulary. A copy: the caller's map still decides the outcome. Additive; the keys stay.
func WithFailedSteps(data map[string]any) map[string]any {
	out := maps.Clone(data)
	steps := []map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(data)) {
		if step, ok := failedStepNames[k]; ok {
			steps = append(steps, map[string]string{"step": step, "reason": fmt.Sprint(data[k])})
		}
	}
	out["failed_steps"] = steps
	return out
}
