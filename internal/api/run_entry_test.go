// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Interactive entry needs the run's owner (#1476). Each test below walks one
// entry point with the same cast: the owner, a super admin who is not the
// owner, a super admin on a run with no personal owner, and the security tier.

const (
	entryOwnerSub  = "sub-owner"
	entryOwnerMail = "owner@corp.example"
	entryRefusal   = `"error":"only the person who started this run can open it interactively","reason":"run_owner_only"`
)

func TestMayEnterRun(t *testing.T) {
	person := types.AgentRun{CreatedBy: "alice"}
	service := types.AgentRun{CreatedBy: "admin-token", OperatorOwned: true}
	for _, tc := range []struct {
		name      string
		run       types.AgentRun
		principal string
		super     bool
		want      bool
	}{
		{"the owner", person, "alice", false, true},
		{"the owner who is also a super admin", person, "alice", true, true},
		{"a super admin on a person's run", person, "root", true, false},
		{"a member on a person's run", person, "mallory", false, false},
		{"a super admin on an operator-owned run", service, "root", true, true},
		{"a member on an operator-owned run", service, "mallory", false, false},
		{"the admin token on its own run", service, "admin-token", true, true},
		{"the admin token on a person's run", person, "admin-token", true, false},
		{"no principal never matches an empty owner", types.AgentRun{}, "", false, false},
		{"an operator-owned run is not a personal one for a non-admin", types.AgentRun{OperatorOwned: true}, "", false, false},
	} {
		if got := mayEnterRun(tc.run, tc.principal, tc.super); got != tc.want {
			t.Errorf("%s: mayEnterRun = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// entryServer is holderTestServer with a person-owned run (the owner's sub), or
// an operator-owned one, plus the sessions the cast signs in with.
func entryServer(t *testing.T, operatorOwned bool) (*Server, types.AgentRun, *sshTestRecorder, map[string]*http.Cookie) {
	t.Helper()
	srv, st, _, rec, run := holderTestServer(t)
	st.mu.Lock()
	run.CreatedBy, run.OperatorOwned = entryOwnerSub, operatorOwned
	st.runs[run.ID] = run
	st.mu.Unlock()
	return srv, run, rec, map[string]*http.Cookie{
		"owner":    ssoSession(t, entryOwnerSub, entryOwnerMail, oidc.RoleUser),
		"admin":    ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin),
		"security": ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin),
	}
}

func refusedOwnerOnly(code int, body string) bool {
	return code == http.StatusForbidden && strings.Contains(body, entryRefusal)
}

// TestEntry_Mint: ticket mint admits the owner, refuses a super admin on a
// person's run with the 403 and the approved body, keeps the 404 for the
// security tier, and admits a super admin on a run with no personal owner.
func TestEntry_Mint(t *testing.T) {
	srv, run, rec, ses := entryServer(t, false)
	path := "/api/v1/runs/" + run.ID.String() + "/attach-ticket"

	if w := doSSO(t, srv, http.MethodPost, path, ses["owner"], ""); w.Code != http.StatusOK {
		t.Fatalf("the owner: %d %s, want 200", w.Code, w.Body.String())
	}
	w := doSSO(t, srv, http.MethodPost, path, ses["admin"], "")
	if !refusedOwnerOnly(w.Code, w.Body.String()) {
		t.Fatalf("a super admin on a person's run: %d %s, want the 403 run_owner_only body", w.Code, w.Body.String())
	}
	if ev := findAudit(rec.snapshot(), run.ID, "authz.denied", "denied"); ev == nil || !strings.Contains(string(ev.Data), "run_owner_only") {
		t.Fatalf("the refusal was not audited as run_owner_only: %+v", ev)
	}
	if w := doSSO(t, srv, http.MethodPost, path, ses["security"], ""); w.Code != http.StatusNotFound {
		t.Fatalf("the security tier: %d %s, want the unchanged 404", w.Code, w.Body.String())
	}
	// The admin token is one trust seat: refused on a person's run.
	if w := do(t, srv, http.MethodPost, path, adminToken, ""); w.Code != http.StatusForbidden {
		t.Fatalf("the admin token on a person's run: %d %s, want 403", w.Code, w.Body.String())
	}

	srv, run, _, ses = entryServer(t, true)
	path = "/api/v1/runs/" + run.ID.String() + "/attach-ticket"
	if w := doSSO(t, srv, http.MethodPost, path, ses["admin"], ""); w.Code != http.StatusOK {
		t.Fatalf("a super admin on an operator-owned run: %d %s, want 200", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodPost, path, adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("the admin token on an operator-owned run: %d %s, want 200", w.Code, w.Body.String())
	}
}

// TestEntry_AdminTokenOnItsOwnRun: a run the admin token created is its own.
func TestEntry_AdminTokenOnItsOwnRun(t *testing.T) {
	srv, st, _, _, run := holderTestServer(t)
	st.mu.Lock()
	run.CreatedBy, run.OperatorOwned = "admin-token", true
	st.runs[run.ID] = run
	st.mu.Unlock()
	if w := do(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach-ticket", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("the admin token on its own run: %d %s, want 200", w.Code, w.Body.String())
	}
}

// TestEntry_MintRefusesATicketAfterALateRevoke: a revoke that lands between
// admission and the store write leaves no ticket behind, and an unanswerable
// check fails closed.
func TestEntry_MintRefusesATicketAfterALateRevoke(t *testing.T) {
	srv, run, _, ses := entryServer(t, false)
	rev := newCutoffRevocations()
	srv.cfg.SessionRevocations = rev
	path := "/api/v1/runs/" + run.ID.String() + "/attach-ticket"
	if w := doSSO(t, srv, http.MethodPost, path, ses["owner"], ""); w.Code != http.StatusOK {
		t.Fatalf("control: %d %s", w.Code, w.Body.String())
	}
	rev.err = context.DeadlineExceeded
	if w := doSSO(t, srv, http.MethodPost, path, ses["owner"], ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("an unanswerable revocation check: %d %s, want it to fail closed", w.Code, w.Body.String())
	}
}

// TestEntry_PortalOfASuperAdminCannotMintForAnotherRun: a delegated token is
// never a super admin, whoever the person behind it is.
func TestEntry_PortalOfASuperAdminCannotMintForAnotherRun(t *testing.T) {
	h := newUIHarness(t, okBackend())
	ds := newFakeDelegateStore()
	h.srv.cfg.Store = &uiDelegateStore{h.store, ds}
	raw, _ := seedDelegation(t, ds, "root") // the person behind it is an admin; the run is alice's
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/"+h.run.ID.String()+"/attach-ticket", raw, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("a portal minting for another person's run: %d %s, want 404", w.Code, w.Body.String())
	}
	own, _ := seedDelegation(t, ds, h.owner)
	if w := do(t, h.srv, http.MethodPost, "/api/v1/runs/"+h.run.ID.String()+"/attach-ticket", own, ""); w.Code != http.StatusOK {
		t.Fatalf("control, a portal minting for its own person's run: %d %s", w.Code, w.Body.String())
	}
}

// TestEntry_AttachTicketConsume: the WebSocket's ticket lane refuses an admin
// stamp on a person's run (the 403 and a session.attach failure naming it),
// admits the owner's, and admits an admin stamp on an operator-owned run.
// Past the entry check a plain GET is answered 426 Upgrade Required.
func TestEntry_AttachTicketConsume(t *testing.T) {
	srv, run, rec, _ := entryServer(t, false)
	attach := func(srv *Server, run types.AgentRun, principal, role string) (int, string) {
		tok, err := mintAttachTicket(context.Background(), srv.cfg.Store, run.ID, types.ActorHuman, principal, role, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, "", "")
		return w.Code, w.Body.String()
	}
	if code, body := attach(srv, run, "root", oidc.RoleAdmin); code != http.StatusForbidden || !strings.Contains(body, entryRefusal) {
		t.Fatalf("an admin ticket on a person's run: %d %s, want the 403 run_owner_only body", code, body)
	}
	failed := false
	for _, ev := range rec.snapshot() {
		if ev.Action == "session.attach" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), "run_owner_only") {
			failed = true
		}
	}
	if !failed {
		t.Fatal("the refusal left no session.attach failure row naming run_owner_only")
	}
	if code, body := attach(srv, run, entryOwnerSub, oidc.RoleUser); code != http.StatusUpgradeRequired {
		t.Fatalf("the owner's ticket: %d %s, want past the entry check (426)", code, body)
	}
	if code, body := attach(srv, run, entryOwnerSub, oidc.RoleAdmin); code != http.StatusUpgradeRequired {
		t.Fatalf("the owner's own admin ticket: %d %s, want past the entry check (426)", code, body)
	}
	if code, body := attach(srv, run, "mallory", oidc.RoleUser); code != http.StatusForbidden || strings.Contains(body, "run_owner_only") {
		t.Fatalf("a member's ticket: %d %s, want the unchanged not-your-run 403", code, body)
	}
	srv2, run2, _, _ := entryServer(t, true)
	if code, body := attach(srv2, run2, "root", oidc.RoleAdmin); code != http.StatusUpgradeRequired {
		t.Fatalf("an admin ticket on an operator-owned run: %d %s, want 426", code, body)
	}
}

// TestEntry_CookieAttach: the cookie lane keeps requireOperator, so a member
// stays out, and now also refuses a super admin on a person's run, auditing
// session.attach with lane cookie.
func TestEntry_CookieAttach(t *testing.T) {
	srv, run, rec, ses := entryServer(t, false)
	path := "/api/v1/runs/" + run.ID.String() + "/attach"
	w := doSSO(t, srv, http.MethodGet, path, ses["admin"], "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), entryRefusal) {
		t.Fatalf("a super admin's cookie on a person's run: %d %s, want the 403 run_owner_only body", w.Code, w.Body.String())
	}
	found := false
	for _, ev := range rec.snapshot() {
		if ev.Action == "session.attach" && ev.Outcome == "failure" &&
			strings.Contains(string(ev.Data), `"lane":"cookie"`) && strings.Contains(string(ev.Data), "run_owner_only") {
			found = true
		}
	}
	if !found {
		t.Fatal("no session.attach failure with lane cookie")
	}
	if w := doSSO(t, srv, http.MethodGet, path, ses["owner"], ""); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "run_owner_only") {
		t.Fatalf("a member's cookie: %d %s, want requireOperator's own refusal (members gain no cookie lane)", w.Code, w.Body.String())
	}
	// The admin who owns the run, and any admin on an operator-owned run, pass
	// the entry check; a plain GET is then answered 426.
	own := ssoSession(t, entryOwnerSub, entryOwnerMail, oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodGet, path, own, ""); w.Code != http.StatusUpgradeRequired {
		t.Fatalf("an admin on their own run: %d %s, want 426", w.Code, w.Body.String())
	}
	srv2, run2, _, ses2 := entryServer(t, true)
	if w := doSSO(t, srv2, http.MethodGet, "/api/v1/runs/"+run2.ID.String()+"/attach", ses2["admin"], ""); w.Code != http.StatusUpgradeRequired {
		t.Fatalf("an admin on an operator-owned run: %d %s, want 426", w.Code, w.Body.String())
	}
	// An admin-token bearer is refused on a person's run, as everywhere else.
	if w := do(t, srv, http.MethodGet, path, adminToken, ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), entryRefusal) {
		t.Fatalf("the admin token on a person's run: %d %s, want 403 run_owner_only", w.Code, w.Body.String())
	}
}

// TestEntry_UIReassert: an admin relay cookie minted before the entry rule
// narrowed dies at its next re-assert on a person's run, and lives on an
// operator-owned one.
func TestEntry_UIReassert(t *testing.T) {
	h := newUIHarness(t, closingBackend("sandbox app"))
	now := h.clock.now()
	cookie := &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{
		Run: h.run.ID, App: "code", Port: uiTestPort, Principal: "root", Role: oidc.RoleAdmin,
		Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix(),
	})}
	path := uiRelayPrefix(h.run.ID, "code") + "/"
	if r := uiGet(h, path, cookie); r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), uiSessionNoLongerAuthorizedMsg) {
		t.Fatalf("an admin cookie on a person's run: %d %s, want the not-authorized 403", r.Code, r.Body.String())
	}
	if !h.audit.hasDataValue("reason", uiDeniedReasonNotAuthorized) {
		t.Fatalf("no not_authorized denial; reasons: %s", h.audit.dataReasons())
	}
	owned := h.run
	owned.OperatorOwned = true
	h.store.putRun(owned)
	if r := uiGet(h, path, cookie); r.Code != http.StatusOK {
		t.Fatalf("an admin cookie on an operator-owned run: %d %s, want 200", r.Code, r.Body.String())
	}
}

// TestEntry_SSHLiveRecheck: an admin connection admitted on an operator-owned
// run is refused its next channel once the run is a person's, and an admin key
// on a person's run is refused at connect with the reason in the audit.
func TestEntry_SSHLiveRecheck(t *testing.T) {
	st := newSSHMemStore()
	svc := types.AgentRun{ID: uuid.New(), CreatedBy: "admin-token", OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-svc"}
	st.putRun(svc)
	now := time.Now()
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "root@example.com", CreatedAt: now,
		PublicKey: string(ssh.MarshalAuthorizedKey(pub)), Role: oidc.RoleAdmin, RoleCheckedAt: &now})
	h := newSSHTestHarness(t, st, &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil }})

	client, err := sshDial(t, h, svc.ID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	checkSSHNewChannels(t, client, true)
	person := svc
	person.OperatorOwned = false
	st.putRun(person)
	checkSSHNewChannels(t, client, false)
}

// TestEntry_TakeoverRefusalBody: a super admin's take-over of a person's run is
// the 403 with the approved body (the predicate table is
// TestAttachTakeover_OwnerOrSuperAdminOnly).
func TestEntry_TakeoverRefusalBody(t *testing.T) {
	srv, run, _, ses := entryServer(t, false)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", ses["admin"], "")
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusForbidden ||
		body.Reason != "run_owner_only" || body.Error != "only the person who started this run can open it interactively" {
		t.Fatalf("take-over by a super admin: %d %s, want the 403 run_owner_only body", w.Code, w.Body.String())
	}
}

// TestEntry_SuperAdminKeepsGovernance: the super admin loses the shell, not
// the run. A person's run is still readable and killable by them; revive and
// resume are owner-or-super-admin and do not move (the route matrix pins them).
func TestEntry_SuperAdminKeepsGovernance(t *testing.T) {
	srv, run, _, ses := entryServer(t, false)
	base := "/api/v1/runs/" + run.ID.String()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodGet, base + "/grants"},
		{http.MethodGet, base + "/policy"},
		{http.MethodPost, base + "/kill"},
		{http.MethodPost, base + "/resume"},
		{http.MethodPost, base + "/revive"},
	} {
		w := doSSO(t, srv, tc.method, tc.path, ses["admin"], "")
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
			t.Errorf("%s %s as a super admin: %d %s, want the handler's own answer", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/audit", ses["admin"], ""); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Errorf("audit read as a super admin: %d, want reachable", w.Code)
	}
}
