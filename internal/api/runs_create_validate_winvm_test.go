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

// windows_vm is admitted by the request check whatever WARDYN_MACOS_VMS says, and needs no agent; the fold
// refuses it while features.windows_vm is off.
func TestMachineKindRefusalWindowsUntilRegistered(t *testing.T) {
	for _, env := range []string{"", "1"} {
		t.Setenv(envMacOSVMs, env)
		if got := machineKindRefusal(types.MachineWindowsVM); got != "" {
			t.Errorf("WARDYN_MACOS_VMS=%q: refusal %q, want windows_vm admitted", env, got)
		}
		h := newHarness(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"machine_kind":"windows_vm","windows_image":"Server"}`))
		// Later checks may still answer this bare request; neither the kind nor a missing agent is what refuses it.
		if _, _, _, _, ok := h.srv.decodeAndValidateCreateRun(rec, req); !ok {
			if got := errorReasonOf(rec.Body.String()); got == reasonMachineKindUnknown || got == reasonAgentRequired {
				t.Errorf("WARDYN_MACOS_VMS=%q: create answered %d %s, want windows_vm admitted with no agent", env, rec.Code, rec.Body.String())
			}
		}
	}
	srv, _, _ := winvmServer(t, false)
	_, rf := srv.windowsImageDecision(winvmHTTP(memberContext()), winvmReq(), types.RunPolicySpec{})
	wantWinRefusal(t, "switch off", rf, http.StatusForbidden, reasonWindowsVMsOff, "")
}
