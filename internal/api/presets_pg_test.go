// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Launch presets (#1143) on real Postgres: the store's upsert/version
// statement and the agent_runs preset columns are what these pin.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

type presetFixture struct {
	srv    *Server
	pg     store.PG
	audit  *recRecorder
	member *http.Cookie
}

// newPresetFixture wires a PG-backed server with SSO (so a member is a real
// session) and a runner, and assigns every `standard` user a governance
// profile whose ceiling allows pypi.org only and refuses task_mode=exec.
func newPresetFixture(t *testing.T) presetFixture {
	t.Helper()
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	profile, err := pg.UpsertGovernanceProfile(ctx, types.GovernanceProfile{
		Name:      "walled",
		Ceiling:   types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2},
		Limits:    types.GovernanceLimits{DenyTaskModeExec: true},
		CreatedBy: "seed",
	})
	if err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if _, err := pg.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectUserType, Subject: types.UserTypeStandard, ProfileID: profile.ID,
	}); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}
	h := newHarness(t)
	audit := &recRecorder{}
	cfg := baseTestConfig(h, pg)
	cfg.Audit = audit
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "pypi.org"}, MinConfinementClass: types.CC2}
	return presetFixture{
		srv: New(cfg), pg: pg, audit: audit,
		member: ssoSession(t, "sub-preset-member", "member@corp.example", oidc.RoleUser),
	}
}

func (f presetFixture) putPreset(t *testing.T, name string, req client.PresetRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	return do(t, f.srv, http.MethodPut, "/api/v1/presets/"+name, adminToken, string(body))
}

func (f presetFixture) launch(t *testing.T, req client.CreateRunRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	return doSSO(t, f.srv, http.MethodPost, "/api/v1/runs", f.member, string(body))
}

func (f presetFixture) actions(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// presetBundle is the explicit request a preset stands for in these tests: an
// inline policy wider than the member's ceiling (evil.example is clamped away)
// with a UI app, so the comparison covers the ceiling's narrowing too.
func presetBundle() client.CreateRunRequest {
	return client.CreateRunRequest{
		Agent: "claude-code",
		Repo:  "acme/widgets",
		InlinePolicy: &types.RunPolicySpec{
			AllowedDomains:      []string{"pypi.org", "evil.example"},
			MinConfinementClass: types.CC2,
			UIApps:              []types.UIApp{{Name: "editor", Port: 8080}},
		},
	}
}

// dispatched waits for a created run's dispatch to finish (RUNNING), so its
// writes land before the throwaway database is dropped, and returns its
// run.policy.resolve envelope: the policy the sandbox was actually given.
func (f presetFixture) dispatched(t *testing.T, id uuid.UUID) *types.AuditEvent {
	t.Helper()
	ev := waitForRecAudit(t, f.audit, id, "run.policy.resolve", "success")
	waitForRunState(t, f.srv, id, types.RunRunning)
	return ev
}

func mustCreated(t *testing.T, w *httptest.ResponseRecorder) client.CreateRunResult {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	var out client.CreateRunResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return out
}

// TestPG_PresetLaunchEqualsExplicitSpec is the issue's done-when: a member
// launching a preset gets the same run, the same 201 warnings and the same
// dispatched policy as the same member sending the preset's request
// explicitly — the ceiling narrows both identically. Only the identity
// fields and the preset stamp differ.
func TestPG_PresetLaunchEqualsExplicitSpec(t *testing.T) {
	f := newPresetFixture(t)
	if w := f.putPreset(t, "widgets", client.PresetRequest{Request: presetBundle()}); w.Code != http.StatusCreated {
		t.Fatalf("put preset = %d: %s", w.Code, w.Body.String())
	}

	explicit := presetBundle()
	explicit.Title, explicit.Task = "nightly", "run the tests"
	viaSpec := mustCreated(t, f.launch(t, explicit))
	viaPreset := mustCreated(t, f.launch(t, client.CreateRunRequest{Preset: "widgets", Title: "nightly", Task: "run the tests"}))

	if viaPreset.Preset != "widgets" || viaPreset.PresetVersion != 1 {
		t.Errorf("preset run stamp = (%q, %d), want (widgets, 1)", viaPreset.Preset, viaPreset.PresetVersion)
	}
	if viaSpec.Preset != "" || viaSpec.PresetVersion != 0 {
		t.Errorf("explicit run carries a preset stamp (%q, %d)", viaSpec.Preset, viaSpec.PresetVersion)
	}
	normalize := func(r client.CreateRunResult) client.CreateRunResult {
		r.ID, r.SPIFFEID, r.Preset, r.PresetVersion = uuid.Nil, "", "", 0
		r.CreatedAt, r.UpdatedAt = viaSpec.CreatedAt, viaSpec.UpdatedAt
		return r
	}
	a, b := normalize(viaSpec), normalize(viaPreset)
	if !reflect.DeepEqual(a.AgentRun, b.AgentRun) {
		t.Errorf("created run differs:\nexplicit %+v\npreset   %+v", a.AgentRun, b.AgentRun)
	}
	if !reflect.DeepEqual(a.Warnings, b.Warnings) {
		t.Errorf("201 warnings differ:\nexplicit %q\npreset   %q", a.Warnings, b.Warnings)
	}

	specEnv, presetEnv := f.dispatched(t, viaSpec.ID), f.dispatched(t, viaPreset.ID)
	if string(specEnv.Data) != string(presetEnv.Data) {
		t.Errorf("dispatched policy differs:\nexplicit %s\npreset   %s", specEnv.Data, presetEnv.Data)
	}
	var dispatched types.RunPolicySpec
	if err := json.Unmarshal(presetEnv.Data, &dispatched); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if slices.Contains(dispatched.AllowedDomains, "evil.example") || !slices.Contains(dispatched.AllowedDomains, "pypi.org") {
		t.Errorf("dispatched egress %v, want pypi.org and not the ceiling-exceeding evil.example", dispatched.AllowedDomains)
	}

	// The stamp is a column, not a response decoration: it reads back.
	got := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+viaPreset.ID.String(), adminToken, "")
	var run types.AgentRun
	if err := json.Unmarshal(got.Body.Bytes(), &run); err != nil || run.Preset != "widgets" || run.PresetVersion != 1 {
		t.Errorf("GET /runs/{id} = %d %s, want preset widgets v1", got.Code, got.Body.String())
	}
}

// TestPG_PresetExceedingCeilingRefusedLikeExplicit: a preset carrying a field
// the member's governance profile refuses (task_mode=exec) is refused with
// the byte-identical 403 the explicit request gets, and creates no run.
func TestPG_PresetExceedingCeilingRefusedLikeExplicit(t *testing.T) {
	f := newPresetFixture(t)
	bundle := client.CreateRunRequest{Agent: "claude-code", TaskMode: "exec"}
	if w := f.putPreset(t, "exec-job", client.PresetRequest{Request: bundle}); w.Code != http.StatusCreated {
		t.Fatalf("put preset = %d: %s", w.Code, w.Body.String())
	}
	explicit := bundle
	explicit.Task = "make test"
	wSpec := f.launch(t, explicit)
	wPreset := f.launch(t, client.CreateRunRequest{Preset: "exec-job", Task: "make test"})
	if wSpec.Code != http.StatusForbidden {
		t.Fatalf("explicit exec launch = %d, want 403: %s", wSpec.Code, wSpec.Body.String())
	}
	if wPreset.Code != wSpec.Code || wPreset.Body.String() != wSpec.Body.String() {
		t.Errorf("preset launch = %d %s\nwant the explicit refusal %d %s", wPreset.Code, wPreset.Body.String(), wSpec.Code, wSpec.Body.String())
	}
	runs, err := f.pg.ListRuns(context.Background())
	if err != nil || len(runs) != 0 {
		t.Errorf("a refused launch left %d run(s) (err %v)", len(runs), err)
	}
	// The admin is not bound by the member's profile, so the same preset
	// launches for them: the refusal above is the caller's ceiling, not the preset.
	w := do(t, f.srv, http.MethodPost, "/api/v1/runs", adminToken, `{"preset":"exec-job","task":"make test"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("admin preset launch = %d, want 201: %s", w.Code, w.Body.String())
	}
	f.dispatched(t, mustCreated(t, w).ID)
}

// TestPG_PresetRefusesNonPerLaunchFields: alongside a preset only title, task
// and preset_version may be set; anything else is refused by name, before
// the preset is even read, and preset_version alone is refused too.
func TestPG_PresetRefusesNonPerLaunchFields(t *testing.T) {
	f := newPresetFixture(t)
	if w := f.putPreset(t, "widgets", client.PresetRequest{Request: presetBundle()}); w.Code != http.StatusCreated {
		t.Fatalf("put preset = %d: %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		name, body, reason string
	}{
		{"image", `{"preset":"widgets","task":"x","image":"ghcr.io/evil/agent:1"}`, reasonPresetField},
		{"agent", `{"preset":"widgets","agent":"codex-cli"}`, reasonPresetField},
		{"inline_policy", `{"preset":"widgets","inline_policy":{"allowed_domains":["evil.example"]}}`, reasonPresetField},
		{"version without preset", `{"agent":"claude-code","preset_version":2}`, reasonPresetVersionNoName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs", f.member, tc.body)
			var got errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if w.Code != http.StatusBadRequest || got.Reason != tc.reason {
				t.Errorf("= %d %s, want 400 reason %s", w.Code, w.Body.String(), tc.reason)
			}
		})
	}
	runs, err := f.pg.ListRuns(context.Background())
	if err != nil || len(runs) != 0 {
		t.Errorf("a refused launch left %d run(s) (err %v)", len(runs), err)
	}
}

// TestPG_PresetUnknownAndClosedAnswerAlike: an unknown preset, and one not
// open to the caller's user type, answer the same 422 with a named reason; the
// closed one is also left out of the member's list and read.
func TestPG_PresetUnknownAndClosedAnswerAlike(t *testing.T) {
	f := newPresetFixture(t)
	if _, err := f.pg.CreateUserType(context.Background(), types.UserType{ID: "contractor", Name: "Contractor"}); err != nil {
		t.Fatalf("seed user type: %v", err)
	}
	if w := f.putPreset(t, "contractors-only", client.PresetRequest{UserTypes: []string{"contractor"}, Request: presetBundle()}); w.Code != http.StatusCreated {
		t.Fatalf("put preset = %d: %s", w.Code, w.Body.String())
	}
	for _, name := range []string{"no-such-preset", "contractors-only"} {
		w := f.launch(t, client.CreateRunRequest{Preset: name, Task: "x"})
		var got errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		want := `preset "` + name + `" does not exist or is not open to you`
		if w.Code != http.StatusUnprocessableEntity || got.Reason != reasonPresetUnknown || got.Error != want {
			t.Errorf("launch %q = %d %s, want 422 %s %q", name, w.Code, w.Body.String(), reasonPresetUnknown, want)
		}
	}
	var doc client.PresetsDocument
	w := doSSO(t, f.srv, http.MethodGet, "/api/v1/presets", f.member, "")
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Code != http.StatusOK || len(doc.Presets) != 0 {
		t.Errorf("member list = %d %s, want 200 with no presets", w.Code, w.Body.String())
	}
	if w := doSSO(t, f.srv, http.MethodGet, "/api/v1/presets/contractors-only", f.member, ""); w.Code != http.StatusNotFound {
		t.Errorf("member read of a closed preset = %d, want 404", w.Code)
	}
	w = do(t, f.srv, http.MethodGet, "/api/v1/presets", adminToken, "")
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.Presets) != 1 || doc.Presets[0].Name != "contractors-only" {
		t.Errorf("admin list = %d %s, want the one preset", w.Code, w.Body.String())
	}
}

// TestPG_PresetVersionRecordedAndAudited: every write that lands is audited
// and moves the version; an identical write moves nothing and writes no row;
// a run records the version it expanded, and a pinned stale version is
// refused.
func TestPG_PresetVersionRecordedAndAudited(t *testing.T) {
	f := newPresetFixture(t)
	req := client.PresetRequest{Description: "widgets CI", Request: presetBundle()}
	if w := f.putPreset(t, "widgets", req); w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	w := f.putPreset(t, "widgets", req)
	var p client.Preset
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != http.StatusOK || p.Version != 1 {
		t.Fatalf("identical put = %d %s, want 200 at version 1", w.Code, w.Body.String())
	}
	req.Request.Repo = "acme/gadgets"
	w = f.putPreset(t, "widgets", req)
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != http.StatusOK || p.Version != 2 {
		t.Fatalf("changed put = %d %s, want 200 at version 2", w.Code, w.Body.String())
	}

	creates, updates := f.actions("preset.create"), f.actions("preset.update")
	if len(creates) != 1 || len(updates) != 1 {
		t.Fatalf("audit rows: %d preset.create, %d preset.update; want 1 and 1 (an identical put writes none)", len(creates), len(updates))
	}
	var data struct {
		Version int                     `json:"version"`
		Request client.CreateRunRequest `json:"request"`
	}
	if err := json.Unmarshal(updates[0].Data, &data); err != nil || updates[0].Target != "widgets" ||
		data.Version != 2 || data.Request.Repo != "acme/gadgets" || updates[0].Actor == "" {
		t.Errorf("preset.update row = %+v (%s), want target widgets, version 2 and the stored request", updates[0], updates[0].Data)
	}

	run := mustCreated(t, f.launch(t, client.CreateRunRequest{Preset: "widgets", Task: "x"}))
	if run.Preset != "widgets" || run.PresetVersion != 2 || run.Repo != "acme/gadgets" {
		t.Errorf("run = (%q v%d repo %q), want widgets v2 on acme/gadgets", run.Preset, run.PresetVersion, run.Repo)
	}
	var runRow struct {
		Preset        string `json:"preset"`
		PresetVersion int    `json:"preset_version"`
	}
	for _, e := range f.actions("run.create") {
		if e.Target == run.ID.String() {
			_ = json.Unmarshal(e.Data, &runRow)
		}
	}
	if runRow.Preset != "widgets" || runRow.PresetVersion != 2 {
		t.Errorf("run.create audit = %+v, want preset widgets v2 (the chained row outlives the run row)", runRow)
	}
	f.dispatched(t, run.ID)
	stored, err := f.pg.GetRun(context.Background(), run.ID)
	if err != nil || stored.PresetVersion != 2 {
		t.Errorf("stored run preset_version = %d (err %v), want 2", stored.PresetVersion, err)
	}

	w = f.launch(t, client.CreateRunRequest{Preset: "widgets", PresetVersion: 1, Task: "x"})
	var got errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusConflict || got.Reason != reasonPresetVersionMoved {
		t.Errorf("pinned stale version = %d %s, want 409 %s", w.Code, w.Body.String(), reasonPresetVersionMoved)
	}

	if w := do(t, f.srv, http.MethodDelete, "/api/v1/presets/widgets", adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if dels := f.actions("preset.delete"); len(dels) != 1 || dels[0].Target != "widgets" {
		t.Errorf("preset.delete rows = %+v, want one naming widgets", dels)
	}
	if stored, _ := f.pg.GetRun(context.Background(), run.ID); stored.Preset != "widgets" {
		t.Errorf("deleting the preset cleared the run's stamp: %q", stored.Preset)
	}
}

// TestPG_PresetWritesAreAdminTier: a member may read presets but not write
// them, and a refused write leaves no row and no audit.
func TestPG_PresetWritesAreAdminTier(t *testing.T) {
	f := newPresetFixture(t)
	body, _ := json.Marshal(client.PresetRequest{Request: presetBundle()})
	if w := doSSO(t, f.srv, http.MethodPut, "/api/v1/presets/mine", f.member, string(body)); w.Code != http.StatusForbidden {
		t.Errorf("member put = %d, want 403: %s", w.Code, w.Body.String())
	}
	if _, err := f.pg.GetLaunchPreset(context.Background(), "mine"); err != store.ErrNotFound {
		t.Errorf("a refused member write stored a preset (err %v)", err)
	}
	if w := f.putPreset(t, "mine", client.PresetRequest{Request: presetBundle()}); w.Code != http.StatusCreated {
		t.Fatalf("admin put = %d: %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, f.srv, http.MethodDelete, "/api/v1/presets/mine", f.member, ""); w.Code != http.StatusForbidden {
		t.Errorf("member delete = %d, want 403", w.Code)
	}
	if len(f.actions("preset.create")) != 1 || len(f.actions("preset.delete")) != 0 {
		t.Errorf("audit: %d create, %d delete rows; want 1 and 0", len(f.actions("preset.create")), len(f.actions("preset.delete")))
	}
	if w := doSSO(t, f.srv, http.MethodGet, "/api/v1/presets/mine", f.member, ""); w.Code != http.StatusOK {
		t.Errorf("member read = %d, want 200", w.Code)
	}
}

// TestPG_PresetWriteValidation: a preset may not carry a per-launch field, and
// names only existing user types.
func TestPG_PresetWriteValidation(t *testing.T) {
	f := newPresetFixture(t)
	withTask := presetBundle()
	withTask.Task = "baked in"
	preSplit := presetBundle()
	preSplit.InlinePolicy.AzureDevOpsCapabilities = []adoscope.Capability{"read"}
	for _, tc := range []struct {
		name, preset string
		req          client.PresetRequest
	}{
		{"task in request", "ok", client.PresetRequest{Request: withTask}},
		{"unknown user type", "ok", client.PresetRequest{UserTypes: []string{"ghost"}, Request: presetBundle()}},
		{"bad name", "Not_A_Slug", client.PresetRequest{Request: presetBundle()}},
		{"no agent", "ok", client.PresetRequest{Request: client.CreateRunRequest{Repo: "acme/widgets"}}},
		{"the pre-split read id", "ok", client.PresetRequest{Request: preSplit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := f.putPreset(t, tc.preset, tc.req); w.Code != http.StatusBadRequest {
				t.Errorf("= %d %s, want 400", w.Code, w.Body.String())
			}
		})
	}
	if list, err := f.pg.ListLaunchPresets(context.Background()); err != nil || len(list) != 0 {
		t.Errorf("refused writes stored %d preset(s) (err %v)", len(list), err)
	}
}
