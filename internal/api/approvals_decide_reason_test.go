// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDecideReason_BoundAtTheWrite pins the decision reason's bound at the body
// decode: a reason of exactly the limit decides, one character over is a 400 that
// leaves the approval PENDING with nothing audited, and the bound counts
// characters, not bytes. The empty body, an unknown field and a member deciding
// an egress approval on a run they own keep working. A body past the ceiling is
// a 413.
func TestDecideReason_BoundAtTheWrite(t *testing.T) {
	f := newScopeFixture(t)
	admin := ssoSession(t, "sub-admin-reason", "admin@corp.example", oidc.RoleAdmin)
	member := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleUser)

	reasonBody := func(reason string, extra map[string]any) string {
		m := map[string]any{"reason": reason}
		for k, v := range extra {
			m[k] = v
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	atLimit := strings.Repeat("é", maxGovernanceRejectReasonLen) // 1024 bytes, 512 characters
	overLimit := strings.Repeat("a", maxGovernanceRejectReasonLen+1)

	cases := []struct {
		name       string
		who        *http.Cookie
		body       string
		want       int
		wantReason string
		wantStored string
	}{
		{"a reason of exactly the limit decides", admin, reasonBody(atLimit, nil), http.StatusOK, "", atLimit},
		{"a reason one character over is refused", admin, reasonBody(overLimit, nil), http.StatusBadRequest, reasonInvalidRequestBody, ""},
		{"a member's over-limit reason on an owned run is refused", member, reasonBody(overLimit, nil), http.StatusBadRequest, reasonInvalidRequestBody, ""},
		{"a member decides an owned run's egress approval", member, reasonBody("mine", nil), http.StatusOK, "", "mine"},
		{"the empty body still decides", admin, "", http.StatusOK, "", ""},
		{"an unknown field is still accepted", admin, reasonBody("ok", map[string]any{"from_a_newer_client": true}), http.StatusOK, "", "ok"},
		{"a body past the ceiling is refused", admin, reasonBody("x", map[string]any{"pad": strings.Repeat("p", 16<<10)}), http.StatusRequestEntityTooLarge, reasonRequestBodyTooLarge, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := f.seedEgress(t, "registry.npmjs.org")
			audited := len(f.audit())
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/deny", tc.who, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
			f.approval.mu.Lock()
			got := f.approval.byID[id]
			f.approval.mu.Unlock()
			if tc.want != http.StatusOK {
				if r := errorReason(w); r != tc.wantReason {
					t.Errorf("reason = %q, want %q", r, tc.wantReason)
				}
				if got.State != types.ApprovalPending || got.Reason != "" {
					t.Fatalf("refused decision changed the row: state=%q reason length=%d", got.State, len(got.Reason))
				}
				if n := len(f.audit()); n != audited {
					t.Fatalf("refused decision audited %d events", n-audited)
				}
				return
			}
			if got.State != types.ApprovalDenied || got.Reason != tc.wantStored {
				t.Fatalf("state=%q stored reason length=%d, want DENIED with length %d", got.State, len(got.Reason), len(tc.wantStored))
			}
		})
	}
}
