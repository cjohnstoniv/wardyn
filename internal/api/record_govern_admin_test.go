// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Constrained-admin mode and Record Mode (WARDYN_GOVERN_ADMIN_RUNS): the lane
// builds its own allow-all, credentialed spec and skips the member clamp, so a
// governed admin is refused by name unless the deployment exempts recording.

// recordGovernStore is recordLLMModeStore, which persists runs so a record launch
// completes, plus the workspace read the handler makes first.
type recordGovernStore struct{ *recordLLMModeStore }

func (s recordGovernStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}

type recordGovernFixture struct {
	srv   *Server
	st    *recordLLMModeStore
	audit *recRecorder
	wsID  uuid.UUID
}

func newRecordGovernFixture(t *testing.T, on bool, exempt ...string) *recordGovernFixture {
	t.Helper()
	wsID := uuid.New()
	st := newRecordLLMModeStore(types.Workspace{
		ID: wsID, Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
	})
	h := newHarness(t)
	cfg := baseTestConfig(h, recordGovernStore{st})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	cfg.GovernAdminRuns = on
	cfg.GovernAdminRunsExempt = exempt
	return &recordGovernFixture{srv: New(cfg), st: st, audit: h.audit, wsID: wsID}
}

func (f *recordGovernFixture) rows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// recordDirect posts the record request straight to the handler under ctx, for
// the callers (an admin-role personal token) the cookie helpers cannot express.
func (f *recordGovernFixture) recordDirect(ctx context.Context) *httptest.ResponseRecorder {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", f.wsID.String())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+f.wsID.String()+"/record", strings.NewReader(`{"name":"build & test"}`))
	r = r.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.srv.handleRecordGoverned(w, r)
	return w
}

func (f *recordGovernFixture) recordSSO(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	cookie := memberModeSSOSession(t, governAdminSub, "owner@corp.example", oidc.RoleAdmin, false)
	return doSSO(t, f.srv, http.MethodPost, "/api/v1/workspaces/"+f.wsID.String()+"/record", cookie, `{"name":"build & test"}`)
}

func (f *recordGovernFixture) recordAdminToken(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodPost, "/api/v1/workspaces/"+f.wsID.String()+"/record", adminToken, `{"name":"build & test"}`)
}

// startDatum returns the data of the one run.record.start success row.
func (f *recordGovernFixture) startDatum(t *testing.T) map[string]any {
	t.Helper()
	rows := f.rows("run.record.start")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("run.record.start rows = %+v, want exactly one success row", rows)
	}
	var d map[string]any
	if err := json.Unmarshal(rows[0].Data, &d); err != nil {
		t.Fatalf("decode run.record.start data: %v", err)
	}
	return d
}

func TestRecordWorkspace_GovernedAdminRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(t *testing.T, f *recordGovernFixture) *httptest.ResponseRecorder
	}{
		{"an SSO admin", func(t *testing.T, f *recordGovernFixture) *httptest.ResponseRecorder { return f.recordSSO(t) }},
		{"an admin-role personal token", func(t *testing.T, f *recordGovernFixture) *httptest.ResponseRecorder {
			return f.recordDirect(governAdminTokenCtx(t))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecordGovernFixture(t, true)
			w := tc.do(t, f)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
			}
			var body struct{ Error, Reason string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Reason != "recording_governed" || body.Error != recordingGovernedRefusal {
				t.Errorf("body = %+v, want reason recording_governed and the M10 sentence", body)
			}
			denied := f.rows("authz.denied")
			if len(denied) != 1 || denied[0].Target != "workspaces.record" || !strings.Contains(string(denied[0].Data), `"recording_governed"`) {
				t.Errorf("authz.denied rows = %+v, want one at target workspaces.record carrying recording_governed", denied)
			}
			// Refused before the ceiling read and the import-step claim: the store
			// stub would panic on a profile read, and nothing may be launched.
			if len(f.st.runs) != 0 || len(f.rows("run.record.start")) != 0 {
				t.Errorf("a refused record launched a run or wrote run.record.start (runs=%d)", len(f.st.runs))
			}
		})
	}
}

func TestRecordWorkspace_ExemptAndBreakGlassLaunchMarked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exempt []string
		do     func(t *testing.T, f *recordGovernFixture) *httptest.ResponseRecorder
	}{
		{"an SSO admin under the recording exemption", []string{"recording"},
			func(t *testing.T, f *recordGovernFixture) *httptest.ResponseRecorder { return f.recordSSO(t) }},
		{"an admin-role personal token under the recording exemption", []string{"recording"},
			func(t *testing.T, f *recordGovernFixture) *httptest.ResponseRecorder {
				return f.recordDirect(governAdminTokenCtx(t))
			}},
		{"the admin token", nil,
			func(t *testing.T, f *recordGovernFixture) *httptest.ResponseRecorder { return f.recordAdminToken(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecordGovernFixture(t, true, tc.exempt...)
			if w := tc.do(t, f); w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
			}
			if d := f.startDatum(t); d["governance_exempt"] != true {
				t.Errorf("run.record.start data = %v, want governance_exempt true", d)
			}
		})
	}
}

// With the switch off nothing changes: an admin launches as before and no row
// carries the marker. The exemption names a lane of the switch, so alone it
// does nothing either.
func TestRecordWorkspace_SwitchOffUnchanged(t *testing.T) {
	for _, exempt := range [][]string{nil, {"recording"}} {
		f := newRecordGovernFixture(t, false, exempt...)
		if w := f.recordSSO(t); w.Code != http.StatusAccepted {
			t.Fatalf("exempt=%v: status = %d, want 202; body=%s", exempt, w.Code, w.Body.String())
		}
		if d := f.startDatum(t); d["governance_exempt"] != nil {
			t.Errorf("exempt=%v: run.record.start data = %v, want no governance_exempt key", exempt, d)
		}
	}
}

func TestGovernAdminRunsCheck(t *testing.T) {
	if _, ok := governAdminRunsCheck(false, false, true); ok {
		t.Error("the row shows with the switch off")
	}
	// SETUP_CHECK.GOVERN_ADMIN_RUNS (mock M10), byte for byte.
	on, _ := governAdminRunsCheck(true, false, true)
	if on.ID != "govern_admin_runs" || on.Label != "Admin runs" || on.Status != "info" || on.Fix != "" ||
		on.Detail != "Admins' own runs are governed: each is bounded by the governance profile and grants that apply to that person. Record Mode is refused for them. The admin token stays outside, as break-glass, and its runs are marked `governance_exempt` in the audit trail." {
		t.Errorf("ON = %+v", on)
	}
	exempt, _ := governAdminRunsCheck(true, true, true)
	if exempt.Status != "info" ||
		exempt.Detail != "Admins' own runs are governed: each is bounded by the governance profile and grants that apply to that person. Record Mode is exempt; each recording is marked `governance_exempt` in the audit trail. The admin token stays outside, as break-glass, and its runs are marked `governance_exempt` in the audit trail." {
		t.Errorf("ON_EXEMPT = %+v", exempt)
	}
	noPerson, _ := governAdminRunsCheck(true, false, false)
	if noPerson.Status != "warn" || noPerson.Blocking ||
		noPerson.Detail != "`WARDYN_GOVERN_ADMIN_RUNS` is on, but nobody signs in to this deployment, so it governs no one: every run is launched as the admin and marked `governance_exempt`." ||
		noPerson.Fix != "Configure single sign-on, or unset `WARDYN_GOVERN_ADMIN_RUNS`." {
		t.Errorf("NO_PERSON = %+v", noPerson)
	}
}

func TestSetupStatusReportsGovernAdminRuns(t *testing.T) {
	cfg := baseTestConfig(newHarness(t), &setupStatusCostStore{})
	cfg.GovernAdminRuns = true
	cfg.GovernAdminRunsExempt = []string{"recording"}
	w := do(t, New(cfg), http.MethodGet, "/api/v1/setup/status", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var got SetupStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Auth.GovernAdminRuns || len(got.Auth.GovernAdminRunsExempt) != 1 || got.Auth.GovernAdminRunsExempt[0] != "recording" {
		t.Errorf("auth = %+v, want both fields", got.Auth)
	}
	// Kept through redaction: every signed-in person can read the posture their
	// own runs are under.
	if red := redactSetupStatusForUser(got); !reflect.DeepEqual(red.Auth, got.Auth) {
		t.Errorf("redacted auth = %+v, want the posture kept", red.Auth)
	}
}
