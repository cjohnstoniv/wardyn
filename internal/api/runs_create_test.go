// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestCreateRun_ReservedTaskIsRejected is the W15-d regression: a client (here
// a plain member — the finding's own threat actor) must never be able to
// forge run.Task into a server-set discriminator by simply POSTing it. Before
// the fix, task="harness login" reached handleUploadSSOToken's own gate
// (run.Task == harnessLoginTask && run.Agent == awsSSOAgent — BOTH
// client-settable, neither backed by a second trusted-linkage field like the
// workspace-scoped tasks below have) and runIsUnrecordable's recording-
// suppression check, completely bypassing the operatorOnly gate on the real
// POST /setup/harness-login door. Parametrized over every entry in
// reservedRunTasks so this test breaks the moment the enumeration and the
// guard it backs drift apart. Runs as a MEMBER (never operator/admin) — the
// guard must reject regardless of caller identity, since none of these
// discriminators is ever legitimate client input on this door.
func TestCreateRun_ReservedTaskIsRejected(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.router = h.srv.routes()
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)

	for task := range reservedRunTasks {
		t.Run(task, func(t *testing.T) {
			w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", member,
				`{"agent":"claude-code","task":"`+task+`"}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("task %q: code = %d, want 400: %s", task, w.Code, w.Body.String())
			}
		})
	}
}

// TestCreateRun_LegitimateTasksStillPass proves the W15-d guard stays narrowly
// scoped to exactly the four reserved discriminators: ordinary free-text
// tasks (including the common empty-task case, and an exec-mode shell
// command) plus the OTHER, deliberately-NOT-reserved step-run task strings
// ("source scan" / "workspace scan" — see reservedRunTasks' doc on why only
// harnessLoginTask lacks a second trusted-linkage guard) must all still
// decode/validate cleanly. Calls decodeAndValidateCreateRun directly (rather
// than the full POST /runs handler) so this test needs no store/runner
// fixture — decodeAndValidateCreateRun is a pure request-shape check that
// never touches either.
func TestCreateRun_LegitimateTasksStillPass(t *testing.T) {
	h := newHarness(t)
	for _, task := range []string{"", "write a healthz endpoint and open a PR", "source scan", "workspace scan", "echo hi"} {
		t.Run(task, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
				strings.NewReader(`{"agent":"claude-code","task":"`+task+`"}`))
			w := httptest.NewRecorder()
			_, _, ok := h.srv.decodeAndValidateCreateRun(w, r)
			if !ok {
				t.Fatalf("task %q: decodeAndValidateCreateRun rejected a legitimate task (code %d): %s", task, w.Code, w.Body.String())
			}
		})
	}
}
