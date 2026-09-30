// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pvStore is govEscapeStore with the reads GET /runs/{id}/policy makes: the
// run's audit rows (the recorder every write lands in), its grants, and the
// governance profile list.
type pvStore struct {
	*govEscapeStore
	audit    *recRecorder
	grants   []types.CredentialGrant
	profiles []types.GovernanceProfile
}

func (s *pvStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	var out []types.AuditEvent
	for _, ev := range s.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s *pvStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, nil
}

func (s *pvStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return s.profiles, nil
}

func pvFixture(t *testing.T, cs *capStore) (*Server, *pvStore) {
	t.Helper()
	h := newHarness(t)
	audit := &recRecorder{}
	st := &pvStore{govEscapeStore: newGovEscapeStore(cs), audit: audit}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{govCorpSecret: []byte("v")}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), st
}

const pvOwner = "sub-pv-owner"

func (s *pvStore) seedRun(sub string, state types.RunState) types.AgentRun {
	run := types.AgentRun{
		ID: uuid.New(), CreatedAt: time.Now().UTC().Add(-time.Hour), CreatedBy: sub,
		Agent: "claude-code", State: state, ConfinementClass: types.CC2,
	}
	s.mu.Lock()
	s.runs[run.ID], s.states[run.ID] = run, state
	s.mu.Unlock()
	return run
}

func (s *pvStore) rows(runID uuid.UUID, rows ...pvRow) {
	at := time.Now().UTC().Add(-time.Hour)
	for i, r := range rows {
		b, err := json.Marshal(r.data)
		if err != nil {
			panic(err)
		}
		outcome := r.outcome
		if outcome == "" {
			outcome = "success"
		}
		id := runID
		_ = s.audit.Record(context.Background(), types.AuditEvent{
			ID: uuid.New(), Time: at.Add(time.Duration(i) * time.Second), RunID: &id,
			Action: r.action, Outcome: outcome, Data: b,
		})
	}
}

type pvRow struct {
	action, outcome string
	data            any
}

func pvResolve(spec types.RunPolicySpec, diskFilled bool) pvRow {
	return pvRow{action: "run.policy.resolve", data: effectivePolicyDatum{RunPolicySpec: spec, DiskMiBFilled: diskFilled}}
}

func pvCreate(policyID *uuid.UUID, src *policySourceRecord, clamp ...string) pvRow {
	d := map[string]any{"policy_id": policyID, "inline_policy": policyID == nil}
	if src != nil {
		d["policy_source"] = src
	}
	if len(clamp) > 0 {
		d["clamp_warnings"] = clamp
	}
	return pvRow{action: "run.create", data: d}
}

func pvSource(kind string, id *uuid.UUID, name string, bounded bool, spec types.RunPolicySpec) *policySourceRecord {
	r := newPolicySourceRecord(kind, id, name, nil, bounded, spec)
	return &r
}

type pvReader struct {
	cookie *http.Cookie // nil: the admin token
}

func (rd pvReader) get(t *testing.T, srv *Server, runID uuid.UUID) (*httptest.ResponseRecorder, runPolicyResponse) {
	t.Helper()
	path := "/api/v1/runs/" + runID.String() + "/policy"
	var w *httptest.ResponseRecorder
	if rd.cookie == nil {
		w = do(t, srv, http.MethodGet, path, adminToken, "")
	} else {
		w = doSSO(t, srv, http.MethodGet, path, rd.cookie, "")
	}
	var resp runPolicyResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode policy view: %v (%s)", err, w.Body.String())
		}
	}
	return w, resp
}

func pvMember(t *testing.T, sub string) pvReader { return pvReader{govSession(t, sub, nil, false)} }

func pvSecurity(t *testing.T) pvReader {
	return pvReader{ssoSession(t, "sub-pv-sec", "sec@corp.example", oidc.RoleSecurityAdmin)}
}

func pvChange(changes []runPolicyChange, cause, field string) *runPolicyChange {
	for i := range changes {
		if changes[i].Cause == cause && changes[i].Field == field {
			return &changes[i]
		}
	}
	return nil
}

func TestRunPolicyView_SourceKinds(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	member := pvMember(t, pvOwner)
	base := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	polID := uuid.New()
	st.policies[polID] = types.RunPolicy{ID: polID, Name: "ci-now", Spec: base}
	profID := uuid.New()
	st.profiles = []types.GovernanceProfile{{ID: profID, Name: "walled"}}

	cases := []struct {
		name     string
		mutate   func(*types.AgentRun)
		row      pvRow
		wantKind string
		wantName string
		complete bool
	}{
		{"recorded stored", nil, pvCreate(&polID, pvSource(policyKindStored, &polID, "ci-at-launch", false, base)), policyKindStored, "ci-at-launch", true},
		{"recorded inline", nil, pvCreate(nil, pvSource(policyKindInline, nil, "", true, base)), policyKindInline, "", true},
		{"recorded default", nil, pvCreate(nil, pvSource(policyKindDefault, nil, "", false, base)), policyKindDefault, "", true},
		{"recorded profile", nil, pvCreate(nil, pvSource(policyKindProfile, nil, "walled", true, base)), policyKindProfile, "walled", true},
		{"older stored takes the live name", nil, pvCreate(&polID, nil), policyKindStored, "ci-now", false},
		{"older inline", nil, pvCreate(nil, nil), policyKindInline, "", false},
		{"older profile", func(r *types.AgentRun) { r.GovernanceProfileID = &profID },
			pvRow{action: "run.create", data: map[string]any{"inline_policy": false}}, policyKindProfile, "walled", false},
		{"older default", nil, pvRow{action: "run.create", data: map[string]any{"inline_policy": false}}, policyKindDefault, "", false},
		{"a lane that writes its own row", nil, pvRow{action: "run.create", data: map[string]any{"agent": "claude-code"}}, policyKindUnknown, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := st.seedRun(pvOwner, types.RunRunning)
			if tc.mutate != nil {
				tc.mutate(&run)
				st.mu.Lock()
				st.runs[run.ID] = run
				st.mu.Unlock()
			}
			st.rows(run.ID, tc.row, pvResolve(base, false))
			w, resp := member.get(t, srv, run.ID)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if resp.Source.Kind != tc.wantKind || resp.Source.Name != tc.wantName || resp.Complete != tc.complete {
				t.Errorf("source = %+v complete=%v, want kind %q name %q complete %v", resp.Source, resp.Complete, tc.wantKind, tc.wantName, tc.complete)
			}
			if resp.State != "recorded" || resp.RecordedAt == nil || resp.Spec == nil {
				t.Errorf("state=%q recorded_at=%v spec=%v, want a recorded envelope", resp.State, resp.RecordedAt, resp.Spec)
			}
		})
	}
}

func TestRunPolicyView_PresetRidesTheSource(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	run.Preset, run.PresetVersion = "nightly", 3
	st.mu.Lock()
	st.runs[run.ID] = run
	st.mu.Unlock()
	st.rows(run.ID, pvCreate(nil, nil), pvResolve(types.RunPolicySpec{MinConfinementClass: types.CC2}, false))
	_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
	if resp.Source.Preset != "nightly" || resp.Source.PresetVersion != 3 {
		t.Errorf("source = %+v, want preset nightly v3", resp.Source)
	}
}

// pvSecretSpec is a resolved policy carrying everything a member must not read:
// a host mount path, an operator secret name, and a raw inspection corpus.
func pvSecretSpec() types.RunPolicySpec {
	return types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC2,
		WorkspaceMounts:     []types.WorkspaceMount{{Source: "/srv/team-data", Target: "/home/agent/data"}},
		EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantAPIKey, Scope: pvScope("api.anthropic.com", govCorpSecret), TTLSeconds: 300,
		}},
		LLMInspection: &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{"prod-db"}},
	}
}

func pvScope(host, secret string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"host": host, "header": "Authorization", "secret_name": secret})
	return b
}

func TestRunPolicyView_RedactionPerReader(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	resolved := pvSecretSpec()
	// The envelope as dispatch writes it: values already replaced by a count.
	resolved.LLMInspection.WorkspaceSecretValues = []string{"<2 value(s) redacted>"}
	st.rows(run.ID, pvCreate(nil, pvSource(policyKindInline, nil, "", true, pvSecretSpec())), pvResolve(resolved, false))

	for _, tc := range []struct {
		name         string
		reader       pvReader
		hidden       bool
		wantRedacted bool
	}{
		{"member owner", pvMember(t, pvOwner), true, true},
		{"security admin", pvSecurity(t), false, false},
		{"super admin", pvReader{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, resp := tc.reader.get(t, srv, run.ID)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if strings.Contains(body, "workspace_secret_values") || strings.Contains(body, "redacted}") || strings.Contains(body, "value(s) redacted") {
				t.Errorf("the inspection corpus, even as a count, reached %s: %s", tc.name, body)
			}
			if resp.Redacted != tc.wantRedacted {
				t.Errorf("redacted = %v, want %v", resp.Redacted, tc.wantRedacted)
			}
			leaked := strings.Contains(body, "/srv/team-data") || strings.Contains(body, govCorpSecret)
			if tc.hidden == leaked {
				t.Errorf("mount path or secret name visible=%v for %s, want visible=%v: %s", leaked, tc.name, !tc.hidden, body)
			}
			if tc.hidden && resp.Spec.WorkspaceMounts[0].Source != "<redacted>" {
				t.Errorf("mount source = %q, want <redacted>", resp.Spec.WorkspaceMounts[0].Source)
			}
			if resp.Spec.LLMInspection == nil || len(resp.Spec.LLMInspection.WorkspaceSecretNames) != 1 {
				t.Errorf("llm_inspection = %+v, want the names kept", resp.Spec.LLMInspection)
			}
		})
	}
}

// TestRunPolicyView_SpecReuse pins the reuse promise, scoped: the spec always
// strict-decodes as a policy, passes the write validator for the security
// tier, and does not for a reader whose hidden values were replaced.
func TestRunPolicyView_SpecReuse(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	spec := pvSecretSpec()
	spec.LLMInspection.WorkspaceSecretValues = []string{"<1 value(s) redacted>"}
	st.rows(run.ID, pvResolve(spec, true))
	strict := func(t *testing.T, body []byte) types.RunPolicySpec {
		t.Helper()
		var doc struct {
			Spec json.RawMessage `json:"spec"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(doc.Spec))
		dec.DisallowUnknownFields()
		var got types.RunPolicySpec
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("the served spec does not strict-decode as a policy: %v\n%s", err, doc.Spec)
		}
		return got
	}
	sec, _ := pvSecurity(t).get(t, srv, run.ID)
	if err := validatePolicySpec(strict(t, sec.Body.Bytes())); err != nil {
		t.Errorf("the security tier's spec does not pass validatePolicySpec: %v", err)
	}
	member, _ := pvMember(t, pvOwner).get(t, srv, run.ID)
	got := strict(t, member.Body.Bytes())
	if err := validatePolicySpec(got); err == nil {
		t.Error("the member-tier spec passed validatePolicySpec; the <redacted> mount source must fail it")
	}
	if strings.Contains(member.Body.String(), "disk_mib_filled") {
		t.Error("disk_mib_filled leaked into the document; it must stay outside the policy")
	}
}

func TestRunPolicyView_ForeignReaderGetsTheRunsOwn404(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	st.rows(run.ID, pvResolve(types.RunPolicySpec{MinConfinementClass: types.CC2}, false))
	foreign := pvMember(t, "sub-pv-foreign")

	wPolicy, _ := foreign.get(t, srv, run.ID)
	wRun := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), foreign.cookie, "")
	if wPolicy.Code != http.StatusNotFound || wRun.Code != http.StatusNotFound {
		t.Fatalf("foreign reader: policy=%d run=%d, want 404 both", wPolicy.Code, wRun.Code)
	}
	if !bytes.Equal(wPolicy.Body.Bytes(), wRun.Body.Bytes()) {
		t.Errorf("the 404 differs from GET /runs/{id}'s:\n policy: %s\n run:    %s", wPolicy.Body.String(), wRun.Body.String())
	}
	var notOwner bool
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action == authz.AuditAction && strings.Contains(string(ev.Data), `"reason":"not_owner"`) {
			notOwner = true
		}
	}
	if !notOwner {
		t.Error("no not_owner audit row for the foreign read")
	}
	if w, _ := foreign.get(t, srv, uuid.New()); w.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", w.Code)
	}
}

func TestRunPolicyView_DelegatedTokenRefused(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	const person = "sub-person"
	tok, _ := seedDelegation(t, ast.fakeDelegateStore, person)
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: person, State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()
	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/policy", tok, "")
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || body.Reason != string(authz.ReasonDelegationScope) {
		t.Fatalf("delegated read = %d reason %q, want 403 delegation_scope: %s", w.Code, body.Reason, w.Body.String())
	}
}

func TestRunPolicyView_NoEnvelopeStates(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	member := pvMember(t, pvOwner)
	for _, tc := range []struct {
		state types.RunState
		want  string
	}{{types.RunPending, "not_yet"}, {types.RunStarting, "not_yet"}, {types.RunFailed, "never"}, {types.RunKilled, "never"}} {
		run := st.seedRun(pvOwner, tc.state)
		st.rows(run.ID, pvCreate(nil, pvSource(policyKindInline, nil, "", true, types.RunPolicySpec{})))
		w, resp := member.get(t, srv, run.ID)
		if w.Code != http.StatusOK || resp.State != tc.want || resp.Spec != nil || resp.RecordedAt != nil {
			t.Errorf("%s: status %d state %q spec %v, want 200 %q with no spec", tc.state, w.Code, resp.State, resp.Spec, tc.want)
		}
		if resp.Source.Kind != policyKindInline || resp.Changes == nil || !strings.Contains(w.Body.String(), `"changes":[]`) {
			t.Errorf("%s: source %+v, changes must still say [] and the source still shows: %s", tc.state, resp.Source, w.Body.String())
		}
	}
}

func TestRunPolicyView_LegacyActionAccepted(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	row := pvResolve(types.RunPolicySpec{AllowedDomains: []string{"legacy.example"}, MinConfinementClass: types.CC2}, false)
	row.action = "run.policy.effective"
	st.rows(run.ID, pvCreate(nil, nil), row)
	_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
	if resp.State != "recorded" || resp.Spec == nil || !slices.Contains(resp.Spec.AllowedDomains, "legacy.example") {
		t.Errorf("legacy run.policy.effective not read: %+v", resp)
	}
}

func TestRunPolicyView_OlderRunIgnoresCreateFailureRows(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	polID := uuid.New()
	st.policies[polID] = types.RunPolicy{ID: polID, Name: "not-this-one"}
	// The failure family reuses the action name and carries its own facts.
	failed := pvCreate(&polID, nil)
	failed.outcome = "failure"
	st.rows(run.ID, failed, pvCreate(nil, nil), pvResolve(types.RunPolicySpec{MinConfinementClass: types.CC2}, false))
	_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
	if resp.Source.Kind != policyKindInline || resp.Source.PolicyID != nil {
		t.Errorf("source = %+v, want the success row's inline source, not the failure row's saved policy", resp.Source)
	}
}

func TestRunPolicyView_RestartDeniesAreFolded(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	base := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, DeniedDomains: []string{"old.example"}, MinConfinementClass: types.CC2}
	st.rows(run.ID,
		pvCreate(nil, pvSource(policyKindInline, nil, "", false, base)),
		pvResolve(base, false),
		pvRow{action: "run.revive", data: map[string]any{"denied_added": []string{"walled.example", "old.example"}}},
		pvRow{action: "run.revive", outcome: "failure", data: map[string]any{"denied_added": []string{"never-applied.example"}}},
	)
	// pvStore is not a store.Pager, so this reads through the fetch-all fallback.
	_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
	if !slices.Equal(resp.Spec.DeniedDomains, []string{"old.example", "walled.example"}) {
		t.Errorf("denied_domains = %v, want the launch deny plus the restart deny, once each", resp.Spec.DeniedDomains)
	}
	c := pvChange(resp.Changes, causeRestart, fieldDenied)
	if c == nil || !slices.Equal(c.Added, []string{"walled.example"}) || c.At == nil {
		t.Errorf("changes = %+v, want one restart change adding walled.example with a time", resp.Changes)
	}
}

func TestRunPolicyView_ChangesNeverNull(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	st.rows(run.ID, pvCreate(nil, pvSource(policyKindDefault, nil, "", false, spec)), pvResolve(spec, false))
	w, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
	if !strings.Contains(w.Body.String(), `"changes":[]`) || len(resp.Changes) != 0 || !resp.Complete {
		t.Errorf("an unchanged run: %s", w.Body.String())
	}
}

func TestRunPolicyView_StoredPolicyNow(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	member := pvMember(t, pvOwner)
	at := func(d time.Duration) time.Time { return time.Now().UTC().Add(d) }
	launch := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	edited := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "new.example"}, MinConfinementClass: types.CC2}
	seed := func(cur *types.RunPolicy, recorded bool) uuid.UUID {
		id := uuid.New()
		if cur != nil {
			cur.ID = id
			st.policies[id] = *cur
		}
		run := st.seedRun(pvOwner, types.RunRunning)
		var src *policySourceRecord
		if recorded {
			src = pvSource(policyKindStored, &id, "ci", false, launch)
		}
		st.rows(run.ID, pvCreate(&id, src), pvResolve(launch, false))
		return run.ID
	}
	check := func(t *testing.T, runID uuid.UUID, wantState, wantName string) {
		t.Helper()
		_, resp := member.get(t, srv, runID)
		if resp.StoredPolicyNow == nil || resp.StoredPolicyNow.State != wantState || resp.StoredPolicyNow.Name != wantName {
			t.Errorf("stored_policy_now = %+v, want %s %q", resp.StoredPolicyNow, wantState, wantName)
		}
		if wantState == "deleted" && !resp.Source.Deleted {
			t.Errorf("source.deleted = false for a deleted saved policy: %+v", resp.Source)
		}
	}
	t.Run("same", func(t *testing.T) {
		check(t, seed(&types.RunPolicy{Name: "ci", Spec: launch, UpdatedAt: at(-2 * time.Hour)}, true), "same", "ci")
	})
	t.Run("changed", func(t *testing.T) {
		check(t, seed(&types.RunPolicy{Name: "ci", Spec: edited, UpdatedAt: at(-time.Minute)}, true), "changed", "ci")
	})
	t.Run("a rename alone reads same, under the current name", func(t *testing.T) {
		check(t, seed(&types.RunPolicy{Name: "ci-renamed", Spec: launch, UpdatedAt: at(-time.Minute)}, true), "same", "ci-renamed")
	})
	t.Run("a secret-only edit is invisible to the redacted comparison", func(t *testing.T) {
		withSecret := pvSecretSpec()
		withSecret.LLMInspection = nil
		id := uuid.New()
		st.policies[id] = types.RunPolicy{ID: id, Name: "ci", Spec: withSecret, UpdatedAt: at(-time.Minute)}
		run := st.seedRun(pvOwner, types.RunRunning)
		st.rows(run.ID, pvCreate(&id, pvSource(policyKindStored, &id, "ci", false, withSecret)), pvResolve(withSecret, false))
		check(t, run.ID, "same", "ci")
	})
	t.Run("older run, edited after launch", func(t *testing.T) {
		check(t, seed(&types.RunPolicy{Name: "ci", Spec: edited, UpdatedAt: at(time.Minute)}, false), "updated", "ci")
	})
	t.Run("older run, untouched", func(t *testing.T) {
		check(t, seed(&types.RunPolicy{Name: "ci", Spec: launch, UpdatedAt: at(-2 * time.Hour)}, false), "same", "ci")
	})
	t.Run("deleted", func(t *testing.T) {
		check(t, seed(nil, true), "deleted", "")
	})
	t.Run("not offered for an inline source", func(t *testing.T) {
		run := st.seedRun(pvOwner, types.RunRunning)
		st.rows(run.ID, pvCreate(nil, pvSource(policyKindInline, nil, "", false, launch)), pvResolve(launch, false))
		if _, resp := member.get(t, srv, run.ID); resp.StoredPolicyNow != nil {
			t.Errorf("stored_policy_now = %+v for an inline source", resp.StoredPolicyNow)
		}
	})
}

// TestRunPolicyView_CausesEndToEnd reads the causes through the route from
// launch rows written by the shapes the writers use.
func TestRunPolicyView_CausesEndToEnd(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	base := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "corp.example"}, MinConfinementClass: types.CC2}
	resolved := types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com", "registry.npmjs.org"},
		DeniedDomains:       []string{"corp.example"},
		MinConfinementClass: types.CC2,
	}
	st.rows(run.ID,
		pvCreate(nil, pvSource(policyKindInline, nil, "", true, base), "Removed corp.example: not allowed for you."),
		pvRow{action: "run.egress.add", data: map[string]any{"kind": "workspace", "added_domains": []string{"registry.npmjs.org"}}},
		pvResolve(resolved, false),
	)
	_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
	if c := pvChange(resp.Changes, causeWorkspace, fieldAllowed); c == nil || !slices.Equal(c.Added, []string{"registry.npmjs.org"}) {
		t.Errorf("workspace change = %+v in %+v", c, resp.Changes)
	}
	lim := pvChange(resp.Changes, causeLimits, fieldAllowed)
	if lim == nil || !slices.Equal(lim.Removed, []string{"corp.example"}) || len(lim.Detail) != 1 {
		t.Errorf("limits change = %+v, want corp.example removed with the clamp sentence as detail", lim)
	}
	if c := pvChange(resp.Changes, causeLimits, fieldDenied); c == nil || !slices.Equal(c.Added, []string{"corp.example"}) || len(c.Detail) != 0 {
		t.Errorf("limits deny change = %+v, want the deny added, with no repeated detail", c)
	}
}

// the pure provenance function

func evidenceOf(mut func(*auditEvidence)) auditEvidence {
	ev := collectEvidence(nil, nil)
	if mut != nil {
		mut(&ev)
	}
	return ev
}

func pvSpec(allowed []string, mut func(*types.RunPolicySpec)) types.RunPolicySpec {
	sp := types.RunPolicySpec{AllowedDomains: allowed, MinConfinementClass: types.CC2}
	if mut != nil {
		mut(&sp)
	}
	return sp
}

func TestExplainRunPolicy_OneCasePerCause(t *testing.T) {
	deleted := func(s ...string) func(*types.RunPolicySpec) {
		return func(sp *types.RunPolicySpec) { sp.DeniedDomains = s }
	}
	restartAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm", Ecosystem: "npm"},
	}}
	netOnly := types.SiteConfig{EgressRedirects: []types.EgressRedirect{{From: "https://files.example/", To: "https://mirror.corp/"}}}
	provider := types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{{
		ID: "mp1", Kind: types.ModelProviderAnthropicAPIKey, Name: "Anthropic",
		Harnesses: []types.ProviderHarness{{Harness: "claude-code"}},
	}}}}
	cases := []struct {
		name    string
		base    *types.RunPolicySpec
		got     types.RunPolicySpec
		ev      auditEvidence
		sc      types.SiteConfig
		run     types.AgentRun
		cause   string
		field   string
		added   []string
		removed []string
	}{
		{"workspace: egress.add", ptr(pvSpec([]string{"a.example"}, nil)), pvSpec([]string{"a.example", "npm.example"}, nil),
			evidenceOf(func(e *auditEvidence) { e.workspace["npm.example"] = true }), types.SiteConfig{}, types.AgentRun{},
			causeWorkspace, fieldAllowed, []string{"npm.example"}, nil},
		{"source_control: egress.add", ptr(pvSpec(nil, nil)), pvSpec([]string{"github.example"}, nil),
			evidenceOf(func(e *auditEvidence) { e.sourceControl["github.example"] = true }), types.SiteConfig{}, types.AgentRun{},
			causeSourceControl, fieldAllowed, []string{"github.example"}, nil},
		{"mirror: the To host", ptr(pvSpec(nil, nil)), pvSpec([]string{"artifactory.corp:443"}, nil),
			evidenceOf(func(e *auditEvidence) {
				e.mirror["artifactory.corp:443"] = true
				e.mirrorHosts["artifactory.corp"] = true
			}), sc, types.AgentRun{},
			causeMirror, fieldAllowed, []string{"artifactory.corp:443"}, nil},
		{"mirror: (d) the public hosts it replaced", ptr(pvSpec([]string{"registry.npmjs.org", "a.example"}, nil)), pvSpec([]string{"a.example", "artifactory.corp:443"}, nil),
			evidenceOf(func(e *auditEvidence) {
				e.mirror["artifactory.corp:443"] = true
				e.mirrorHosts["artifactory.corp"] = true
			}), sc, types.AgentRun{},
			causeMirror, fieldAllowed, []string{"artifactory.corp:443"}, []string{"registry.npmjs.org"}},
		{"mirror: (c) a network-only redirect's deny", ptr(pvSpec([]string{"a.example"}, nil)), pvSpec([]string{"a.example"}, deleted("files.example")),
			evidenceOf(nil), netOnly, types.AgentRun{},
			causeMirror, fieldDenied, []string{"files.example"}, nil},
		{"model_access: bedrock hosts", ptr(pvSpec(nil, nil)), pvSpec([]string{"bedrock.example"}, nil),
			evidenceOf(func(e *auditEvidence) { e.model["bedrock.example"] = true }), types.SiteConfig{}, types.AgentRun{},
			causeModelAccess, fieldAllowed, []string{"bedrock.example"}, nil},
		{"model_access: (b) the provider's key host", ptr(pvSpec(nil, nil)), pvSpec([]string{"api.anthropic.com"}, nil),
			evidenceOf(nil), provider, types.AgentRun{Agent: "claude-code", ModelProviderID: "mp1"},
			causeModelAccess, fieldAllowed, []string{"api.anthropic.com"}, nil},
		{"git_broker: the confine row", ptr(pvSpec([]string{"github.com"}, nil)), pvSpec(nil, deleted("github.com")),
			evidenceOf(func(e *auditEvidence) { e.confine["github.com"] = true }), types.SiteConfig{}, types.AgentRun{},
			causeGitBroker, fieldDenied, []string{"github.com"}, nil},
		{"profile: denied_added", ptr(pvSpec(nil, nil)), pvSpec(nil, deleted("corp.example")),
			evidenceOf(func(e *auditEvidence) { e.profileDenied["corp.example"] = true; e.profile = "walled" }), types.SiteConfig{}, types.AgentRun{},
			causeProfile, fieldDenied, []string{"corp.example"}, nil},
		{"profile: disk clamped to its max", ptr(pvSpec(nil, func(s *types.RunPolicySpec) { s.Resources = &types.ResourceLimits{DiskMiB: 9000} })),
			pvSpec(nil, func(s *types.RunPolicySpec) { s.Resources = &types.ResourceLimits{DiskMiB: 4096} }),
			evidenceOf(func(e *auditEvidence) { e.profile, e.profileMaxDisk = "walled", 4096 }), types.SiteConfig{}, types.AgentRun{},
			causeProfile, fieldDisk, []string{"4096"}, []string{"9000"}},
		{"org_disk: filled from the default", ptr(pvSpec(nil, nil)),
			pvSpec(nil, func(s *types.RunPolicySpec) { s.Resources = &types.ResourceLimits{DiskMiB: 8192} }),
			evidenceOf(func(e *auditEvidence) { e.diskFilled = true }), types.SiteConfig{}, types.AgentRun{},
			causeOrgDisk, fieldDisk, []string{"8192"}, nil},
		{"restart: revive denies", ptr(pvSpec(nil, nil)), pvSpec(nil, deleted("late.example")),
			evidenceOf(func(e *auditEvidence) { e.restart["late.example"] = restartAt }), types.SiteConfig{}, types.AgentRun{},
			causeRestart, fieldDenied, []string{"late.example"}, nil},
		{"source_control: (c) ADO Entra entries", ptr(pvSpec(nil, nil)),
			pvSpec(adoEntraEgressEntries("contoso"), nil),
			evidenceOf(nil), types.SiteConfig{}, types.AgentRun{},
			causeSourceControl, fieldAllowed, adoEntraEgressEntries("contoso"), nil},
		{"limits: a removal on a bound run", ptr(pvSpec([]string{"a.example", "wide.example"}, nil)), pvSpec([]string{"a.example"}, nil),
			evidenceOf(func(e *auditEvidence) { e.bounded = true; e.clamp = []string{"Removed wide.example."} }), types.SiteConfig{}, types.AgentRun{},
			causeLimits, fieldAllowed, nil, []string{"wide.example"}},
		{"limits: a raised floor", ptr(types.RunPolicySpec{MinConfinementClass: types.CC1}), types.RunPolicySpec{MinConfinementClass: types.CC3},
			evidenceOf(func(e *auditEvidence) { e.bounded = true }), types.SiteConfig{}, types.AgentRun{},
			causeLimits, fieldMinCC, []string{"CC3"}, []string{"CC1"}},
		{"git_broker: an older run holding a github grant", nil, pvSpec(nil, deleted("github.com")),
			evidenceOf(func(e *auditEvidence) { e.legacyGitBroker = true }), types.SiteConfig{}, types.AgentRun{},
			causeGitBroker, fieldDenied, []string{"github.com"}, nil},
		{"launch: anything unexplained", ptr(pvSpec(nil, nil)), pvSpec([]string{"odd.example"}, nil),
			evidenceOf(nil), types.SiteConfig{}, types.AgentRun{},
			causeLaunch, fieldAllowed, []string{"odd.example"}, nil},
		{"launch: a change on an unbound run is not limits", ptr(pvSpec([]string{"a.example", "b.example"}, nil)), pvSpec([]string{"a.example"}, nil),
			evidenceOf(nil), types.SiteConfig{}, types.AgentRun{},
			causeLaunch, fieldAllowed, nil, []string{"b.example"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := explainRunPolicy(tc.base, tc.got, tc.ev, tc.sc, tc.run)
			c := pvChange(got, tc.cause, tc.field)
			if c == nil || !slices.Equal(c.Added, tc.added) || !slices.Equal(c.Removed, tc.removed) {
				t.Fatalf("changes = %+v, want %s/%s added %v removed %v", got, tc.cause, tc.field, tc.added, tc.removed)
			}
			if tc.cause == causeRestart && (c.At == nil || !c.At.Equal(restartAt)) {
				t.Errorf("restart change at = %v, want %v", c.At, restartAt)
			}
			if tc.cause == causeProfile && tc.field == fieldDenied && c.Profile != "walled" {
				t.Errorf("profile = %q, want walled", c.Profile)
			}
			for _, other := range got {
				if other.Cause != tc.cause && other.Cause == causeLaunch && tc.cause != causeLaunch {
					t.Errorf("an entry fell through to launch: %+v", other)
				}
			}
		})
	}
}

// the finding-3 orderings, each with the wrong cause it must not read as

func TestExplainRunPolicy_RuleOrder(t *testing.T) {
	deny := func(sp *types.RunPolicySpec) { sp.DeniedDomains = []string{"github.com"} }
	t.Run("(a) no git grants and a profile that walls off github.com reads profile", func(t *testing.T) {
		got := explainRunPolicy(ptr(pvSpec(nil, nil)), pvSpec(nil, deny),
			evidenceOf(func(e *auditEvidence) { e.profileDenied["github.com"] = true; e.profile = "walled" }), types.SiteConfig{}, types.AgentRun{})
		if c := pvChange(got, causeProfile, fieldDenied); c == nil || pvChange(got, causeGitBroker, fieldDenied) != nil {
			t.Errorf("changes = %+v, want profile and no git_broker", got)
		}
	})
	t.Run("(a) an older run: profile still beats the grants-gated git rule", func(t *testing.T) {
		got := explainRunPolicy(nil, pvSpec(nil, deny),
			evidenceOf(func(e *auditEvidence) {
				e.profileDenied["github.com"] = true
				e.profile = "walled"
				e.legacyGitBroker = true
			}), types.SiteConfig{}, types.AgentRun{})
		if pvChange(got, causeProfile, fieldDenied) == nil || pvChange(got, causeGitBroker, fieldDenied) != nil {
			t.Errorf("changes = %+v, want profile and no git_broker", got)
		}
	})
	t.Run("an older run with no github grant never reads git_broker", func(t *testing.T) {
		got := explainRunPolicy(nil, pvSpec(nil, deny), evidenceOf(nil), types.SiteConfig{}, types.AgentRun{})
		if len(got) != 0 {
			t.Errorf("changes = %+v, want none: nothing states a cause for an older run without a base", got)
		}
	})
	t.Run("(e) clamp warnings on a run that was not bound produce no limits entry", func(t *testing.T) {
		got := explainRunPolicy(ptr(pvSpec([]string{"a.example", "b.example"}, nil)), pvSpec([]string{"a.example"}, nil),
			evidenceOf(func(e *auditEvidence) { e.clamp = []string{"profile warning"} }), types.SiteConfig{}, types.AgentRun{})
		if pvChange(got, causeLimits, fieldAllowed) != nil {
			t.Errorf("changes = %+v, want no limits entry when bounded is false", got)
		}
	})
	t.Run("an evidence row beats limits on a bound run", func(t *testing.T) {
		got := explainRunPolicy(ptr(pvSpec([]string{"github.com"}, nil)), pvSpec(nil, deny),
			evidenceOf(func(e *auditEvidence) { e.bounded = true; e.confine["github.com"] = true }), types.SiteConfig{}, types.AgentRun{})
		if pvChange(got, causeGitBroker, fieldDenied) == nil || pvChange(got, causeLimits, fieldDenied) != nil {
			t.Errorf("changes = %+v, want git_broker for the deny", got)
		}
	})
	t.Run("without a base, entries no row claims stay silent", func(t *testing.T) {
		got := explainRunPolicy(nil, pvSpec([]string{"a.example", "npm.example"}, nil),
			evidenceOf(func(e *auditEvidence) { e.workspace["npm.example"] = true }), types.SiteConfig{}, types.AgentRun{})
		if len(got) != 1 || got[0].Cause != causeWorkspace || !slices.Equal(got[0].Added, []string{"npm.example"}) {
			t.Errorf("changes = %+v, want exactly the claimed workspace entry", got)
		}
	})
}

func TestExplainRunPolicy_GrantsMountsAndReposUseRedactionSafeKeys(t *testing.T) {
	scope := func(kv map[string]any) json.RawMessage { b, _ := json.Marshal(kv); return b }
	resolved := pvSpec(nil, func(s *types.RunPolicySpec) {
		s.EligibleGrants = []types.GrantSpec{
			{Kind: types.GrantAPIKey, Scope: scope(map[string]any{"host": "api.example", "secret_name": "hidden-name"})},
			{Kind: types.GrantGitHubToken, Scope: scope(map[string]any{"repos": []string{"o/r", "o/s"}})},
		}
		s.WorkspaceMounts = []types.WorkspaceMount{{Source: "/srv/hidden", Target: "/home/agent/data"}}
		s.WorkspaceRepos = []types.WorkspaceRepo{{Repo: "o/r", Target: "/w/r", Ref: "main"}}
	})
	// The base is what the run.create row holds: redacted.
	got := explainRunPolicy(ptr(redactSpecForUser(pvSpec(nil, nil))), resolved, evidenceOf(nil), types.SiteConfig{}, types.AgentRun{})
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "hidden") {
		t.Errorf("a change entry names a hidden value: %s", body)
	}
	if c := pvChange(got, causeLaunch, fieldGrants); c == nil || !slices.Equal(c.Added, []string{"api_key:api.example", "github_token:o/r,o/s"}) {
		t.Errorf("grant keys = %+v", c)
	}
	if c := pvChange(got, causeLaunch, fieldMounts); c == nil || !slices.Equal(c.Added, []string{"/home/agent/data"}) {
		t.Errorf("mount keys = %+v", c)
	}
	if c := pvChange(got, causeLaunch, fieldRepos); c == nil || !slices.Equal(c.Added, []string{"o/r@main"}) {
		t.Errorf("repo keys = %+v", c)
	}
	// A redacted twin of the same policy has no differences at all.
	if same := explainRunPolicy(ptr(redactSpecForUser(resolved)), resolved, evidenceOf(nil), types.SiteConfig{}, types.AgentRun{}); len(same) != 0 {
		t.Errorf("a policy against its own redacted copy = %+v, want no changes", same)
	}
}

// TestLatestPolicyResolve_LastWinsAndSkipsUndecodable pins the shared reader
// the UI gateway and this view both use.
func TestLatestPolicyResolve_LastWinsAndSkipsUndecodable(t *testing.T) {
	mk := func(action string, data string, at time.Time) types.AuditEvent {
		return types.AuditEvent{Action: action, Data: json.RawMessage(data), Time: at}
	}
	t0 := time.Now()
	events := []types.AuditEvent{
		mk("run.policy.resolve", `{"allowed_domains":["first.example"]}`, t0),
		mk("run.policy.effective", `{"allowed_domains":["second.example"],"disk_mib_filled":true}`, t0.Add(time.Second)),
		mk("run.policy.resolve", `{not json`, t0.Add(2*time.Second)),
		mk("run.policy.resolve", ``, t0.Add(3*time.Second)),
	}
	d, at, ok := latestPolicyResolve(events)
	if !ok || !slices.Equal(d.AllowedDomains, []string{"second.example"}) || !d.DiskMiBFilled || !at.Equal(t0.Add(time.Second)) {
		t.Errorf("latest = %+v at %v ok %v", d, at, ok)
	}
	if _, _, ok := latestPolicyResolve(nil); ok {
		t.Error("no envelope reported as found")
	}
}
