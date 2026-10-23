// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
)

func TestWithFailedStepsLeavesTheCallersMapAlone(t *testing.T) {
	in := map[string]any{"runner_error": "k8s: teardown: waiting for pods to terminate", "identity_error": "boom", "note": "x"}
	out := lifecycle.WithFailedSteps(in)
	if _, mutated := in["failed_steps"]; mutated {
		t.Error("WithFailedSteps wrote into the caller's map, which decides the kill's outcome")
	}
	want := []map[string]string{
		{"step": "identity_revoke", "reason": "boom"},
		{"step": "sandbox_teardown", "reason": "k8s: teardown: waiting for pods to terminate"},
	}
	if !reflect.DeepEqual(out["failed_steps"], want) || out["runner_error"] == nil || out["note"] != "x" {
		t.Errorf("WithFailedSteps = %v; want the keys kept and failed_steps %v", out, want)
	}
}

// A PAT revoke still pending is not a failed step: the cascade treats a row with only that key as no failure.
func TestWithFailedStepsDoesNotCountAPendingPATRevoke(t *testing.T) {
	out := lifecycle.WithFailedSteps(map[string]any{"pat_revoke_pending": 2, "broker_error": "down"})
	want := []map[string]string{{"step": "broker_revoke", "reason": "down"}}
	if !reflect.DeepEqual(out["failed_steps"], want) || out["pat_revoke_pending"] != 2 {
		t.Errorf("WithFailedSteps = %v; want the pending count kept and failed_steps %v", out, want)
	}
	if only := lifecycle.WithFailedSteps(map[string]any{"pat_revoke_pending": "1"}); !reflect.DeepEqual(only["failed_steps"], []map[string]string{}) {
		t.Errorf("a pending-only row lists failed_steps %v, want none", only["failed_steps"])
	}
}
