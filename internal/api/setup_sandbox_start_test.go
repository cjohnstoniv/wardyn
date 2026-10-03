// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

func TestSandboxStartCheck(t *testing.T) {
	if _, ok := sandboxStartCheck(nil); ok {
		t.Fatal("a non-Kubernetes runner has no start-deadline row")
	}
	chk, ok := sandboxStartCheck(&SetupSandboxStart{StartTimeoutSeconds: 180, CapacityWaitSeconds: 900})
	if !ok || chk.ID != "sandbox_start" || chk.Status != "info" || chk.Label != "Sandbox start deadlines" {
		t.Fatalf("row = %+v", chk)
	}
	if want := "A sandbox has 3m to start. If no machine has room for it, it waits up to 15m, then fails."; chk.Detail != want {
		t.Errorf("detail = %q, want %q", chk.Detail, want)
	}
	chk, _ = sandboxStartCheck(&SetupSandboxStart{StartTimeoutSeconds: 180, CapacityWaitSeconds: 0})
	if want := "A sandbox has 3m to start. Capacity wait is off, so a sandbox no machine has room for fails when that deadline passes."; chk.Detail != want {
		t.Errorf("wait-off detail = %q, want %q", chk.Detail, want)
	}
	if got := formatStartDeadline(5400); got != "90m" {
		t.Errorf("formatStartDeadline(5400) = %q, want 90m", got)
	}
	if got := formatStartDeadline(90); got != "90s" {
		t.Errorf("formatStartDeadline(90) = %q, want 90s", got)
	}
}

// A member's redacted /setup/status keeps the deadlines: the run page's overdue bound follows them.
func TestRedactSetupStatusKeepsSandboxStart(t *testing.T) {
	st := SetupStatus{Runner: SetupRunner{Driver: "k8s", Kubernetes: true, SandboxStart: &SetupSandboxStart{StartTimeoutSeconds: 60}}}
	got := redactSetupStatusForUser(st).Runner
	if got.Driver != "" || got.SandboxStart == nil || got.SandboxStart.StartTimeoutSeconds != 60 {
		t.Fatalf("redacted runner = %+v", got)
	}
}
