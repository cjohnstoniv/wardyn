// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// windows_vm is a valid kind, but no create accepts it yet, whatever WARDYN_MACOS_VMS says: with the macOS
// switch on, the kind's own Validate would pass it.
func TestMachineKindRefusalWindowsUntilRegistered(t *testing.T) {
	const want = "Windows VM sandboxes are not available yet."
	if err := types.MachineWindowsVM.Validate(); err != nil {
		t.Fatalf("windows_vm is not a kind: %v", err)
	}
	for _, env := range []string{"", "1"} {
		t.Setenv(envMacOSVMs, env)
		if got := machineKindRefusal(types.MachineWindowsVM); got != want {
			t.Errorf("WARDYN_MACOS_VMS=%q: refusal %q, want %q", env, got, want)
		}
		h := newHarness(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"agent":"claude-code","machine_kind":"windows_vm"}`))
		if _, _, _, _, ok := h.srv.decodeAndValidateCreateRun(rec, req); ok || rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("WARDYN_MACOS_VMS=%q: create answered %d %s, want 400 and the refusal", env, rec.Code, rec.Body.String())
		}
	}
}
