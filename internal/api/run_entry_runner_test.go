// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run placed on a registered runner is entered through the same doors as any
// other run, and every door applies mayEnterRun (run_entry.go) before the org
// opens a stream to the runner. These tests walk each door on a runner-placed
// run, with a Runner that records every stream the org opens toward it.

// runnerPlaced is run as dispatch records one placed on a runner: local
// placement, the runner's id, and a sandbox ref in the runner's namespace.
func runnerPlaced(run types.AgentRun) types.AgentRun {
	id := uuid.New()
	run.Placement, run.RunnerID = types.PlacementLocal, &id
	run.SandboxRef = placement.RunnerRefPrefix(id.String()) + "sbx"
	return run
}

// streamLog is the refs the org opened PTYs on.
type streamLog struct {
	holderTestRunner
	mu   sync.Mutex
	refs []string
}

func (r *streamLog) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	r.mu.Lock()
	r.refs = append(r.refs, ref)
	r.mu.Unlock()
	return r.holderTestRunner.Attach(ctx, ref, opts)
}

func (r *streamLog) opened() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refs...)
}

// entryRunnerServer is entryServer's cast on a runner-placed run.
func entryRunnerServer(t *testing.T, operatorOwned bool) (*Server, *streamLog, types.AgentRun, map[string]*http.Cookie) {
	t.Helper()
	srv, st, _, _, run := holderTestServer(t)
	run.CreatedBy, run.OperatorOwned = entryOwnerSub, operatorOwned
	run = runnerPlaced(run)
	st.mu.Lock()
	st.runs[run.ID] = run
	st.mu.Unlock()
	log := &streamLog{}
	srv.cfg.Runner = log
	return srv, log, run, map[string]*http.Cookie{
		"owner":    ssoSession(t, entryOwnerSub, entryOwnerMail, oidc.RoleUser),
		"admin":    ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin),
		"security": ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin),
	}
}

// TestEntryRunner_MintAndTakeover: the ticket mint and the take-over answer a
// runner-placed run exactly as a run placed anywhere else (TestEntry_Mint,
// TestAttachTakeover_OwnerOrSuperAdminOnly).
func TestEntryRunner_MintAndTakeover(t *testing.T) {
	for _, tc := range []struct {
		name     string
		actor    string
		personal bool
		want     int
	}{
		{"the owner", "owner", true, http.StatusOK},
		{"a super admin on a person's run", "admin", true, http.StatusForbidden},
		{"a super admin on an operator-owned run", "admin", false, http.StatusOK},
		{"the security tier on a person's run", "security", true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, log, run, ses := entryRunnerServer(t, !tc.personal)
			base := "/api/v1/runs/" + run.ID.String()
			mint := doSSO(t, srv, http.MethodPost, base+"/attach-ticket", ses[tc.actor], "")
			if mint.Code != tc.want {
				t.Fatalf("mint: %d %s, want %d", mint.Code, mint.Body.String(), tc.want)
			}
			if tc.want == http.StatusForbidden && !refusedOwnerOnly(mint.Code, mint.Body.String()) {
				t.Fatalf("mint refusal body: %s, want run_owner_only", mint.Body.String())
			}

			held := &attachHolder{principal: entryOwnerSub, source: attachSourceWeb, since: time.Now(), displace: func(string) {}}
			if readOnly, release := srv.registerAttachHolder(run.ID, held); readOnly {
				t.Fatal("could not seed the holder")
			} else {
				defer release()
			}
			take := doSSO(t, srv, http.MethodPost, base+"/attach/takeover", ses[tc.actor], "")
			if take.Code != tc.want {
				t.Fatalf("take-over: %d %s, want %d", take.Code, take.Body.String(), tc.want)
			}
			if got := held.evicted.Load(); got != (take.Code == http.StatusOK) {
				t.Fatalf("holder evicted = %v after a %d take-over", got, take.Code)
			}
			if opened := log.opened(); len(opened) != 0 {
				t.Fatalf("the org opened %v on the runner before any attach", opened)
			}
		})
	}
}

func attachDial(t *testing.T, ts *httptest.Server, run types.AgentRun, principal, role string, srv *Server) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	tok, err := mintAttachTicket(context.Background(), srv.cfg.Store, run.ID, types.ActorHuman, principal, role, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, nil)
	if c != nil {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, resp, err
}

// TestEntryRunner_AttachTicketLane: a ticket for a runner-placed run opens the
// runner's PTY only for the owner (or an admin stamp on an operator-owned run),
// and a refused ticket leaves the runner with no stream.
func TestEntryRunner_AttachTicketLane(t *testing.T) {
	srv, log, run, _ := entryRunnerServer(t, false)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	if _, resp, err := attachDial(t, ts, run, "root", oidc.RoleAdmin, srv); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an admin ticket on a person's run: err=%v resp=%v, want the 403", err, resp)
	}
	if _, resp, err := attachDial(t, ts, run, "mallory", oidc.RoleUser, srv); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a member's ticket on another person's run: err=%v resp=%v, want the 403", err, resp)
	}
	if opened := log.opened(); len(opened) != 0 {
		t.Fatalf("refused tickets still opened %v on the runner", opened)
	}

	c, _, err := attachDial(t, ts, run, entryOwnerSub, oidc.RoleUser, srv)
	if err != nil {
		t.Fatalf("the owner's ticket: %v", err)
	}
	readAttachMode(t, c)
	waitFor(t, "the owner's attach to reach the runner", func() bool { return len(log.opened()) > 0 })
	if opened := log.opened(); len(opened) != 1 || opened[0] != run.SandboxRef {
		t.Fatalf("the owner's attach opened %v, want exactly the run's runner ref %q", opened, run.SandboxRef)
	}

	srv2, log2, run2, _ := entryRunnerServer(t, true)
	ts2 := httptest.NewServer(panicFails(t, srv2.Handler()))
	defer ts2.Close()
	c2, _, err := attachDial(t, ts2, run2, "root", oidc.RoleAdmin, srv2)
	if err != nil {
		t.Fatalf("an admin ticket on an operator-owned run: %v", err)
	}
	readAttachMode(t, c2)
	waitFor(t, "the operator-owned attach to reach the runner", func() bool { return len(log2.opened()) > 0 })
	if opened := log2.opened(); len(opened) != 1 || opened[0] != run2.SandboxRef {
		t.Fatalf("an operator-owned attach opened %v, want %q", opened, run2.SandboxRef)
	}
}

// TestEntryRunner_CookieLane: the cookie attach refuses a super admin on a
// person's run before the runner is touched, and passes the owner-admin and
// the operator-owned case to the upgrade.
func TestEntryRunner_CookieLane(t *testing.T) {
	srv, log, run, ses := entryRunnerServer(t, false)
	path := "/api/v1/runs/" + run.ID.String() + "/attach"
	if w := doSSO(t, srv, http.MethodGet, path, ses["admin"], ""); !refusedOwnerOnly(w.Code, w.Body.String()) {
		t.Fatalf("a super admin's cookie: %d %s, want the 403 run_owner_only body", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodGet, path, adminToken, ""); !refusedOwnerOnly(w.Code, w.Body.String()) {
		t.Fatalf("the admin token: %d %s, want the 403 run_owner_only body", w.Code, w.Body.String())
	}
	own := ssoSession(t, entryOwnerSub, entryOwnerMail, oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodGet, path, own, ""); w.Code != http.StatusUpgradeRequired {
		t.Fatalf("an admin on their own run: %d %s, want 426", w.Code, w.Body.String())
	}
	srv2, _, run2, ses2 := entryRunnerServer(t, true)
	if w := doSSO(t, srv2, http.MethodGet, "/api/v1/runs/"+run2.ID.String()+"/attach", ses2["admin"], ""); w.Code != http.StatusUpgradeRequired {
		t.Fatalf("an admin on an operator-owned run: %d %s, want 426", w.Code, w.Body.String())
	}
	if opened := log.opened(); len(opened) != 0 {
		t.Fatalf("the cookie lane opened %v on the runner", opened)
	}
}

// uiRunnerHarness is the UI harness on a runner-placed run, counting the execs
// the org opens toward it.
func uiRunnerHarness(t *testing.T) (*uiHarness, *atomic.Int32) {
	t.Helper()
	h := newUIHarness(t, closingBackend("sandbox app"))
	h.run = runnerPlaced(h.run)
	h.store.putRun(h.run)
	var opens atomic.Int32
	inner := h.runner.execFn
	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		opens.Add(1)
		return inner(spec)
	}
	return h, &opens
}

// TestEntryRunner_UIGateway: the gateway's ticket redemption and its relay
// re-assert apply the entry rule to a runner-placed run, and a refusal opens
// nothing on the runner.
func TestEntryRunner_UIGateway(t *testing.T) {
	h, opens := uiRunnerHarness(t)
	if rec := h.enterWith(h.ticket(h.run.ID, "root", oidc.RoleAdmin)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "run_owner_only") {
		t.Fatalf("an admin ticket on a person's run: %d %s, want the 403 run_owner_only", rec.Code, rec.Body.String())
	}
	if rec := h.enterWith(h.ticket(h.run.ID, "mallory", oidc.RoleUser)); rec.Code != http.StatusForbidden {
		t.Fatalf("a member's ticket: %d %s, want 403", rec.Code, rec.Body.String())
	}
	if n := opens.Load(); n != 0 {
		t.Fatalf("refused redemptions opened %d execs on the runner", n)
	}
	if rec := h.enterWith(h.ticket(h.run.ID, h.owner, oidc.RoleUser)); rec.Code != http.StatusFound {
		t.Fatalf("the owner's ticket: %d %s, want the 302 hand-off", rec.Code, rec.Body.String())
	}

	now := h.clock.now()
	cookie := &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{
		Run: h.run.ID, App: "code", Port: uiTestPort, Principal: "root", Role: oidc.RoleAdmin,
		Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix(),
	})}
	before := opens.Load()
	path := uiRelayPrefix(h.run.ID, "code") + "/"
	if r := uiGet(h, path, cookie); r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), uiSessionNoLongerAuthorizedMsg) {
		t.Fatalf("an admin relay cookie on a person's run: %d %s, want the not-authorized 403", r.Code, r.Body.String())
	}
	if opens.Load() != before {
		t.Fatal("a refused relay request opened an exec on the runner")
	}
	owned := h.run
	owned.OperatorOwned = true
	h.store.putRun(owned)
	if r := uiGet(h, path, cookie); r.Code != http.StatusOK {
		t.Fatalf("an admin relay cookie on an operator-owned run: %d %s, want 200", r.Code, r.Body.String())
	}
}

// TestEntryRunner_SSHGateway: an admin key is refused at connect on a
// runner-placed person's run and opens no exec there; the owner's key reaches
// the runner; an admin key on an operator-owned run is admitted.
func TestEntryRunner_SSHGateway(t *testing.T) {
	st := newSSHMemStore()
	person := runnerPlaced(types.AgentRun{ID: uuid.New(), CreatedBy: "alice-sub", State: types.RunRunning})
	svc := runnerPlaced(types.AgentRun{ID: uuid.New(), CreatedBy: "admin-token", OperatorOwned: true, State: types.RunRunning})
	st.putRun(person)
	st.putRun(svc)
	now := time.Now()
	ownerPriv, ownerPub := mustSSHKeypair(t)
	adminPriv, adminPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(ownerPub), Principal: "alice-sub", PublicKey: string(ssh.MarshalAuthorizedKey(ownerPub))})
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(adminPub), Principal: "root-sub", CreatedAt: now,
		PublicKey: string(ssh.MarshalAuthorizedKey(adminPub)), Role: oidc.RoleAdmin, RoleCheckedAt: &now})
	var execs atomic.Int32
	h := newSSHTestHarness(t, st, &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		execs.Add(1)
		return fakeExecSession("ok\n", "", 0), nil
	}})

	if c, err := sshDial(t, h, person.ID.String(), adminPriv); err == nil {
		_ = c.Close()
		t.Fatal("an admin key was admitted to a runner-placed person's run")
	}
	if n := execs.Load(); n != 0 {
		t.Fatalf("a refused key opened %d execs on the runner", n)
	}
	for _, tc := range []struct {
		name string
		run  types.AgentRun
		key  ed25519.PrivateKey
	}{
		{"the owner's key", person, ownerPriv},
		{"an admin key on an operator-owned run", svc, adminPriv},
	} {
		client, err := sshDial(t, h, tc.run.ID.String(), tc.key)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("%s: new session: %v", tc.name, err)
		}
		if out, err := sess.Output("true"); err != nil || string(out) != "ok\n" {
			t.Fatalf("%s: exec = %q, %v", tc.name, out, err)
		}
		_ = sess.Close()
		_ = client.Close()
	}
	if n := execs.Load(); n != 2 {
		t.Fatalf("admitted keys opened %d execs, want 2", n)
	}
}
