// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestPolicyPreviewPushesUnstoppable: the preview says pushes_unstoppable exactly when the run would carry
// unstoppablePushWarning: the git token broker is off and the chosen wall preset's capability list leaves
// out code_write. The preview's warning filter drops that sentence, so the console reads the field.
func TestPolicyPreviewPushesUnstoppable(t *testing.T) {
	srv, _ := wallsRunFixture(t)
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)
	for _, tc := range []struct {
		name       string
		brokerOff  bool
		wallPreset string
		inline     string
		want       bool
	}{
		{"broker off, an inline list without code_write", true, "", `{"github_capabilities":["code_read"]}`, true},
		{"broker on, the same list", false, "", `{"github_capabilities":["code_read"]}`, false},
		// Every push refused outranks it: deny_all refuses a push to a known forge host even with the brokers off.
		{"broker off, Read-only refuses every push", true, "read-only", "", false},
		{"broker off, Open names no list", true, "open", "", false},
		{"broker off, no preset", true, "", "", false},
	} {
		srv.cfg.DisableGitPATBroker = tc.brokerOff
		body := `{"agent":"claude-code","task":"t"`
		if tc.wallPreset != "" {
			body += `,"wall_preset":"` + tc.wallPreset + `"`
		}
		if tc.inline != "" {
			body += `,"inline_policy":` + tc.inline
		}
		w := doSSO(t, srv, http.MethodPost, previewDoor, member, body+`}`)
		var pv policyPreviewResponse
		if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s: preview %d %v: %s", tc.name, w.Code, err, w.Body)
		}
		if pv.PushesUnstoppable != tc.want {
			t.Errorf("%s: pushes_unstoppable = %v, want %v", tc.name, pv.PushesUnstoppable, tc.want)
		}
		for _, warning := range pv.Warnings {
			if warning == unstoppablePushWarning {
				t.Errorf("%s: the preview sent the run warning itself: %q", tc.name, warning)
			}
		}
	}
}
