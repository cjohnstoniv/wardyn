// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// deny-f2: a refusal caused by the resolved ceiling names the leaf profile and
// how to ask for a change, and nothing else about it.

// policyContactFor is a contact whose every field carries tag, so a value that
// reaches the wrong person can be traced to the profile it came from.
func policyContactFor(tag string) *policyref.Contact {
	return &policyref.Contact{
		Owner: tag + " Owner", Email: tag + "@" + tag + ".example",
		RequestURL: "https://" + tag + ".example/request", RequestText: "ask the " + tag + " team",
	}
}

// taggedProfile is a limits profile that publishes policyContactFor(tag).
func taggedProfile(name, tag string, limits types.GovernanceLimits) *types.GovernanceProfile {
	p := limitsProfile(name, limits)
	p.Contact = policyContactFor(tag)
	return p
}

// profileRefFor is the Ref taggedProfile(name, tag) must be named by.
func profileRefFor(name, tag string) policyref.Ref {
	c := policyContactFor(tag)
	return policyref.Ref{Source: policyref.SourceProfile, Name: name, Owner: c.Owner, Email: c.Email,
		RequestURL: c.RequestURL, RequestText: c.RequestText}
}

// refusalWire is a refusal body as the SDK reads it.
type refusalWire struct {
	Error  string         `json:"error"`
	Reason string         `json:"reason"`
	Policy *policyref.Ref `json:"policy"`
}

func decodeRefusal(t *testing.T, w *httptest.ResponseRecorder) refusalWire {
	t.Helper()
	var b refusalWire
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, w.Body.String())
	}
	return b
}

// activeRunsStore reports a fixed run count, the read the run quota makes.
type activeRunsStore struct {
	*govEscapeStore
	active int
}

func (s *activeRunsStore) CountActiveRunsBy(context.Context, string) (int, error) {
	return s.active, nil
}

// TestCeilingRefusalsNameThePolicy walks every ceiling door POST /runs reaches
// (the shape limits, the run quota, the hold-rule refusals and the autonomy
// ladder) and requires each to answer with the leaf profile and its contact,
// beside the reason and sentence it answered before.
func TestCeilingRefusalsNameThePolicy(t *testing.T) {
	const name, tag = "walled", "alpha"
	want := profileRefFor(name, tag)
	hold := taggedProfile(name, tag, types.GovernanceLimits{})
	hold.Ceiling.ToolRules = []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}
	ladder := taggedProfile(name, tag, types.GovernanceLimits{AutonomyRubric: autonomyRubric(types.AutonomyL0)})

	for _, tc := range []struct {
		name       string
		profile    *types.GovernanceProfile
		body       string
		active     int
		wantStatus int
		wantReason string
		wantTarget string
	}{
		{"task_mode exec", taggedProfile(name, tag, types.GovernanceLimits{DenyTaskModeExec: true}),
			`{"agent":"claude-code","task":"echo hi","task_mode":"exec"}`, 0, 403, "governance_profile", "runs.task_mode"},
		{"shell boot seed", taggedProfile(name, tag, types.GovernanceLimits{DenyTaskModeExec: true}),
			`{"agent":"claude-code","task":"t","interactive":true}`, 0, 403, "governance_profile", "runs.interactive_start"},
		{"interactive", taggedProfile(name, tag, types.GovernanceLimits{DenyInteractive: true}),
			`{"agent":"claude-code"}`, 0, 403, "governance_profile", "runs.interactive"},
		{"run quota", taggedProfile(name, tag, types.GovernanceLimits{MaxConcurrentRuns: 2}),
			`{"agent":"claude-code","task":"t"}`, 2, 422, "run_quota", "runs.quota"},
		{"seed_auto_tools under hold rules", hold,
			`{"agent":"claude-code","task":"t","interactive":true,"seed_auto_tools":true}`, 0, 403, "governance_profile", "runs.seed_auto_tools"},
		{"codex-cli under hold rules", hold,
			`{"agent":"codex-cli","task":"t"}`, 0, 403, "governance_profile", "runs.agent"},
		{"autonomy ladder", ladder,
			`{"agent":"claude-code","task":"t"}`, 0, 403, "governance_profile", "runs.interactive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, audit := govEscapeFixture(t, assignedStore(tc.profile))
			srv.cfg.Store = &activeRunsStore{govEscapeStore: st, active: tc.active}
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-walled", []string{"eng"}, false), tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			got := decodeRefusal(t, w)
			if got.Reason != tc.wantReason || got.Error == "" {
				t.Errorf("body = %+v, want reason %q and the door's own sentence", got, tc.wantReason)
			}
			if got.Policy == nil || *got.Policy != want {
				t.Errorf("policy = %+v, want %+v", got.Policy, want)
			}
			if !strings.Contains(got.Error, `"`+name+`"`) {
				t.Errorf("sentence %q lost the profile name it has always carried", got.Error)
			}
			audit.mu.Lock()
			defer audit.mu.Unlock()
			for _, ev := range audit.events {
				if ev.Action != authz.AuditAction {
					continue
				}
				for _, secret := range []string{want.Owner, want.Email} {
					if strings.Contains(string(ev.Data), secret) {
						t.Errorf("authz.denied row carries %q: %s", secret, ev.Data)
					}
				}
			}
		})
	}

	t.Run("a profile with no contact still names itself and nothing more", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile(name, types.GovernanceLimits{DenyInteractive: true})))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-walled", []string{"eng"}, false), `{"agent":"claude-code"}`)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if got := string(raw["policy"]); got != `{"source":"profile","name":"walled"}` {
			t.Errorf("policy = %s", got)
		}
	})

	t.Run("an operator is never bound, so never refused with a policy", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(taggedProfile(name, tag, types.GovernanceLimits{DenyInteractive: true})))
		if got := (governanceCeiling{Operator: true, Profile: &ResolvedProfile{Name: hold.Name}}).policyRef(); got != nil {
			t.Errorf("operator policyRef = %+v, want nil", got)
		}
		if got := srv.ceilingPolicy(context.Background(), governanceCeiling{Operator: true}); got != nil {
			t.Errorf("operator ceilingPolicy = %+v, want nil", got)
		}
	})
}

// TestDriveDoorNamesThePolicy is the user-drive door, which refuses from a
// ceiling it is handed rather than from POST /runs.
func TestDriveDoorNamesThePolicy(t *testing.T) {
	p := taggedProfile("contractors", "alpha", types.GovernanceLimits{DenyUserDrive: true})
	ceiling := governanceCeiling{Profile: resolvedOf(p), Limits: p.Limits}
	srv, _ := driveRunServer(&driveStore{}, "docker")
	_, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), ceiling, driveMemberCtx([]string{"eng"}, false))
	if ok || w.Code != http.StatusForbidden {
		t.Fatalf("seed = ok %v, status %d, want the 403 door: %s", ok, w.Code, w.Body.String())
	}
	got := decodeRefusal(t, w)
	if want := profileRefFor("contractors", "alpha"); got.Policy == nil || *got.Policy != want {
		t.Errorf("policy = %+v, want %+v", got.Policy, want)
	}
	if want := driveDeniedByProfileMsg("contractors"); got.Error != want {
		t.Errorf("sentence = %q, want %q", got.Error, want)
	}
}

// TestLiveRunDoorsNameTheRunsPolicy: the attach and UI-gateway refusals name the
// profile the run was launched under, and GET /runs/{id} carries the same.
func TestLiveRunDoorsNameTheRunsPolicy(t *testing.T) {
	prof := taggedProfile("ci", "alpha", types.GovernanceLimits{DenyInteractive: true, DenyUIApps: true})
	want := profileRefFor("ci", "alpha")
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1",
		Task: "make test", GovernanceProfileID: &prof.ID}
	st := &profileListStore{profiles: []types.GovernanceProfile{*prof}, authzStore: newAuthzStore()}
	st.authzStore.runs[run.ID] = run
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	t.Run("attach", func(t *testing.T) {
		w := httptest.NewRecorder()
		if !srv.refuseInteractiveAttach(w, httptest.NewRequest(http.MethodGet, "/", nil), run) {
			t.Fatal("attach was not refused")
		}
		got := decodeRefusal(t, w)
		if got.Policy == nil || *got.Policy != want {
			t.Errorf("policy = %+v, want %+v", got.Policy, want)
		}
	})
	t.Run("ui apps", func(t *testing.T) {
		w := httptest.NewRecorder()
		if !srv.refuseUIAppsDenied(w, httptest.NewRequest(http.MethodGet, "/", nil), run) {
			t.Fatal("ui gateway was not refused")
		}
		got := decodeRefusal(t, w)
		if got.Policy == nil || *got.Policy != want {
			t.Errorf("policy = %+v, want %+v", got.Policy, want)
		}
	})
	t.Run("run detail", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser), "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET run = %d: %s", w.Code, w.Body.String())
		}
		var body struct {
			Policy *policyref.Ref `json:"policy"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Policy == nil || *body.Policy != want {
			t.Errorf("policy = %+v, want %+v", body.Policy, want)
		}
	})
	t.Run("a contactless leaf borrows site help at both doors", func(t *testing.T) {
		leaf := limitsProfile("leased", types.GovernanceLimits{DenyInteractive: true, DenyUIApps: true})
		lrun := types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1",
			Task: "make test", GovernanceProfileID: &leaf.ID}
		lst := &profileListStore{profiles: []types.GovernanceProfile{*leaf}, authzStore: newAuthzStore()}
		lst.authzStore.runs[lrun.ID] = lrun
		lst.authzStore.siteCfg.PolicyHelp = &policyref.Contact{RequestURL: "https://site.example.com/help"}
		lcfg := baseTestConfig(newHarness(t), lst)
		lcfg.OIDC = &oidc.Authenticator{}
		lsrv := New(lcfg)
		for name, door := range map[string]func(http.ResponseWriter, *http.Request, types.AgentRun) bool{
			"attach":  lsrv.refuseInteractiveAttach,
			"ui apps": lsrv.refuseUIAppsDenied,
		} {
			w := httptest.NewRecorder()
			if !door(w, httptest.NewRequest(http.MethodGet, "/", nil), lrun) {
				t.Fatalf("%s was not refused", name)
			}
			got := decodeRefusal(t, w)
			if got.Policy == nil || got.Policy.Name != "leased" || got.Policy.RequestURL != "https://site.example.com/help" {
				t.Errorf("%s policy = %+v, want leased with the site help", name, got.Policy)
			}
		}
	})
	t.Run("run detail with no profile falls back to policy_help, then to nothing", func(t *testing.T) {
		bare := types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunRunning}
		st.authzStore.runs[bare.ID] = bare
		get := func() map[string]json.RawMessage {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+bare.ID.String(), ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser), "")
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			return raw
		}
		if _, has := get()["policy"]; has {
			t.Error("a run under no profile on a deployment with no policy_help carries a policy")
		}
		st.authzStore.siteCfg.PolicyHelp = policyContactFor("deploy")
		var ref policyref.Ref
		if err := json.Unmarshal(get()["policy"], &ref); err != nil {
			t.Fatal(err)
		}
		if ref.Source != policyref.SourceDeployment || ref.Owner != "deploy Owner" || ref.Name != "" {
			t.Errorf("policy = %+v, want the deployment's policy_help", ref)
		}
	})
}

// TestRecordAndProviderSignInNameThePolicy: the two launch doors that answer
// outside refuse carry the ref in the error and put it on the wire.
func TestRecordAndProviderSignInNameThePolicy(t *testing.T) {
	want := profileRefFor("two-at-a-time", "alpha")
	prof := taggedProfile("two-at-a-time", "alpha", types.GovernanceLimits{MaxConcurrentRuns: 2})
	prof.Limits.DenyInteractive = true

	t.Run("the error keeps its sentinel and its bytes", func(t *testing.T) {
		inner := &ResolvedProfile{Name: "p"}
		err := recordCeilingError{err: errRecordCeilingLimit, ref: profilePolicyRef(inner)}
		if !strings.Contains(err.Error(), errRecordCeilingLimit.Error()) || err.Error() != errRecordCeilingLimit.Error() {
			t.Errorf("Error() = %q, want the wrapped value's own text", err.Error())
		}
		if r := recordCeilingRef(err); r == nil || r.Name != "p" {
			t.Errorf("recordCeilingRef = %+v", r)
		}
		if recordCeilingRef(errRecordCeilingLimit) != nil {
			t.Error("a bare sentinel carries a ref")
		}
	})

	t.Run("record mode launch", func(t *testing.T) {
		ws := &types.Workspace{ID: uuid.New(), Name: "ws", Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}}}
		srv := providerRunFixture(t, types.SiteConfig{}, assignedStore(prof), ws)
		srv.cfg.Store = &integQuotaStore{integStore: srv.cfg.Store.(*integStore), active: 2}
		code, body := recordDoor(t, srv, "", ws, true)
		if code != http.StatusForbidden {
			t.Fatalf("record = %d %s, want the 403 ceiling refusal", code, body)
		}
		var got refusalWire
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Reason != reasonRecordCeilingLimit || got.Policy == nil || *got.Policy != want {
			t.Errorf("body = %s, want reason %s and policy %+v", body, reasonRecordCeilingLimit, want)
		}
		if !strings.HasPrefix(got.Error, "interactive runs are not allowed by your governance profile") {
			t.Errorf("sentence = %q, want the create path's own sentence", got.Error)
		}
	})

	t.Run("record mode launch, contactless leaf borrows site help", func(t *testing.T) {
		leaf := limitsProfile("leased", types.GovernanceLimits{DenyInteractive: true})
		ws := &types.Workspace{ID: uuid.New(), Name: "ws", Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}}}
		site := types.SiteConfig{PolicyHelp: &policyref.Contact{RequestURL: "https://site.example.com/help"}}
		srv := providerRunFixture(t, site, assignedStore(leaf), ws)
		code, body := recordDoor(t, srv, "", ws, true)
		if code != http.StatusForbidden {
			t.Fatalf("record = %d %s, want the 403 ceiling refusal", code, body)
		}
		var got refusalWire
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Policy == nil || got.Policy.Name != "leased" || got.Policy.RequestURL != site.PolicyHelp.RequestURL {
			t.Errorf("policy = %+v, want leased with the site help", got.Policy)
		}
	})

	t.Run("provider sign-in launch", func(t *testing.T) {
		site := credentialSite(ssoProvider())
		srv, st, _, _ := signInFixture(t, assignedStore(taggedProfile("two-at-a-time", "alpha", types.GovernanceLimits{MaxConcurrentRuns: 1})), site)
		srv.cfg.Store = &signInQuotaStore{signInStore: st, active: 1}
		code, body := signIn(t, srv, govSession(t, "sub-walled", []string{"eng"}, false), site.ModelProviders.Providers[0].ID)
		if code != http.StatusForbidden {
			t.Fatalf("sign-in = %d %s, want the 403 ceiling refusal", code, body)
		}
		var got refusalWire
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Reason != reasonRecordCeilingLimit || got.Policy == nil || *got.Policy != want {
			t.Errorf("body = %s, want reason %s and policy %+v", body, reasonRecordCeilingLimit, want)
		}
	})
}

// integQuotaStore is integStore with the run count the quota reads.
type integQuotaStore struct {
	*integStore
	active int
}

func (s *integQuotaStore) CountActiveRunsBy(context.Context, string) (int, error) {
	return s.active, nil
}

// signInQuotaStore is signInStore with the run count the quota reads.
type signInQuotaStore struct {
	*signInStore
	active int
}

func (s *signInQuotaStore) CountActiveRunsBy(context.Context, string) (int, error) {
	return s.active, nil
}

// TestDeploymentArmReadsPolicyHelpOnlyForARefusal: a member no profile binds is
// named as the deployment; the site's policy_help is read when a refusal is
// written and never by the no-store-read policyRef /me serves.
func TestDeploymentArmReadsPolicyHelpOnlyForARefusal(t *testing.T) {
	cs := &capStore{site: types.SiteConfig{PolicyHelp: policyContactFor("deploy")}}
	srv := govServer(cs)
	bound := governanceCeiling{}

	if got := bound.policyRef(); got == nil || *got != (policyref.Ref{Source: policyref.SourceDeployment}) {
		t.Errorf("policyRef = %+v, want the bare deployment arm", got)
	}
	got := srv.ceilingPolicy(context.Background(), bound)
	if got == nil || got.Source != policyref.SourceDeployment || got.Owner != "deploy Owner" || got.RequestURL != "https://deploy.example/request" {
		t.Errorf("ceilingPolicy = %+v, want the deployment with its policy_help", got)
	}
	if got.Name != "" {
		t.Errorf("deployment ref names %q", got.Name)
	}

	// A site config that cannot be read leaves the bare arm, not a failure.
	srv.cfg.Store = nil
	if got := srv.ceilingPolicy(context.Background(), bound); got == nil || got.Owner != "" {
		t.Errorf("ceilingPolicy without a store = %+v, want the bare deployment arm", got)
	}
}

// TestContactlessLeafBorrowsSiteHelp: a leaf that publishes no contact keeps its
// name and borrows the site's policy_help on all three projections that carry a
// ref; a leaf with its own contact never inherits site help; a site without
// help leaves the bare profile ref.
func TestContactlessLeafBorrowsSiteHelp(t *testing.T) {
	help := &policyref.Contact{RequestURL: "https://site.example.com/help"}
	run := func(p *types.GovernanceProfile, site types.SiteConfig) (*Server, types.AgentRun, map[string]*policyref.Ref) {
		srv, st := runLimitsFixture(t, assignedStore(p))
		st.siteConfig = site
		srv.cfg.Store = &attrStore{govEscapeStore: st, profiles: []types.GovernanceProfile{*p}}
		r := types.AgentRun{GovernanceProfileID: &p.ID}
		ctx := context.Background()
		return srv, r, map[string]*policyref.Ref{
			"ceiling refusal": srv.ceilingPolicy(ctx, governanceCeiling{Profile: resolvedOf(p)}),
			"run detail":      srv.runPolicyRef(ctx, r),
			"dispatch":        srv.runAttribution(ctx, r, site),
		}
	}

	t.Run("no contact borrows site help", func(t *testing.T) {
		_, _, refs := run(limitsProfile("leased", types.GovernanceLimits{}), types.SiteConfig{PolicyHelp: help})
		want := policyref.Ref{Source: policyref.SourceProfile, Name: "leased", RequestURL: help.RequestURL}
		for door, ref := range refs {
			if ref == nil || *ref != want {
				t.Errorf("%s = %+v, want %+v", door, ref, want)
			}
		}
	})
	t.Run("own contact never inherits site help", func(t *testing.T) {
		want := profileRefFor("ci", "alpha")
		_, _, refs := run(taggedProfile("ci", "alpha", types.GovernanceLimits{}), types.SiteConfig{PolicyHelp: help})
		for door, ref := range refs {
			if ref == nil || *ref != want {
				t.Errorf("%s = %+v, want %+v", door, ref, want)
			}
		}
	})
	t.Run("site without help leaves the bare profile ref", func(t *testing.T) {
		want := policyref.Ref{Source: policyref.SourceProfile, Name: "leased"}
		_, _, refs := run(limitsProfile("leased", types.GovernanceLimits{}), types.SiteConfig{})
		for door, ref := range refs {
			if ref == nil || *ref != want {
				t.Errorf("%s = %+v, want %+v", door, ref, want)
			}
		}
	})
	t.Run("/me adds no read and stays the bare profile ref", func(t *testing.T) {
		got := governanceCeiling{Profile: resolvedOf(limitsProfile("leased", types.GovernanceLimits{}))}.policyRef()
		if want := (policyref.Ref{Source: policyref.SourceProfile, Name: "leased"}); got == nil || *got != want {
			t.Errorf("policyRef = %+v, want %+v", got, want)
		}
	})
}

// TestRefuseLeavesPolicyOffHiddenAndRewrittenDecisions pins the parity law: a
// hidden door answers byte-for-byte what a missing resource does, so attaching a
// policy to it must change nothing, and a decision a door rewrote for the wire
// is not a ceiling refusal at all.
func TestRefuseLeavesPolicyOffHiddenAndRewrittenDecisions(t *testing.T) {
	srv := New(baseTestConfig(newHarness(t), nil))
	ref := &policyref.Ref{Source: policyref.SourceProfile, Name: "alpha-profile", Owner: "Alpha Owner",
		Email: "alpha@alpha.example", RequestURL: "https://alpha.example/request"}
	emit := func(d authz.Decision) (int, string) {
		w := httptest.NewRecorder()
		srv.refuse(w, httptest.NewRequest(http.MethodGet, "/api/v1/runs/x", nil), d)
		return w.Code, w.Body.String()
	}
	for _, tc := range []struct {
		name string
		d    authz.Decision
	}{
		{"a hidden decision", authz.Deny(authz.ReasonNotOwner, "run-1", "run not found")},
		{"a decision with a WireReason", authz.Deny(authz.ReasonNotOwner, "run-1", "run not found").AsIf(authz.Reason(reasonRunNotFound))},
		{"a non-hidden decision with a WireReason", authz.Deny(authz.ReasonGovernanceProfile, "runs.attach", "x").AsIf(authz.Reason(reasonRunNotFound))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantCode, want := emit(tc.d)
			gotCode, got := emit(tc.d.WithPolicy(ref))
			if gotCode != wantCode || got != want {
				t.Errorf("with a policy: %d %q\nwithout:       %d %q", gotCode, got, wantCode, want)
			}
			if strings.Contains(got, `"policy"`) || strings.Contains(got, "alpha") {
				t.Errorf("body carries the policy: %s", got)
			}
		})
	}
	t.Run("a ceiling decision does carry it", func(t *testing.T) {
		_, got := emit(authz.Deny(authz.ReasonGovernanceProfile, "runs.attach", "x").WithPolicy(ref))
		if !strings.Contains(got, `"policy":{"source":"profile","name":"alpha-profile"`) {
			t.Errorf("body = %s, want the policy", got)
		}
	})
}
