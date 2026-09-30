// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The governance limits that bind a run after it exists: deny_ui_apps at the
// create clamp and the UI gateway (#1391), deny_interactive at the terminal
// attach and the SSH gateway (#1392).

// #1391: deny_ui_apps at create

func TestDenyUIAppsStripsAtCreate(t *testing.T) {
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-walled", []string{"eng"}, false) }
	const inlineWithApp = `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com"],"ui_apps":[{"name":"code","port":8080}]}}`

	t.Run("an inline ui_app is stripped, warned and audited", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assignedStore(limitsProfile("no-ui", types.GovernanceLimits{DenyUIApps: true})))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member(t), inlineWithApp)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
		}
		var body struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode 201: %v", err)
		}
		const want = `dropped 1 ui_app(s) denied by governance profile "no-ui": code:8080`
		if !slices.Contains(body.Warnings, want) {
			t.Errorf("warnings = %q, want %q", body.Warnings, want)
		}
		st.mu.Lock()
		var runID uuid.UUID
		for id := range st.runs {
			runID = id
		}
		st.mu.Unlock()
		ev := waitForRecAudit(t, audit, runID, "run.policy.resolve", "success")
		var spec types.RunPolicySpec
		if err := json.Unmarshal(ev.Data, &spec); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if len(spec.UIApps) != 0 {
			t.Errorf("run ui_apps = %v, want none under deny_ui_apps", spec.UIApps)
		}
		if !recHasRefusal(audit, authz.ReasonGovernanceProfile, "runs.ui_apps") {
			t.Error("no authz.denied governance_profile row for runs.ui_apps")
		}
	})

	t.Run("the profile's own ceiling ui_apps do not reach a run either", func(t *testing.T) {
		p := limitsProfile("no-ui", types.GovernanceLimits{DenyUIApps: true})
		p.Ceiling.UIApps = []types.UIApp{{Name: "code", Port: 8080}}
		srv, st, audit := govEscapeFixture(t, assignedStore(p))
		got := govCreateAndDispatch(t, srv, st, audit, member(t), `{"agent":"claude-code","task":"t"}`)
		if len(got.UIApps) != 0 {
			t.Errorf("run ui_apps = %v, want none under deny_ui_apps", got.UIApps)
		}
	})

	t.Run("a profile without the limit keeps today's behaviour", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assignedStore(limitsProfile("open", types.GovernanceLimits{})))
		got := govCreateAndDispatch(t, srv, st, audit, member(t), inlineWithApp)
		if len(got.UIApps) != 1 {
			t.Errorf("run ui_apps = %v, want the member's app: an empty ceiling ui_apps is still no opinion", got.UIApps)
		}
	})

	t.Run("a super admin is not bound", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assignedStore(limitsProfile("no-ui", types.GovernanceLimits{DenyUIApps: true})))
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, inlineWithApp)
		if w.Code != http.StatusCreated {
			t.Fatalf("admin create = %d: %s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		var runID uuid.UUID
		for id := range st.runs {
			runID = id
		}
		st.mu.Unlock()
		ev := waitForRecAudit(t, audit, runID, "run.policy.resolve", "success")
		var spec types.RunPolicySpec
		if err := json.Unmarshal(ev.Data, &spec); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if len(spec.UIApps) != 1 {
			t.Errorf("admin run ui_apps = %v, want kept", spec.UIApps)
		}
	})
}

func recHasRefusal(r *recRecorder, reason authz.Reason, target string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Action != authz.AuditAction || ev.Target != target {
			continue
		}
		var data map[string]any
		if json.Unmarshal(ev.Data, &data) == nil && data["reason"] == string(reason) {
			return true
		}
	}
	return false
}

// #1391: deny_ui_apps at the UI gateway

func TestDenyUIAppsRefusesAUIGatewaySession(t *testing.T) {
	profile := types.GovernanceProfile{ID: uuid.New(), Name: "no-ui", Limits: types.GovernanceLimits{DenyUIApps: true}}
	setup := func(t *testing.T, p types.GovernanceProfile) *uiHarness {
		h := newUIHarness(t, okBackend())
		h.store.profiles = []types.GovernanceProfile{p}
		h.run.GovernanceProfileID = &p.ID
		h.store.putRun(h.run)
		return h
	}
	enter := func(h *uiHarness, principal, role string) *httptest.ResponseRecorder {
		return h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, principal, role)}})
	}

	t.Run("the owner is refused", func(t *testing.T) {
		h := setup(t, profile)
		rec := enter(h, h.owner, oidc.RoleUser)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("enter = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		const want = `UI apps are not allowed under the governance profile "no-ui" this run was launched under.`
		if body.Reason != string(authz.ReasonGovernanceProfile) || body.Error != want {
			t.Errorf("body = %+v, want reason governance_profile and %q", body, want)
		}
		if !h.audit.hasDataValue("reason", string(authz.ReasonGovernanceProfile)) {
			t.Errorf("no governance_profile refusal audited; reasons: %s", h.audit.dataReasons())
		}
	})

	t.Run("a super admin is not bound", func(t *testing.T) {
		h := setup(t, profile)
		if rec := enter(h, "root", oidc.RoleAdmin); rec.Code != http.StatusFound {
			t.Fatalf("admin enter = %d, want 302: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a security admin is bound", func(t *testing.T) {
		// The mint stamps a security admin as a plain user (attach_ticket.go), so
		// seeding security_admin pins the door's own super-admin test against a
		// widened predicate rather than tracing the real mint.
		h := setup(t, profile)
		if rec := enter(h, h.owner, oidc.RoleSecurityAdmin); rec.Code != http.StatusForbidden {
			t.Fatalf("security admin enter = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if !h.audit.hasDataValue("reason", string(authz.ReasonGovernanceProfile)) {
			t.Errorf("no governance_profile refusal audited; reasons: %s", h.audit.dataReasons())
		}
	})

	t.Run("a profile without the limit is not bound", func(t *testing.T) {
		h := setup(t, types.GovernanceProfile{ID: uuid.New(), Name: "open"})
		if rec := enter(h, h.owner, oidc.RoleUser); rec.Code != http.StatusFound {
			t.Fatalf("enter = %d, want 302: %s", rec.Code, rec.Body.String())
		}
	})
}

// #1392: deny_interactive at the terminal attach

// profileListStore serves ListGovernanceProfiles over authzStore, whose own
// answer is always empty.
type profileListStore struct {
	*authzStore
	profiles []types.GovernanceProfile
	err      error
}

func (s *profileListStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return s.profiles, s.err
}

func TestDenyInteractiveRefusesTheTerminalAttach(t *testing.T) {
	profile := types.GovernanceProfile{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
	setup := func(t *testing.T, run types.AgentRun, st *profileListStore) (*Server, *harness) {
		h := newHarness(t)
		st.authzStore = newAuthzStore()
		st.authzStore.runs[run.ID] = run
		cfg := baseTestConfig(h, st)
		cfg.Runner = &fakeRunner{}
		return New(cfg), h
	}
	execRun := func(profileID *uuid.UUID, task string) types.AgentRun {
		return types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1",
			Task: task, GovernanceProfileID: profileID}
	}
	attach := func(t *testing.T, srv *Server, st *profileListStore, run types.AgentRun, principal, role string) *httptest.ResponseRecorder {
		tok, err := mintAttachTicket(context.Background(), st, run.ID, types.ActorHuman, principal, role, time.Now())
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, "", "")
	}
	refusedByProfile := func(w *httptest.ResponseRecorder) bool {
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code == http.StatusForbidden && body.Reason == string(authz.ReasonGovernanceProfile)
	}

	t.Run("the owner of an exec run under the profile is refused", func(t *testing.T) {
		run := execRun(&profile.ID, "make test")
		st := &profileListStore{profiles: []types.GovernanceProfile{profile}}
		srv, h := setup(t, run, st)
		w := attach(t, srv, st, run, "alice", oidc.RoleUser)
		if !refusedByProfile(w) {
			t.Fatalf("attach = %d %s, want 403 governance_profile", w.Code, w.Body.String())
		}
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		const want = `attaching is not allowed under the governance profile "ci" this run was launched under: it denies interactive sessions.`
		if body.Error != want {
			t.Errorf("body = %q, want %q", body.Error, want)
		}
		if !recHasRefusal(h.audit, authz.ReasonGovernanceProfile, "runs.attach") {
			t.Error("no authz.denied governance_profile row for runs.attach")
		}
	})

	t.Run("a security admin who owns the run is bound", func(t *testing.T) {
		// The mint stamps a security admin as a plain user (attach_ticket.go), so
		// seeding security_admin pins the door's own super-admin test against a
		// widened predicate rather than tracing the real mint.
		run := execRun(&profile.ID, "make test")
		st := &profileListStore{profiles: []types.GovernanceProfile{profile}}
		srv, _ := setup(t, run, st)
		if w := attach(t, srv, st, run, "alice", oidc.RoleSecurityAdmin); !refusedByProfile(w) {
			t.Fatalf("attach = %d %s, want 403 governance_profile", w.Code, w.Body.String())
		}
	})

	t.Run("a profile read failure refuses rather than attaches", func(t *testing.T) {
		run := execRun(&profile.ID, "make test")
		st := &profileListStore{err: errors.New("conn closed by peer")}
		srv, _ := setup(t, run, st)
		if w := attach(t, srv, st, run, "alice", oidc.RoleUser); w.Code != http.StatusInternalServerError {
			t.Fatalf("attach = %d %s, want 500", w.Code, w.Body.String())
		}
	})

	for _, tc := range []struct {
		name            string
		run             types.AgentRun
		principal, role string
		profiles        []types.GovernanceProfile
	}{
		{"a super admin is not bound", execRun(&profile.ID, "make test"), "root", oidc.RoleAdmin, []types.GovernanceProfile{profile}},
		{"the harness sign-in run is exempt", execRun(&profile.ID, harnessLoginTask), "alice", oidc.RoleUser, []types.GovernanceProfile{profile}},
		{"a run under no profile is not bound", execRun(nil, "make test"), "alice", oidc.RoleUser, nil},
		{"a deleted profile binds nothing", execRun(&profile.ID, "make test"), "alice", oidc.RoleUser, nil},
		{"a profile without the limit is not bound", execRun(&profile.ID, "make test"), "alice", oidc.RoleUser,
			[]types.GovernanceProfile{{ID: profile.ID, Name: "ci"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &profileListStore{profiles: tc.profiles}
			srv, _ := setup(t, tc.run, st)
			// Past the profile door the plain GET (no WebSocket handshake) is
			// answered 426, so a door that wrongly 500s fails here too.
			w := attach(t, srv, st, tc.run, tc.principal, tc.role)
			if refusedByProfile(w) {
				t.Fatalf("attach refused by the profile: %s", w.Body.String())
			}
			if w.Code != http.StatusUpgradeRequired {
				t.Fatalf("attach = %d %s, want 426 Upgrade Required", w.Code, w.Body.String())
			}
		})
	}
}

// #1392: deny_interactive at the SSH gateway

func TestDenyInteractiveRefusesSSH(t *testing.T) {
	profile := types.GovernanceProfile{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
	open := types.GovernanceProfile{ID: uuid.New(), Name: "open"}
	st := newSSHMemStore()
	st.profiles = []types.GovernanceProfile{profile, open}
	deniedRun, openRun := uuid.New(), uuid.New()
	st.putRun(types.AgentRun{ID: deniedRun, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1", GovernanceProfileID: &profile.ID})
	st.putRun(types.AgentRun{ID: openRun, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-2", GovernanceProfileID: &open.ID})

	now := time.Now()
	alicePriv, alicePub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(alicePub), Principal: "alice@example.com",
		PublicKey: string(ssh.MarshalAuthorizedKey(alicePub))})
	adminPriv, adminPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(adminPub), Principal: "root@example.com",
		PublicKey: string(ssh.MarshalAuthorizedKey(adminPub)), Role: oidc.RoleAdmin, RoleCheckedAt: &now})
	secPriv, secPub := mustSSHKeypair(t)
	secRun := uuid.New() // its own run, so the audit row below can only be this key's
	st.putRun(types.AgentRun{ID: secRun, CreatedBy: "sec@example.com", State: types.RunRunning, SandboxRef: "sbx-3", GovernanceProfileID: &profile.ID})
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(secPub), Principal: "sec@example.com",
		PublicKey: string(ssh.MarshalAuthorizedKey(secPub)), Role: oidc.RoleSecurityAdmin, RoleCheckedAt: &now})
	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	t.Run("the owner is refused, with the profile named in the audit", func(t *testing.T) {
		if _, err := sshDial(t, h, deniedRun.String(), alicePriv); err == nil {
			t.Fatal("owner ssh into a deny_interactive run succeeded, want refused")
		}
		ev := waitForAudit(t, h.audit, deniedRun, "ssh.authenticate", "failure")
		if ev == nil {
			t.Fatalf("no ssh.authenticate failure; events=%s", auditDump(h.audit.snapshot(), deniedRun))
		}
		if !strings.Contains(string(ev.Data), `governance profile \"ci\" denies interactive sessions`) {
			t.Errorf("audit data = %s, want the profile named", ev.Data)
		}
	})

	t.Run("a super admin is not bound", func(t *testing.T) {
		client, err := sshDial(t, h, deniedRun.String(), adminPriv)
		if err != nil {
			t.Fatalf("admin ssh refused: %v", err)
		}
		_ = client.Close()
	})

	t.Run("a security admin is bound", func(t *testing.T) {
		// A real key is stamped as a plain user (TestSSHKeyNeverStampsSecurityAdmin),
		// so seeding security_admin pins the door's own super-admin test against a
		// widened predicate rather than tracing the real registration.
		if _, err := sshDial(t, h, secRun.String(), secPriv); err == nil {
			t.Fatal("security admin ssh into a deny_interactive run succeeded, want refused")
		}
		ev := waitForAudit(t, h.audit, secRun, "ssh.authenticate", "failure")
		if ev == nil || !strings.Contains(string(ev.Data), `governance profile \"ci\" denies interactive sessions`) {
			t.Fatalf("audit = %v, want the profile named; events=%s", ev, auditDump(h.audit.snapshot(), secRun))
		}
	})

	t.Run("a profile without the limit is not bound", func(t *testing.T) {
		client, err := sshDial(t, h, openRun.String(), alicePriv)
		if err != nil {
			t.Fatalf("owner ssh refused: %v", err)
		}
		_ = client.Close()
	})
}

// TestDenyInteractiveSSHFailsClosedOnAProfileReadError: when the run's profile
// cannot be read the dial is refused, not let through, and the audit row says
// why.
func TestDenyInteractiveSSHFailsClosedOnAProfileReadError(t *testing.T) {
	profile := types.GovernanceProfile{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
	st := newSSHMemStore()
	st.profiles = []types.GovernanceProfile{profile}
	st.profilesErr = errors.New("conn closed by peer")
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1", GovernanceProfileID: &profile.ID})
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com",
		PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	if _, err := sshDial(t, h, runID.String(), priv); err == nil {
		t.Fatal("ssh with an unreadable governance profile succeeded, want refused")
	}
	ev := waitForAudit(t, h.audit, runID, "ssh.authenticate", "failure")
	if ev == nil {
		t.Fatalf("no ssh.authenticate failure; events=%s", auditDump(h.audit.snapshot(), runID))
	}
	if !strings.Contains(string(ev.Data), "governance profile unreadable") {
		t.Errorf("audit data = %s, want the unreadable profile named", ev.Data)
	}
}
