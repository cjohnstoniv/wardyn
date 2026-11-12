// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestStepHostRulesVMRefusesAVMRun(t *testing.T) {
	rules := []types.HostRuleSet{{Rules: []types.HostRule{{Host: "api.example.com", Effect: types.HostRuleAllow}}}}
	for _, tc := range []struct {
		kind   string
		rules  []types.HostRuleSet
		refuse bool
	}{
		{"linux_vm", rules, true},
		{"macos_vm", rules, true},
		{"container", rules, false},
		{"", rules, false},
		{"linux_vm", nil, false},
	} {
		req := createRunRequest{MachineKind: tc.kind}
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, spec: types.RunPolicySpec{HostRules: tc.rules}}
		ok := capsServer(&fakeRunner{}).stepHostRulesVM(f, nil)
		if ok == tc.refuse {
			t.Errorf("%q with %d sets: ok = %v, want refused = %v (%d %s)", tc.kind, len(tc.rules), ok, tc.refuse, w.Code, w.Body)
		}
		if tc.refuse && (w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"`+reasonHostRulesNeedInspectedVM+`"`)) {
			t.Errorf("%q: %d %s, want 422 %s", tc.kind, w.Code, w.Body, reasonHostRulesNeedInspectedVM)
		}
	}
}
