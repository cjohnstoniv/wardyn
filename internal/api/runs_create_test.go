// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
			_, _, _, ok := h.srv.decodeAndValidateCreateRun(w, r)
			if !ok {
				t.Fatalf("task %q: decodeAndValidateCreateRun rejected a legitimate task (code %d): %s", task, w.Code, w.Body.String())
			}
		})
	}
}

// TestDecodeAndValidateCreateRun_NoTaskCoercesInteractive is the W15-S1-2
// regression: a non-interactive request with no task used to sail through
// decodeAndValidateCreateRun untouched, dispatching a sandbox that execs
// nothing and never reaches a terminal state. The shared chokepoint now
// coerces it to interactive (idle, attachable, reapable) and surfaces why.
func TestDecodeAndValidateCreateRun_NoTaskCoercesInteractive(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
		strings.NewReader(`{"agent":"claude-code","repo":"acme/widgets"}`))
	w := httptest.NewRecorder()

	req, _, warning, ok := h.srv.decodeAndValidateCreateRun(w, r)
	if !ok {
		t.Fatalf("decode: unexpected failure, code=%d body=%s", w.Code, w.Body.String())
	}
	if !req.Interactive {
		t.Error("no task + non-interactive must be coerced to Interactive, not launched as a silent no-op run")
	}
	if warning == "" {
		t.Error("expected an advisory warning explaining the coercion, got none")
	}
}

// TestDecodeAndValidateCreateRun_TaskPresentStaysNonInteractive guards the
// negative: a request that supplies a task is untouched (no coercion, no
// warning) — the guard is conditional, not a blanket override.
func TestDecodeAndValidateCreateRun_TaskPresentStaysNonInteractive(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
		strings.NewReader(`{"agent":"claude-code","repo":"acme/widgets","task":"do the thing"}`))
	w := httptest.NewRecorder()

	req, _, warning, ok := h.srv.decodeAndValidateCreateRun(w, r)
	if !ok {
		t.Fatalf("decode: unexpected failure, code=%d body=%s", w.Code, w.Body.String())
	}
	if req.Interactive {
		t.Error("a request with a task must not be coerced to interactive")
	}
	if warning != "" {
		t.Errorf("expected no warning when a task is present, got %q", warning)
	}
}

// TestDecodeAndValidateCreateRun_ExplicitInteractiveNoWarning guards the
// other negative: an operator who explicitly asked for --interactive gets no
// spurious coercion warning (it was already interactive on purpose).
func TestDecodeAndValidateCreateRun_ExplicitInteractiveNoWarning(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
		strings.NewReader(`{"agent":"claude-code","repo":"acme/widgets","interactive":true}`))
	w := httptest.NewRecorder()

	req, _, warning, ok := h.srv.decodeAndValidateCreateRun(w, r)
	if !ok {
		t.Fatalf("decode: unexpected failure, code=%d body=%s", w.Code, w.Body.String())
	}
	if !req.Interactive {
		t.Error("explicit interactive:true must stay interactive")
	}
	if warning != "" {
		t.Errorf("expected no warning for an explicitly interactive request, got %q", warning)
	}
}

// TestCreateRun_TitleAndDescriptionRoundTrip pins the new display fields all
// the way through: request DTO -> handler -> the six agent_runs column lists ->
// the response. A column-order slip in any of those lists surfaces here as a
// scan error or a swapped value rather than as a mystery in the console.
func TestCreateRun_TitleAndDescriptionRoundTrip(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"fix it","title":"  Refund flow  ","description":"ticket 4412"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v; body=%s", err, w.Body.String())
	}
	// Trimmed at the handler: a title is a grouping key, and " x" grouping
	// separately from "x" is a bug nobody would ever diagnose.
	if created.Title != "Refund flow" {
		t.Errorf("Title = %q, want %q (trimmed)", created.Title, "Refund flow")
	}
	if created.Description != "ticket 4412" {
		t.Errorf("Description = %q, want %q", created.Description, "ticket 4412")
	}

	// And again off the READ path, which is a different column list.
	w = do(t, srv, http.MethodGet, "/api/v1/runs/"+created.ID.String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.Title != "Refund flow" || got.Description != "ticket 4412" {
		t.Errorf("GetRun returned title=%q description=%q, want %q / %q",
			got.Title, got.Description, "Refund flow", "ticket 4412")
	}
}

// TestCreateRun_NoTitleStillPasses pins a DECISION, not a behavior: the console
// requires a title, but the server deliberately does not. The site-config
// probe, harness login and workspace record/verify all create runs with no
// human to name them, so turning this into a 400 would break every one of them
// — and the failure would show up far from the change that caused it.
func TestCreateRun_NoTitleStillPasses(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
		strings.NewReader(`{"agent":"claude-code","task":"echo hi"}`))
	w := httptest.NewRecorder()
	if _, _, _, ok := h.srv.decodeAndValidateCreateRun(w, r); !ok {
		t.Fatalf("a run with no title was rejected (code %d): %s", w.Code, w.Body.String())
	}
}

// TestCreateRun_OverlongTitleIs400 covers the other side: these are free-text
// fields bound for a TEXT column and for every row of the console's run list,
// so they are capped at the door.
func TestCreateRun_OverlongTitleIs400(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]string{
		"title":       `{"agent":"claude-code","title":"` + strings.Repeat("t", maxRunTitleLen+1) + `"}`,
		"description": `{"agent":"claude-code","description":"` + strings.Repeat("d", maxRunDescriptionLen+1) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
			w := httptest.NewRecorder()
			if _, _, _, ok := h.srv.decodeAndValidateCreateRun(w, r); ok {
				t.Fatalf("an over-long %s was accepted", name)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
}
